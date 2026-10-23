// context-storage-bench measures the real SQLiteRetriever and HNSWRetriever
// paths using a synthetic deterministic provider; it never contacts a service.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
	"recordingmcp/internal/semantic"
)

type provider struct{ vectors map[string]semantic.Vector }

func (p *provider) ID() string    { return "synthetic-bench-v1" }
func (p *provider) Model() string { return "seeded-random-v1" }
func (p *provider) Dims() int     { return 64 }
func (p *provider) Embed(ctx context.Context, texts []string) ([]semantic.Vector, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]semantic.Vector, len(texts))
	for i, s := range texts {
		v, ok := p.vectors[s]
		if !ok {
			return nil, fmt.Errorf("unknown synthetic query %q", s)
		}
		out[i] = v
	}
	return out, nil
}

type sample struct {
	Scenario           string   `json:"scenario"`
	Repeat             int      `json:"repeat"`
	Items              int      `json:"items"`
	Queries            int      `json:"queries"`
	ANNQueries         int      `json:"ann_queries"`
	ExactP50MS         float64  `json:"exact_p50_ms"`
	ExactP95MS         float64  `json:"exact_p95_ms"`
	HNSWP50MS          float64  `json:"hnsw_p50_ms"`
	HNSWP95MS          float64  `json:"hnsw_p95_ms"`
	Recall10           float64  `json:"recall_at_10"`
	Recall40           float64  `json:"recall_at_40"`
	Fallback           bool     `json:"fallback_observed"`
	FallbackQueries    int      `json:"fallback_queries"`
	FallbackDiagnostic string   `json:"fallback_diagnostic"`
	SetupMS            float64  `json:"setup_ms"`
	SidecarBuildMS     float64  `json:"sidecar_build_ms"`
	SidecarLoadMS      float64  `json:"sidecar_load_ms"`
	ExactColdMS        float64  `json:"exact_cold_ms"`
	HNSWColdMS         float64  `json:"hnsw_cold_ms"`
	SafetyLeaks        int      `json:"safety_leaks"`
	FailedGates        []string `json:"failed_gates,omitempty"`
	RSSMB              float64  `json:"rss_mb"`
}
type report struct {
	Version  string   `json:"benchmark_version"`
	Seed     int64    `json:"seed"`
	Dims     int      `json:"dims"`
	Provider string   `json:"provider"`
	Note     string   `json:"semantic_claims"`
	Samples  []sample `json:"samples"`
}

func main() {
	sizesFlag := flag.String("sizes", "10000,100000", "item counts")
	reps := flag.Int("repeats", 3, "repeats per size and scenario")
	queries := flag.Int("queries", 20, "queries per repeat and scenario")
	seed := flag.Int64("seed", 730241, "fixed random seed")
	output := flag.String("output", "", "optional JSON file")
	flag.Parse()
	sizes, e := ints(*sizesFlag)
	if e != nil {
		fatal(e)
	}
	if *reps != 3 {
		fatal(fmt.Errorf("repeats must be exactly 3 for comparable report"))
	}
	if *queries < 1 {
		fatal(fmt.Errorf("queries must be positive"))
	}
	r := report{Version: "1", Seed: *seed, Dims: 64, Provider: "synthetic deterministic vectors (no external calls)", Note: "Synthetic random vectors benchmark storage/retrieval mechanics only; they make no semantic-quality claim."}
	for _, n := range sizes {
		for rep := 1; rep <= *reps; rep++ {
			rows, err := run(n, rep, *queries, *seed)
			if err != nil {
				fatal(err)
			}
			r.Samples = append(r.Samples, rows...)
		}
	}
	var b []byte
	b, e = json.MarshalIndent(r, "", "  ")
	if e != nil {
		fatal(e)
	}
	b = append(b, '\n')
	if *output != "" {
		if e = os.WriteFile(*output, b, 0600); e != nil {
			fatal(e)
		}
	} else {
		_, _ = os.Stdout.Write(b)
	}
}
func run(n, repeat, qcount int, seed int64) ([]sample, error) {
	started := time.Now()
	dir, e := os.MkdirTemp("", "context-storage-bench-")
	if e != nil {
		return nil, e
	}
	defer os.RemoveAll(dir)
	db, e := sql.Open("sqlite", filepath.Join(dir, "bench.db"))
	if e != nil {
		return nil, e
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, e = db.Exec(schema); e != nil {
		return nil, e
	}
	rng := rand.New(rand.NewSource(seed + int64(n)*31 + int64(repeat)))
	p := &provider{vectors: map[string]semantic.Vector{}}
	tx, e := db.Begin()
	if e != nil {
		return nil, e
	}
	ins, e := tx.Prepare(`INSERT INTO turns(id,conversation_id,content,source_sha256,transcript_version,original_turn_id,recording_id) VALUES(?,?,?,?,?,?,?)`)
	if e != nil {
		return nil, e
	}
	ei, e := tx.Prepare(`INSERT INTO memory_embeddings(kind,artifact_id,conversation_id,content_sha256,provider_id,model_id,dims,vector,created_at) VALUES('turn',?,?,?,?,?,?,?,0)`)
	if e != nil {
		return nil, e
	}
	for i := 0; i < n; i++ {
		v := randomVector(rng, 64)
		id := fmt.Sprintf("t%07d", i)
		conv := "conv:other"
		if i%10 == 0 {
			conv = "conv:target"
		}
		content := fmt.Sprintf("synthetic row %d", i)
		hash := semanticHash(content)
		_, e = ins.Exec(id, conv, content, strings.Repeat("a", 64), "v1", id, "rec")
		if e != nil {
			return nil, e
		}
		_, e = ei.Exec(id, conv, hash, p.ID(), p.Model(), p.Dims(), semantic.Encode(v))
		if e != nil {
			return nil, e
		}
		if i%997 == 0 {
			if _, e = tx.Exec(`INSERT INTO turn_classes(turn_id,class) VALUES(?,?)`, id, "ambient"); e != nil {
				return nil, e
			}
		}
		if i%991 == 0 {
			if _, e = tx.Exec(`INSERT INTO source_attachments(id,active) VALUES(?,0)`, "a"+id); e != nil {
				return nil, e
			}
			if _, e = tx.Exec(`INSERT INTO attachment_turns(attachment_id,turn_id) VALUES(?,?)`, "a"+id, id); e != nil {
				return nil, e
			}
		}
		if i%733 == 0 {
			p.vectors[fmt.Sprintf("q%d", i)] = v
		}
	}
	if e = ins.Close(); e != nil {
		return nil, e
	}
	if e = ei.Close(); e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	if e = db.Ping(); e != nil {
		return nil, e
	}
	setupMS := float64(time.Since(started).Microseconds()) / 1000
	side := filepath.Join(dir, "index.vec")
	ctx := context.Background()
	buildStart := time.Now()
	if e = semantic.BuildHNSWSidecar(ctx, db, p, side); e != nil {
		return nil, e
	}
	buildMS := float64(time.Since(buildStart).Microseconds()) / 1000
	loadStart := time.Now()
	ann, e := semantic.OpenHNSWRetriever(ctx, db, p, side)
	loadMS := float64(time.Since(loadStart).Microseconds()) / 1000
	exact := &semantic.SQLiteRetriever{DB: db, Provider: p}
	if !ann.HasIndex() {
		return nil, fmt.Errorf("sidecar missing for n=%d repeat=%d", n, repeat)
	}
	scenarios := []struct {
		name     string
		opts     semantic.SearchOptions
		eligible func(semantic.SemanticHit) bool
	}{{"unscoped-current", semantic.SearchOptions{Limit: 40}, func(h semantic.SemanticHit) bool {
		return h.ConversationID == "conv:other" || h.ConversationID == "conv:target"
	}}, {"conversation-scoped", semantic.SearchOptions{ConversationID: "conv:target", Limit: 40}, func(h semantic.SemanticHit) bool { return h.ConversationID == "conv:target" }}, {"stale-filter", semantic.SearchOptions{Limit: 40}, func(h semantic.SemanticHit) bool { return !isStale(h.ArtifactID) }}, {"ambient-filter", semantic.SearchOptions{Limit: 40}, func(h semantic.SemanticHit) bool { return !isAmbient(h.ArtifactID) }}}
	results := make([]sample, 0, len(scenarios))
	keys := make([]string, 0, len(p.vectors))
	for k := range p.vectors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return nil, fmt.Errorf("no queries")
	}
	for _, sc := range scenarios {
		var et, at []float64
		var r10, r40 float64
		fallbackQueries, leaks := 0, 0
		annQueries := 0
		fallbackReasons := map[string]bool{}
		var exactCold, annCold float64
		for qi := 0; qi < qcount; qi++ {
			key := keys[(qi*len(keys)/qcount)%len(keys)]
			opts := sc.opts
			if sc.name == "stale-filter" {
				opts.Kinds = []string{"turn"}
			}
			t := time.Now()
			ex, err := exact.Search(ctx, key, opts)
			if qi == 0 {
				exactCold = float64(time.Since(t).Microseconds()) / 1000
			}
			et = append(et, float64(time.Since(t).Microseconds())/1000)
			if err != nil {
				return nil, err
			}
			t = time.Now()
			got, diag, err := ann.SearchWithDiagnostics(ctx, key, opts)
			elapsed := float64(time.Since(t).Microseconds()) / 1000
			if qi == 0 && annCold == 0 && !diag.Fallback {
				annCold = elapsed
			}
			if err != nil {
				return nil, err
			}
			if diag.Fallback {
				fallbackQueries++
				fallbackReasons[diag.FallbackReason] = true
			} else {
				annQueries++
				at = append(at, elapsed)
				if annCold == 0 {
					annCold = elapsed
				}
				r10 += recall(ex, got, 10)
				r40 += recall(ex, got, 40)
			}
			for _, h := range got {
				if isStale(h.ArtifactID) || isAmbient(h.ArtifactID) || !sc.eligible(h) {
					leaks++
				}
				if sc.name == "conversation-scoped" && h.ConversationID != "conv:target" {
					leaks++
				}
			}
		}
		failed := make([]string, 0)
		if fallbackQueries != 0 {
			failed = append(failed, "ann-fallback")
		}
		if leaks != 0 {
			failed = append(failed, "safety-leak")
		}
		reasons := make([]string, 0, len(fallbackReasons))
		for reason := range fallbackReasons {
			reasons = append(reasons, reason)
		}
		sort.Strings(reasons)
		results = append(results, sample{Scenario: sc.name, Repeat: repeat, Items: n, Queries: qcount, ANNQueries: annQueries, ExactP50MS: percentile(et, .5), ExactP95MS: percentile(et, .95), HNSWP50MS: percentile(at, .5), HNSWP95MS: percentile(at, .95), Recall10: mean(r10, annQueries), Recall40: mean(r40, annQueries), Fallback: fallbackQueries > 0, FallbackQueries: fallbackQueries, FallbackDiagnostic: strings.Join(reasons, ","), SetupMS: setupMS, SidecarBuildMS: buildMS, SidecarLoadMS: loadMS, ExactColdMS: exactCold, HNSWColdMS: annCold, SafetyLeaks: leaks, FailedGates: failed, RSSMB: rssMB()})
	}
	return results, nil
}

const schema = `PRAGMA journal_mode=OFF; CREATE TABLE turns(id TEXT PRIMARY KEY,conversation_id TEXT,content TEXT,source_sha256 TEXT,transcript_version TEXT,original_turn_id TEXT,recording_id TEXT); CREATE TABLE memory_embeddings(kind TEXT,artifact_id TEXT,conversation_id TEXT,content_sha256 TEXT,provider_id TEXT,model_id TEXT,dims INTEGER,vector BLOB,created_at INTEGER); CREATE INDEX emb_config ON memory_embeddings(provider_id,model_id,dims); CREATE TABLE turn_classes(turn_id TEXT PRIMARY KEY,class TEXT); CREATE TABLE source_attachments(id TEXT PRIMARY KEY,active INTEGER); CREATE TABLE attachment_turns(attachment_id TEXT,turn_id TEXT); CREATE TABLE derived_artifacts(kind TEXT,artifact_id TEXT,stale INTEGER); CREATE TABLE notes(id TEXT,summary TEXT); CREATE TABLE note_evidence(note_id TEXT,turn_id TEXT,ordinal INTEGER);`

func randomVector(r *rand.Rand, d int) semantic.Vector {
	v := make(semantic.Vector, d)
	var s float64
	for i := range v {
		x := float32(r.NormFloat64())
		v[i] = x
		s += float64(x * x)
	}
	scale := float32(1 / sqrt(s))
	for i := range v {
		v[i] *= scale
	}
	return v
}
func sqrt(x float64) float64 {
	z := x
	for i := 0; i < 8; i++ {
		z = (z + x/z) / 2
	}
	return z
}
func semanticHash(s string) string { // all synthetic content is current until explicitly stale via attachments
	_ = s
	return hashHex(s)
}
func hashHex(s string) string { h := sha256Sum([]byte(s)); return fmt.Sprintf("%x", h) }
func isStale(id string) bool {
	var i int
	_, e := fmt.Sscanf(strings.TrimPrefix(id, "t"), "%d", &i)
	return e == nil && i%991 == 0
}
func isAmbient(id string) bool {
	var i int
	_, e := fmt.Sscanf(strings.TrimPrefix(id, "t"), "%d", &i)
	return e == nil && i%997 == 0
}
func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }
func recall(ex []semantic.SemanticHit, got []semantic.SemanticHit, k int) float64 {
	if len(ex) > k {
		ex = ex[:k]
	}
	if len(ex) == 0 {
		return 1
	}
	m := map[string]bool{}
	for _, h := range ex {
		m[h.ArtifactID] = true
	}
	n := 0
	for _, h := range got {
		if m[h.ArtifactID] {
			n++
		}
	}
	return float64(n) / float64(len(ex))
}
func mean(total float64, count int) float64 {
	if count == 0 {
		return 0
	}
	return total / float64(count)
}

func percentile(xs []float64, p float64) float64 {
	sort.Float64s(xs)
	if len(xs) == 0 {
		return 0
	}
	return xs[int(float64(len(xs)-1)*p)]
}
func ints(s string) ([]int, error) {
	var x []int
	for _, v := range strings.Split(s, ",") {
		var n int
		if _, e := fmt.Sscan(v, &n); e != nil || n <= 0 {
			return nil, fmt.Errorf("invalid size %q", v)
		}
		x = append(x, n)
	}
	return x, nil
}
func rssMB() float64 {
	b, e := os.ReadFile("/proc/self/status")
	if e != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			var kb float64
			_, _ = fmt.Sscanf(l, "VmRSS: %f", &kb)
			return kb / 1024
		}
	}
	return 0
}
func fatal(e error) { fmt.Fprintln(os.Stderr, e); os.Exit(2) }
