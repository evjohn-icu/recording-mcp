// Command turnclass-refill classifies transcript turns as speech or ambient
// sound for a database whose rows predate classification. Ingest classifies
// new turns with the deterministic rules in internal/turnclass; this tool
// covers what the rules cannot decide (lyrics, fiction narration, media
// playback) by batching the remaining turns to an OpenAI-compatible chat
// endpoint, and can also be re-run to refine older model rulings.
//
// It writes only turn_classes rows: first classification wins for a turn, so
// a re-run never downgrades an ingest-time rule ruling. Missing rows always
// mean "not classified yet", never "ambient".
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"recordingmcp/internal/migrations"
	"recordingmcp/internal/turnclass"

	_ "modernc.org/sqlite"
)

func main() {
	dbPath := flag.String("db", "data/context-memory.db", "SQLite memory database")
	url := flag.String("model-url", "", "OpenAI-compatible chat completions URL (empty runs the rules pass only)")
	model := flag.String("model", "", "model id sent to the endpoint")
	keyEnv := flag.String("key-env", "", "environment variable holding the API key")
	batchSize := flag.Int("batch", 80, "turns per model request")
	convPass := flag.Bool("conversation-pass", false, "rule on whole conversations (media vs conversation) and mark media recordings' turns ambient; skips the per-turn model pass")
	flag.Parse()

	ctx := context.Background()
	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := migrations.Apply(ctx, db); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	rules, remaining, err := rulesPass(ctx, db)
	if err != nil {
		log.Fatalf("rules pass: %v", err)
	}
	log.Printf("rules pass: %d ambient, %d unclassified", rules, remaining)

	if *url == "" || *model == "" {
		return
	}
	key := os.Getenv(*keyEnv)
	if key == "" {
		log.Fatalf("environment variable %s is empty", *keyEnv)
	}
	if *convPass {
		media, err := conversationPass(ctx, db, *url, *model, key)
		if err != nil {
			log.Fatalf("conversation pass: %v", err)
		}
		log.Printf("conversation pass: %d media conversations marked ambient", media)
		return
	}
	modelled, err := modelPass(ctx, db, *url, *model, key, *batchSize)
	if err != nil {
		log.Fatalf("model pass: %v", err)
	}
	log.Printf("model pass: %d turns ruled", modelled)
}

// pending is one turn awaiting classification.
type pending struct{ id, content string }

// rulesPass applies the deterministic ambient rules to every unclassified
// turn and reports how many became ambient and how many remain undecided.
func rulesPass(ctx context.Context, db *sql.DB) (ambient, remaining int, err error) {
	rows, err := db.QueryContext(ctx, `SELECT t.id,t.content FROM turns t
 WHERE NOT EXISTS(SELECT 1 FROM turn_classes tc WHERE tc.turn_id=t.id)`)
	if err != nil {
		return 0, 0, err
	}
	var undecided []pending
	var matches []pending
	now := time.Now().UnixMilli()
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.content); err != nil {
			rows.Close()
			return 0, 0, err
		}
		if _, _, matched := turnclass.Classify(p.content); matched {
			matches = append(matches, p)
			continue
		}
		undecided = append(undecided, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, err
	}
	rows.Close()

	// Writes happen only after the read cursor is closed: the store is a
	// single connection and an Exec while rows are open would deadlock.
	for _, p := range matches {
		class, subtype, _ := turnclass.Classify(p.content)
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO turn_classes(turn_id,class,subtype,method,created_at) VALUES(?,?,?,?,?)`,
			p.id, class, subtype, turnclass.MethodRule, now); err != nil {
			return 0, 0, err
		}
		if class == turnclass.ClassAmbient {
			ambient++
		}
	}
	// The undecided list is the model pass input; persist nothing for them
	// here so a rules-only run leaves the honest "not classified yet" state.
	return ambient, len(undecided), nil
}

// ruling is the wire shape of one model ruling; the JSON tags match the
// classify prompt's output schema.
type ruling struct {
	Index   int    `json:"index"`
	Class   string `json:"class"`
	Subtype string `json:"subtype"`
}

// salvageRulings extracts every balanced JSON object that parses as a
// ruling. Some models wrap valid ruling objects in malformed container
// syntax (missing commas, sibling junk); a strict parse of the container
// would fail the whole chunk, so each object is unmarshalled independently.
func salvageRulings(s string) []ruling {
	var out []ruling
	rest := s
	for {
		obj := firstJSONObject(rest)
		if obj == "" {
			return out
		}
		var r ruling
		if err := json.Unmarshal([]byte(obj), &r); err == nil &&
			(r.Class == turnclass.ClassSpeech || r.Class == turnclass.ClassAmbient) {
			out = append(out, r)
		}
		rest = rest[len(obj):]
	}
}

var validSubtype = map[string]bool{
	"":                                true,
	turnclass.SubtypeMusicLyrics:      true,
	turnclass.SubtypeMediaPlayback:    true,
	turnclass.SubtypeVoiceAssistant:   true,
	turnclass.SubtypeTransitBroadcast: true,
	turnclass.SubtypeKioskPrompt:      true,
}

const classifySystemPrompt = `你是转写轮次分类器。对每个轮次判断它是真人对话（speech）还是环境音（ambient）。环境音包括：语音助手指令、交通/商超广播、音乐歌词、影视/直播/有声剧等媒体播放、纯设备提示音。真人之间的对话、对设备的口头讨论、即使提到广播或空调也属于 speech。只输出严格 JSON 对象：{"rulings":[{"index":0,"class":"speech","subtype":""},...]}，subtype 对 ambient 可取 music_lyrics/media_playback/voice_assistant/transit_broadcast/kiosk_prompt，speech 留空。`

// modelPass batches undecided turns to the endpoint and stores its rulings
// with method=model. The model only sees turn text with stable indexes; the
// ids stay in this process so a hallucinated id can never reach the database.
func modelPass(ctx context.Context, db *sql.DB, url, model, key string, batchSize int) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT t.id,t.content FROM turns t
 WHERE NOT EXISTS(SELECT 1 FROM turn_classes tc WHERE tc.turn_id=t.id) ORDER BY t.id`)
	if err != nil {
		return 0, err
	}
	var undecided []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.content); err != nil {
			rows.Close()
			return 0, err
		}
		undecided = append(undecided, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	client := &http.Client{Timeout: 120 * time.Second}
	ruled := 0
	for start := 0; start < len(undecided); start += batchSize {
		end := min(start+batchSize, len(undecided))
		chunk := undecided[start:end]
		var count int
		var err error
		for attempt := 0; attempt < 4; attempt++ {
			count, err = classifyChunk(ctx, db, client, url, model, key, chunk)
			if err == nil {
				break
			}
			log.Printf("chunk at %d attempt %d failed: %v", start, attempt+1, err)
			time.Sleep(3 * time.Duration(attempt+1) * time.Second)
		}
		if err != nil {
			// One bad chunk must not abandon the run: those turns stay
			// honestly unclassified and a re-run picks them up.
			log.Printf("chunk at %d skipped after retries: %v", start, err)
			continue
		}
		ruled += count
		log.Printf("model pass: %d/%d turns ruled", ruled, len(undecided))
	}
	return ruled, nil
}

// chatCompletion sends one system+user exchange and returns the message
// content. Transient retries are the caller's concern.
func chatCompletion(ctx context.Context, client *http.Client, url, model, key, system, user string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"temperature": 0, "max_tokens": 4096,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("endpoint returned %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", err
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("endpoint returned no choices: %s", truncate(string(raw), 200))
	}
	return parsed.Choices[0].Message.Content, nil
}

const conversationClassPrompt = `你是转写内容分类器。给出一段录音转写的抽样，判断整段录音的性质：
- "media"：影视剧、有声剧/有声书、游戏直播、歌曲演唱等媒体内容为主，其中的"对话"是作品或播主内容，不是真实人际沟通
- "conversation"：真实人际对话（面谈/电话/会议），或个人口述记录
- "mixed"：真实对话与媒体内容混杂，需逐轮区分
只输出严格 JSON：{"class":"media"} 或 {"class":"conversation"} 或 {"class":"mixed"}`

// conversationPass rules on each conversation as a whole. Per-turn judgement
// lacks context, so audiobook and livestream dialogue reads as speech; a
// conversation-level "media" ruling marks all of its turns ambient
// (subtype media_playback), overriding earlier per-turn speech rows because
// the context-free ruling is strictly less informed.
func conversationPass(ctx context.Context, db *sql.DB, url, model, key string) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT conversation_id,id,content FROM turns ORDER BY conversation_id,segment_index,id`)
	if err != nil {
		return 0, err
	}
	type turn struct{ id, content string }
	byConv := map[string][]turn{}
	for rows.Next() {
		var conv string
		var t turn
		if err := rows.Scan(&conv, &t.id, &t.content); err != nil {
			rows.Close()
			return 0, err
		}
		byConv[conv] = append(byConv[conv], t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	client := &http.Client{Timeout: 120 * time.Second}
	now := time.Now().UnixMilli()
	mediaConvs := 0
	for conv, turns := range byConv {
		// Sample up to 16 turns evenly across the recording; a whole-media
		// recording reads as media from any honest sample.
		step := max(1, len(turns)/16)
		lines := []string{}
		for i := 0; i < len(turns) && len(lines) < 16; i += step {
			text := turns[i].content
			if len([]rune(text)) > 80 {
				text = string([]rune(text)[:80])
			}
			lines = append(lines, text)
		}
		content, err := chatCompletion(ctx, client, url, model, key, conversationClassPrompt, strings.Join(lines, "\n"))
		if err != nil {
			log.Printf("conversation %s skipped: %v", conv, err)
			continue
		}
		m := firstJSONObject(content)
		if m == "" {
			log.Printf("conversation %s skipped: no JSON in %s", conv, truncate(content, 80))
			continue
		}
		var out struct {
			Class string `json:"class"`
		}
		if err := json.Unmarshal([]byte(m), &out); err != nil {
			log.Printf("conversation %s skipped: %v", conv, err)
			continue
		}
		if out.Class != "media" {
			continue
		}
		for _, t := range turns {
			if _, err := db.ExecContext(ctx,
				`INSERT OR REPLACE INTO turn_classes(turn_id,class,subtype,method,created_at) VALUES(?,?,?,?,?)`,
				t.id, turnclass.ClassAmbient, turnclass.SubtypeMediaPlayback, turnclass.MethodModel, now); err != nil {
				return mediaConvs, err
			}
		}
		mediaConvs++
		log.Printf("conversation %s ruled media: %d turns marked ambient", conv, len(turns))
	}
	return mediaConvs, nil
}

func classifyChunk(ctx context.Context, db *sql.DB, client *http.Client, url, model, key string, chunk []pending) (int, error) {
	lines := make([]string, len(chunk))
	for i, p := range chunk {
		text := p.content
		if len([]rune(text)) > 200 {
			text = string([]rune(text)[:200])
		}
		lines[i] = fmt.Sprintf("[%d] %s", i, text)
	}
	content, err := chatCompletion(ctx, client, url, model, key, classifySystemPrompt, strings.Join(lines, "\n"))
	if err != nil {
		return 0, err
	}
	m := firstJSONObject(content)
	if m == "" {
		return 0, fmt.Errorf("no JSON in model output: %s", truncate(content, 200))
	}
	var out struct {
		Rulings []ruling `json:"rulings"`
	}
	if err := json.Unmarshal([]byte(m), &out); err != nil || len(out.Rulings) == 0 {
		// Some models wrap valid ruling objects in malformed container
		// syntax; salvage what parses instead of failing the chunk.
		out.Rulings = salvageRulings(m)
		if len(out.Rulings) == 0 {
			return 0, fmt.Errorf("parse model output: %v: %s", err, truncate(m, 200))
		}
	}

	now := time.Now().UnixMilli()
	seen := make(map[int]bool, len(out.Rulings))
	ruled := 0
	for _, r := range out.Rulings {
		if r.Index < 0 || r.Index >= len(chunk) || seen[r.Index] {
			continue
		}
		seen[r.Index] = true
		class := turnclass.ClassSpeech
		if r.Class == turnclass.ClassAmbient {
			class = turnclass.ClassAmbient
		}
		subtype := r.Subtype
		if class == turnclass.ClassSpeech || !validSubtype[subtype] {
			subtype = ""
		}
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO turn_classes(turn_id,class,subtype,method,created_at) VALUES(?,?,?,?,?)`,
			chunk[r.Index].id, class, subtype, turnclass.MethodModel, now); err != nil {
			return ruled, err
		}
		ruled++
	}
	return ruled, nil
}

// firstJSONObject returns the first complete top-level JSON object in s. A
// greedy regex would span two sibling objects and produce malformed JSON, so
// the scan tracks string literals and escape sequences to find the balanced
// closing brace of the first object.
func firstJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth, inString, escaped := 0, false, false
	for i := start; i < len(s); i++ {
		switch c := s[i]; {
		case escaped:
			escaped = false
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
