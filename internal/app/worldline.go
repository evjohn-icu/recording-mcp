package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"recordingmcp/internal/decision"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/plane"
)

// WorldlineService builds a deterministic, source-backed projection. It never
// calls a model or edits source notes; all human feedback is appended to the
// existing event plane.
type WorldlineService struct {
	Time           *TimeService
	DecisionEngine decision.Engine
	mu             sync.Mutex
}

type WorldlineQuery struct {
	Query  string
	From   *time.Time
	To     *time.Time
	Status string
	Offset int
	Limit  int
}

type Worldline struct {
	Version string           `json:"version"`
	Topics  []WorldlineTopic `json:"topics"`
	Total   int              `json:"total"`
	Next    *int             `json:"next_offset,omitempty"`
	Stats   WorldlineStats   `json:"stats"`
	Range   WorldlineRange   `json:"range"`
}
type WorldlineRange struct {
	Start          *time.Time `json:"start,omitempty"`
	End            *time.Time `json:"end,omitempty"`
	ValidStart     *time.Time `json:"valid_start,omitempty"`
	ValidEnd       *time.Time `json:"valid_end,omitempty"`
	MentionedStart *time.Time `json:"mentioned_start,omitempty"`
	MentionedEnd   *time.Time `json:"mentioned_end,omitempty"`
}
type WorldlineStats struct {
	SourceNotes int `json:"source_notes"`
	Topics      int `json:"topics"`
	Claims      int `json:"claims"`
	DatedClaims int `json:"dated_claims"`
	UnknownTime int `json:"unknown_time"`
	Conflicts   int `json:"conflicts"`
}
type WorldlineTopic struct {
	ID               string          `json:"id"`
	Title            string          `json:"title"`
	SourceTopicIDs   []string        `json:"source_topic_ids"`
	SourceTopicCount int             `json:"source_topic_count"`
	HasMore          bool            `json:"has_more"`
	Lanes            []WorldlineLane `json:"lanes"`
}
type WorldlineLane struct {
	ID             string           `json:"id"`
	Title          string           `json:"title"`
	SourceTopicIDs []string         `json:"source_topic_ids"`
	Subject        string           `json:"subject"`
	Category       string           `json:"category"`
	Claims         []WorldlineClaim `json:"claims"`
}
type WorldlineClaim struct {
	ID               string              `json:"id"`
	Version          string              `json:"version"`
	Summary          string              `json:"summary"`
	Kind             string              `json:"kind"`
	Status           string              `json:"status"`
	ValidFrom        *time.Time          `json:"valid_from,omitempty"`
	ValidUntil       *time.Time          `json:"valid_until,omitempty"`
	MentionedAt      *time.Time          `json:"mentioned_at,omitempty"`
	TimeBasis        string              `json:"time_basis"`
	TimePrecision    string              `json:"time_precision"`
	ChangeType       string              `json:"change_type"`
	Evidence         []WorldlineEvidence `json:"evidence"`
	ConflictWith     []string            `json:"conflict_with"`
	TopicID          string              `json:"-"`
	SourceTopicID    string              `json:"-"`
	SourceTopicTitle string              `json:"-"`
	SubjectKey       string              `json:"-"`
	CategoryKey      string              `json:"-"`
	Metadata         map[string]any      `json:"-"`
}
type WorldlineEvidence struct {
	TurnID         string `json:"turn_id"`
	Revision       string `json:"revision"`
	ConversationID string `json:"conversation_id"`
	RecordingID    string `json:"recording_id"`
	Text           string `json:"text"`
	OffsetMS       *int64 `json:"offset_ms,omitempty"`
	SubjectID      string `json:"-"`
	ScopedSpeaker  string `json:"-"`
}
type WorldlineDetail struct {
	Version    string         `json:"version"`
	Topic      WorldlineTopic `json:"topic"`
	Stats      WorldlineStats `json:"stats"`
	Total      int            `json:"total"`
	NextOffset *int           `json:"next_offset,omitempty"`
}

type WorldlineFeedback struct {
	ExpectedVersion    string              `json:"expected_version"`
	Action             string              `json:"action"`
	TargetTopicID      string              `json:"target_topic_id,omitempty"`
	TargetClaimID      string              `json:"target_claim_id,omitempty"`
	DestinationTopicID string              `json:"destination_topic_id,omitempty"`
	Title              string              `json:"title,omitempty"`
	Summary            string              `json:"summary,omitempty"`
	ClaimIDs           []string            `json:"claim_ids,omitempty"`
	ValidFrom          *time.Time          `json:"valid_from,omitempty"`
	ValidUntil         *time.Time          `json:"valid_until,omitempty"`
	MentionedAt        *time.Time          `json:"mentioned_at,omitempty"`
	Evidence           []EvidenceReference `json:"evidence,omitempty"`
}

type worldlineSource struct {
	claim       WorldlineClaim
	topicIDs    []string
	topicTitles map[string]string
}

func (s *WorldlineService) List(ctx context.Context, scope string, q WorldlineQuery) (Worldline, error) {
	if s == nil || s.Time == nil || s.Time.Store == nil || s.Time.Events == nil {
		return Worldline{}, errors.New("worldline service unavailable")
	}
	if strings.TrimSpace(scope) == "" {
		return Worldline{}, errors.New("profile scope required")
	}
	if q.Status == "" {
		q.Status = "all"
	}
	if q.Status != "all" && q.Status != "unconfirmed" && q.Status != "reviewed" && q.Status != "conflict" {
		return Worldline{}, errors.New("status must be all, unconfirmed, reviewed, or conflict")
	}
	if q.Offset < 0 {
		return Worldline{}, errors.New("offset must be non-negative")
	}
	if q.Limit == 0 {
		q.Limit = 100
	}
	if q.Limit < 1 || q.Limit > 500 {
		return Worldline{}, errors.New("limit must be between 1 and 500")
	}
	if q.From != nil && q.To != nil && q.From.After(*q.To) {
		return Worldline{}, errors.New("from must not follow to")
	}
	topics, stats, version, err := s.project(ctx, scope)
	if err != nil {
		return Worldline{}, err
	}
	filtered := filterWorldline(topics, q)
	stats = worldlineFilteredStats(filtered)
	result := Worldline{Version: version, Topics: []WorldlineTopic{}, Stats: stats, Range: worldlineDataRange(topics)}
	result.Total = len(filtered)
	for i := range filtered {
		capTopicPreview(&filtered[i], 16, 24, 32)
	}
	if q.Offset >= len(filtered) {
		return result, nil
	}
	end := q.Offset + q.Limit
	if end > len(filtered) {
		end = len(filtered)
	}
	result.Topics = filtered[q.Offset:end]
	if end < len(filtered) {
		next := end
		result.Next = &next
	}
	return result, nil
}

func (s *WorldlineService) Topic(ctx context.Context, scope, topicID string, offset, limit int, filters ...WorldlineQuery) (WorldlineDetail, error) {
	if offset < 0 || limit < 1 || limit > 500 {
		return WorldlineDetail{}, errors.New("offset must be non-negative and limit must be between 1 and 500")
	}
	topics, stats, version, err := s.project(ctx, scope)
	if err != nil {
		return WorldlineDetail{}, err
	}
	q := WorldlineQuery{Status: "all"}
	if len(filters) > 0 {
		q = filters[0]
		if q.Status == "" {
			q.Status = "all"
		}
	}
	if q.Status != "all" && q.Status != "unconfirmed" && q.Status != "reviewed" && q.Status != "conflict" {
		return WorldlineDetail{}, errors.New("invalid status filter")
	}
	if q.From != nil && q.To != nil && q.From.After(*q.To) {
		return WorldlineDetail{}, errors.New("from must not follow to")
	}
	for _, topic := range topics {
		if topic.ID == topicID {
			filtered := filterWorldline([]WorldlineTopic{topic}, q)
			if len(filtered) == 0 {
				topic.Lanes = []WorldlineLane{}
			} else {
				topic = filtered[0]
			}
			total := topicClaimCount(topic)
			page := pageTopicClaims(topic, offset, limit)
			page.SourceTopicCount = topic.SourceTopicCount
			if len(page.SourceTopicIDs) > 32 {
				page.SourceTopicIDs = page.SourceTopicIDs[:32]
			}
			detail := WorldlineDetail{Version: version, Topic: page, Stats: stats, Total: total}
			if next := offset + limit; next < total {
				detail.NextOffset = &next
			}
			return detail, nil
		}
	}
	return WorldlineDetail{}, errors.New("topic not found")
}

func (s *WorldlineService) project(ctx context.Context, scope string) ([]WorldlineTopic, WorldlineStats, string, error) {
	sources, err := s.readSources(ctx)
	if err != nil {
		return nil, WorldlineStats{}, "", err
	}
	events, err := s.Time.History(ctx, scope)
	if err != nil {
		return nil, WorldlineStats{}, "", err
	}
	topics, err := buildWorldline(sources)
	if err != nil {
		return nil, WorldlineStats{}, "", err
	}
	version := worldlineVersion(sources, events)
	currentNotes := map[string]string{}
	for _, source := range sources {
		if id, ok := source.claim.Metadata["source_note_id"].(string); ok {
			currentNotes[id] = source.claim.Version
		}
	}
	for _, event := range events {
		if event.Type == "worldline.feedback" && event.SourceStatus == "current" && feedbackSourcesCurrent(event, currentNotes) {
			applyWorldlineFeedback(&topics, event)
		}
	}
	for _, event := range events {
		if (event.Type == "fact.observed" || event.Type == "topic.fact") && event.SourceStatus == "current" && event.StructuredData["purged"] != true {
			if claim, ok := s.factClaim(ctx, event, events, currentNotes); ok {
				appendEventClaim(&topics, claim)
			}
		}
	}
	finalizeWorldline(&topics)
	stats := worldlineStats(sources, topics)
	return topics, stats, version, nil
}

func (s *WorldlineService) readSources(ctx context.Context) ([]worldlineSource, error) {
	db := s.Time.Store.DB()
	topicRows, err := db.QueryContext(ctx, `SELECT id,title FROM topics ORDER BY id`)
	if err != nil {
		return nil, err
	}
	topicTitle := map[string]string{}
	for topicRows.Next() {
		var id, title string
		if err := topicRows.Scan(&id, &title); err != nil {
			topicRows.Close()
			return nil, err
		}
		topicTitle[id] = title
	}
	if err := topicRows.Err(); err != nil {
		topicRows.Close()
		return nil, err
	}
	if err := topicRows.Close(); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT n.id,n.conversation_id,n.kind,n.summary,n.annotation_version,n.topic_ids_json,
		t.id,t.recording_id,t.content,t.global_start_ms,t.speaker_scoped_id,a.transcript_revision,
		r.recording_started_at,r.absolute_time_known
		FROM notes n JOIN note_evidence ne ON ne.note_id=n.id
		JOIN turns t ON t.id=ne.turn_id JOIN attachment_turns at ON at.turn_id=t.id
		JOIN source_attachments a ON a.id=at.attachment_id AND a.active=1
		JOIN recordings r ON r.id=t.recording_id
		WHERE NOT `+memory.NoteStalePredicate("n")+` AND `+memory.CurrentSpeechTurnPredicate("t")+`
		AND NOT EXISTS(SELECT 1 FROM note_evidence ne2 JOIN turn_classes tc2 ON tc2.turn_id=ne2.turn_id AND tc2.class='ambient' WHERE ne2.note_id=n.id)
		ORDER BY n.id,ne.ordinal,a.transcript_revision`)
	if err != nil {
		return nil, err
	}
	byID := map[string]*worldlineSource{}
	projectionCache := map[string]map[string]string{}
	order := []string{}
	for rows.Next() {
		var noteID, conversationID, kind, summary, annotationVersion, topicsJSON string
		var turnID, recordingID, text, revision string
		var scopedSpeaker sql.NullString
		var startMS sql.NullInt64
		var started sql.NullString
		var absolute bool
		if err := rows.Scan(&noteID, &conversationID, &kind, &summary, &annotationVersion, &topicsJSON,
			&turnID, &recordingID, &text, &startMS, &scopedSpeaker, &revision, &started, &absolute); err != nil {
			rows.Close()
			return nil, err
		}
		base := byID[noteID]
		if base == nil {
			var topicIDs []string
			if err := json.Unmarshal([]byte(topicsJSON), &topicIDs); err != nil {
				rows.Close()
				return nil, fmt.Errorf("decode note topic IDs: %w", err)
			}
			category, categoryKey := "facts", "facts"
			if kind != "observation" {
				category, categoryKey = "plans", "plans"
			}
			sourceVersion := annotationVersion + "-" + shortHash(kind+"\x00"+summary+"\x00"+topicsJSON)
			claimID := "claim-" + shortHash(noteID)
			base = &worldlineSource{topicIDs: topicIDs, topicTitles: map[string]string{}, claim: WorldlineClaim{
				ID: claimID, Version: sourceVersion, Summary: summary, Kind: kind, Status: "unconfirmed",
				TimeBasis: "unknown", TimePrecision: "unknown", ChangeType: "report", Evidence: []WorldlineEvidence{}, ConflictWith: []string{},
				CategoryKey: categoryKey, Metadata: map[string]any{"source_note_id": noteID, "source_note_version": sourceVersion},
			}}
			if category == "plans" {
				base.claim.Status = "unconfirmed"
			}
			byID[noteID] = base
			order = append(order, noteID)
		}
		for _, topicID := range base.topicIDs {
			if title := topicTitle[topicID]; title != "" {
				base.topicTitles[topicID] = title
			}
		}
		evidence := WorldlineEvidence{TurnID: turnID, Revision: revision, ConversationID: conversationID, RecordingID: recordingID, Text: text}
		if scopedSpeaker.Valid {
			evidence.ScopedSpeaker = scopedSpeaker.String
		}
		if startMS.Valid {
			v := startMS.Int64
			evidence.OffsetMS = &v
		}
		duplicate := false
		for _, prior := range base.claim.Evidence {
			if prior.TurnID == evidence.TurnID && prior.Revision == evidence.Revision {
				duplicate = true
				break
			}
		}
		if !duplicate {
			base.claim.Evidence = append(base.claim.Evidence, evidence)
		}
		if absolute && started.Valid {
			if start, err := memory.ParseStartedAt(started.String); err == nil && start != nil {
				mentioned := start.UTC()
				if startMS.Valid {
					mentioned = mentioned.Add(time.Duration(startMS.Int64) * time.Millisecond)
				}
				if base.claim.MentionedAt == nil || mentioned.Before(*base.claim.MentionedAt) {
					base.claim.MentionedAt = &mentioned
				}
				if derived := relativeWorldlineDate(text, mentioned); len(derived) > 0 {
					prior, _ := base.claim.Metadata["derived_dates"].([]string)
					base.claim.Metadata["derived_dates"] = append(prior, derived...)
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// Resolve confirmed identities only after releasing the transcript cursor;
	// the SQLite store intentionally has a single connection in many setups.
	for _, id := range order {
		claim := &byID[id].claim
		for i := range claim.Evidence {
			e := &claim.Evidence[i]
			if e.ScopedSpeaker == "" {
				continue
			}
			key := e.RecordingID + "\x00" + e.Revision
			projection, loaded := projectionCache[key]
			if !loaded {
				projection, err = s.Time.Store.SpeakerProjection(ctx, e.RecordingID, e.Revision)
				if err != nil {
					return nil, err
				}
				projectionCache[key] = projection
			}
			e.SubjectID = projection[e.ScopedSpeaker]
		}
	}
	out := make([]worldlineSource, 0, len(order))
	for _, id := range order {
		source := *byID[id]
		// Resolve identity only if every evidence turn has a confirmed binding
		// to the same person. An unknown or mixed speaker permanently keeps the
		// note in its own unknown-subject lane.
		var subject string
		knownSubject := len(source.claim.Evidence) > 0
		for _, evidence := range source.claim.Evidence {
			if evidence.SubjectID == "" || subject != "" && subject != evidence.SubjectID {
				knownSubject = false
				break
			}
			subject = evidence.SubjectID
		}
		if knownSubject {
			source.claim.SubjectKey = "person:" + subject
		}
		applyClaimTime(&source.claim)
		if source.claim.SubjectKey == "" {
			source.claim.SubjectKey = "unknown:" + source.claim.ID
		}
		for _, tid := range source.topicIDs {
			if title := source.topicTitles[tid]; title == "" {
				source.topicTitles[tid] = "未命名主题"
			}
		}
		out = append(out, source)
	}
	return out, nil
}

var worldlineDateRE = regexp.MustCompile(`(?:([0-9]{4})-([0-9]{1,2})-([0-9]{1,2})|([0-9]{4})年([0-9]{1,2})月([0-9]{1,2})日?)`)
var dateValidityRE = regexp.MustCompile(`(?:自|从|截至|起于)\s*(?:[0-9]{4}-[0-9]{1,2}-[0-9]{1,2}|[0-9]{4}年[0-9]{1,2}月[0-9]{1,2}日?)|(?:[0-9]{4}-[0-9]{1,2}-[0-9]{1,2}|[0-9]{4}年[0-9]{1,2}月[0-9]{1,2}日?)(?:起|开始|入职|搬入|搬到|离开|辞职|发生|生效|至今)`)
var relativeValidityRE = regexp.MustCompile(`(?:今天|昨天).{0,6}(?:起|开始|入职|搬入|搬到|离开|辞职|发生|住在|工作于)`)

func applyClaimTime(claim *WorldlineClaim) {
	if !dateValidityRE.MatchString(claim.Summary) && !relativeValidityRE.MatchString(claim.Summary) {
		return
	}
	dates := map[string]time.Time{}
	for _, match := range worldlineDateRE.FindAllStringSubmatch(claim.Summary, -1) {
		dateText := match[0]
		if !dateHasValidityLanguage(claim.Summary, dateText) || !evidenceSupportsValidity(claim.Evidence, dateText) {
			continue
		}
		var parsed time.Time
		var err error
		zone := time.FixedZone("Asia/Shanghai", 8*60*60)
		if strings.Contains(dateText, "年") {
			parsed, err = time.ParseInLocation("2006年1月2日", dateText, zone)
		} else {
			for _, layout := range []string{"2006-1-2", "2006-01-02", "2006-1-02", "2006-01-2"} {
				parsed, err = time.ParseInLocation(layout, dateText, zone)
				if err == nil {
					break
				}
			}
		}
		if err == nil {
			dates[parsed.Format("2006-01-02")] = parsed
		}
	}
	if derived, ok := claim.Metadata["derived_dates"].([]string); ok && (strings.Contains(claim.Summary, "今天") || strings.Contains(claim.Summary, "昨天")) {
		for _, raw := range derived {
			marker, dateText, found := strings.Cut(raw, ":")
			if !found || !strings.Contains(claim.Summary, marker) || !evidenceContainsDate(claim.Evidence, marker) {
				continue
			}
			zone := time.FixedZone("Asia/Shanghai", 8*60*60)
			if parsed, err := time.ParseInLocation("2006-01-02", dateText, zone); err == nil {
				dates[marker+":"+parsed.Format("2006-01-02")] = parsed
			}
		}
	}
	if len(dates) == 1 {
		for dateText, date := range dates {
			if strings.Contains(dateText, ":") {
				claim.ValidFrom = &date
				claim.TimeBasis = "relative_date"
				claim.TimePrecision = "open"
			} else if dateIsCutoff(claim.Summary, dateText) {
				claim.ValidUntil = &date
				claim.TimeBasis = "explicit_valid_until"
				claim.TimePrecision = "unknown"
			} else {
				claim.ValidFrom = &date
				claim.TimeBasis = "explicit_date"
				claim.TimePrecision = "open"
			}
		}
	} else if len(dates) > 1 {
		claim.TimeBasis = "ambiguous_dates"
		claim.TimePrecision = "unknown"
	}
}

func evidenceContainsDate(evidence []WorldlineEvidence, date string) bool {
	for _, e := range evidence {
		if strings.Contains(e.Text, date) {
			return true
		}
	}
	return false
}

func evidenceSupportsValidity(evidence []WorldlineEvidence, date string) bool {
	for _, e := range evidence {
		if strings.Contains(e.Text, date) && dateHasValidityLanguage(e.Text, date) {
			return true
		}
	}
	return false
}

func dateHasValidityLanguage(summary, date string) bool {
	idx := strings.Index(summary, date)
	if idx < 0 {
		return false
	}
	before := strings.TrimSpace(summary[:idx])
	after := strings.TrimSpace(summary[idx+len(date):])
	for _, marker := range []string{"自", "从", "截至", "起于"} {
		if strings.HasSuffix(before, marker) {
			return true
		}
	}
	for _, marker := range []string{"起", "开始", "入职", "搬入", "搬到", "离开", "辞职", "发生", "生效", "至今"} {
		if strings.HasPrefix(after, marker) {
			return true
		}
	}
	return false
}

func dateIsCutoff(summary, date string) bool {
	candidates := []string{date}
	if parsed, err := time.Parse("2006-01-02", date); err == nil {
		candidates = append(candidates, parsed.Format("2006年1月2日"), parsed.Format("2006年1月2日")[:len(parsed.Format("2006年1月2日"))-3])
	}
	for _, candidate := range candidates {
		idx := strings.Index(summary, candidate)
		if idx >= 0 && strings.HasSuffix(strings.TrimSpace(summary[:idx]), "截至") {
			return true
		}
	}
	return false
}

func relativeWorldlineDate(text string, recordingStart time.Time) []string {
	zone := time.FixedZone("Asia/Shanghai", 8*60*60)
	local := recordingStart.In(zone)
	if strings.Contains(text, "昨天") {
		return []string{"昨天:" + local.AddDate(0, 0, -1).Format("2006-01-02")}
	}
	if strings.Contains(text, "今天") {
		return []string{"今天:" + local.Format("2006-01-02")}
	}
	return nil
}

func buildWorldline(sources []worldlineSource) ([]WorldlineTopic, error) {
	type topicAccum struct {
		topic WorldlineTopic
		lanes map[string]*WorldlineLane
	}
	group := map[string]*topicAccum{}
	for _, source := range sources {
		topicIDs := source.topicIDs
		if len(topicIDs) == 0 {
			topicIDs = []string{"unclassified:" + source.claim.ID}
			source.topicTitles[topicIDs[0]] = "未分类"
		}
		for _, sourceTopicID := range topicIDs {
			title := source.topicTitles[sourceTopicID]
			normalized := normalizeTopicTitle(title)
			key := macroTopic(normalized)
			if strings.HasPrefix(sourceTopicID, "unclassified:") {
				key = "待归类"
			}
			acc := group[key]
			if acc == nil {
				acc = &topicAccum{topic: WorldlineTopic{Title: key, SourceTopicIDs: []string{}, Lanes: []WorldlineLane{}}, lanes: map[string]*WorldlineLane{}}
				group[key] = acc
			}
			acc.topic.SourceTopicIDs = append(acc.topic.SourceTopicIDs, sourceTopicID)
			laneKey := normalized + "\x00" + source.claim.CategoryKey + "\x00" + source.claim.SubjectKey
			if strings.HasPrefix(source.claim.SubjectKey, "unknown:") {
				laneKey += "\x00" + source.claim.ID
			}
			lane := acc.lanes[laneKey]
			if lane == nil {
				subject := source.claim.SubjectKey
				if strings.HasPrefix(subject, "person:") {
					subject = strings.TrimPrefix(subject, "person:")
				} else {
					subject = "unknown"
				}
				lane = &WorldlineLane{Title: title, SourceTopicIDs: []string{}, Subject: subject, Category: source.claim.CategoryKey, Claims: []WorldlineClaim{}}
				acc.lanes[laneKey] = lane
			}
			lane.SourceTopicIDs = append(lane.SourceTopicIDs, sourceTopicID)
			claim := source.claim
			claim.ID = "claim-" + shortHash(claim.ID+"\x00"+sourceTopicID)
			claim.TopicID = normalized
			claim.SourceTopicID = sourceTopicID
			claim.SourceTopicTitle = title
			lane.Claims = append(lane.Claims, claim)
		}
	}
	keys := make([]string, 0, len(group))
	for key := range group {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]WorldlineTopic, 0, len(keys))
	for _, key := range keys {
		acc := group[key]
		acc.topic.SourceTopicIDs = uniqueSorted(acc.topic.SourceTopicIDs)
		acc.topic.SourceTopicCount = len(acc.topic.SourceTopicIDs)
		acc.topic.ID = "topic-" + shortHash("macro:"+key)
		if len(acc.topic.SourceTopicIDs) > 32 {
			acc.topic.SourceTopicIDs = acc.topic.SourceTopicIDs[:32]
			acc.topic.HasMore = true
		}
		laneKeys := make([]string, 0, len(acc.lanes))
		for k := range acc.lanes {
			laneKeys = append(laneKeys, k)
		}
		sort.Strings(laneKeys)
		for _, laneKey := range laneKeys {
			lane := acc.lanes[laneKey]
			lane.SourceTopicIDs = uniqueSorted(lane.SourceTopicIDs)
			lane.ID = "lane-" + shortHash(laneKey)
			sort.Slice(lane.Claims, func(i, j int) bool { return lane.Claims[i].ID < lane.Claims[j].ID })
			acc.topic.Lanes = append(acc.topic.Lanes, *lane)
		}
		out = append(out, acc.topic)
	}
	return out, nil
}

func (s *WorldlineService) factClaim(ctx context.Context, event plane.Event, all []plane.Event, currentNotes map[string]string) (WorldlineClaim, bool) {
	if event.StructuredData == nil || event.StructuredData["purged"] == true {
		return WorldlineClaim{}, false
	}
	if !feedbackSourcesCurrent(event, currentNotes) {
		return WorldlineClaim{}, false
	}
	refs, malformed := sourceReferences(event)
	if malformed || len(refs) == 0 {
		return WorldlineClaim{}, false
	}
	evidence := []WorldlineEvidence{}
	for _, ref := range refs {
		at := strings.LastIndex(ref, "@")
		if at < 1 || at == len(ref)-1 {
			return WorldlineClaim{}, false
		}
		var conv, rec, text string
		var revision string
		var offset sql.NullInt64
		var scoped sql.NullString
		err := s.Time.Store.DB().QueryRowContext(ctx, `SELECT t.conversation_id,t.recording_id,t.content,t.global_start_ms,a.transcript_revision,t.speaker_scoped_id
			FROM turns t JOIN attachment_turns at ON at.turn_id=t.id
			JOIN source_attachments a ON a.id=at.attachment_id AND a.active=1
			WHERE t.id=? AND NOT EXISTS(SELECT 1 FROM turn_classes tc WHERE tc.turn_id=t.id AND tc.class='ambient')`, ref[:at]).Scan(&conv, &rec, &text, &offset, &revision, &scoped)
		if err != nil || revision != ref[at+1:] {
			return WorldlineClaim{}, false
		}
		ev := WorldlineEvidence{TurnID: ref[:at], Revision: revision, ConversationID: conv, RecordingID: rec, Text: text}
		if scoped.Valid {
			projection, err := s.Time.Store.SpeakerProjection(ctx, rec, revision)
			if err != nil {
				return WorldlineClaim{}, false
			}
			ev.SubjectID = projection[scoped.String]
		}
		if offset.Valid {
			v := offset.Int64
			ev.OffsetMS = &v
		}
		evidence = append(evidence, ev)
	}
	cat := "facts"
	claim := WorldlineClaim{ID: event.ID, Version: event.ID, Summary: event.Content, Kind: "observation", Status: "unconfirmed", TimeBasis: "unknown", TimePrecision: "unknown", ChangeType: "report", Evidence: evidence, ConflictWith: []string{}, CategoryKey: cat, SubjectKey: s.eventSubject(ctx, event, evidence), Metadata: map[string]any{"source_event_id": event.ID}}
	if change, ok := event.StructuredData["change_type"].(string); ok && change != "" {
		claim.ChangeType = change
	}
	if event.ValidAt != nil {
		t := event.ValidAt.UTC()
		claim.ValidFrom, claim.TimeBasis, claim.TimePrecision = &t, "valid_at", "instant"
	} else if event.ValidFrom != nil {
		t := event.ValidFrom.UTC()
		claim.ValidFrom, claim.TimeBasis, claim.TimePrecision = &t, "valid_from", "open"
	}
	if event.ValidUntil != nil {
		t := event.ValidUntil.UTC()
		claim.ValidUntil = &t
		claim.TimePrecision = "interval"
	}
	if len(evidence) > 0 {
		claim.SourceTopicTitle = strings.TrimSpace(fmt.Sprint(event.StructuredData["topic_title"]))
		claim.TopicID = normalizeTopicTitle(claim.SourceTopicTitle)
		if claim.TopicID == "" || claim.TopicID == "<nil>" {
			claim.TopicID = "待归类"
		}
	}
	for _, candidate := range all {
		if (candidate.Type == "fact.corrected" || candidate.Type == "fact.retracted") && eventChainRoot(candidate, all) == event.ID {
			if candidate.RecordedAt != nil && event.RecordedAt != nil && !candidate.RecordedAt.After(*event.RecordedAt) {
				continue
			}
			if candidate.Type == "fact.retracted" {
				return WorldlineClaim{}, false
			}
			claim.Summary = candidate.Content
			claim.Version = candidate.ID
			claim.ChangeType = "record_correction"
			if candidate.ValidAt != nil {
				t := candidate.ValidAt.UTC()
				claim.ValidFrom, claim.TimeBasis, claim.TimePrecision = &t, "corrected_valid_at", "instant"
			}
		}
	}
	return claim, true
}

func eventChainRoot(event plane.Event, all []plane.Event) string {
	byID := map[string]plane.Event{}
	for _, e := range all {
		byID[e.ID] = e
	}
	id := event.ID
	for i := 0; i < len(all)+1; i++ {
		e, ok := byID[id]
		if !ok {
			return ""
		}
		parent, _ := e.StructuredData["target_event_id"].(string)
		if parent == "" {
			return id
		}
		id = parent
	}
	return ""
}

func (s *WorldlineService) eventSubject(ctx context.Context, event plane.Event, evidence []WorldlineEvidence) string {
	if len(evidence) == 0 {
		return "unknown:" + event.ID
	}
	var subject string
	for _, e := range evidence {
		if e.SubjectID == "" {
			return "unknown:" + event.ID
		}
		if subject == "" {
			subject = e.SubjectID
		} else if subject != e.SubjectID {
			return "unknown:" + event.ID
		}
	}
	return "person:" + subject
}

func appendEventClaim(topics *[]WorldlineTopic, claim WorldlineClaim) {
	key := "待归类"
	if claim.TopicID != "" {
		key = macroTopic(claim.TopicID)
	}
	macroID := "topic-" + shortHash("macro:"+key)
	var topic *WorldlineTopic
	for i := range *topics {
		if (*topics)[i].ID == macroID {
			topic = &(*topics)[i]
			break
		}
	}
	if topic == nil {
		t := WorldlineTopic{ID: macroID, Title: key, SourceTopicIDs: []string{}, Lanes: []WorldlineLane{}}
		*topics = append(*topics, t)
		topic = &(*topics)[len(*topics)-1]
	}
	subject := claim.SubjectKey
	if subject == "" {
		subject = "unknown:" + claim.ID
	} else if strings.HasPrefix(subject, "person:") {
		subject = strings.TrimPrefix(subject, "person:")
	}
	for i := range topic.Lanes {
		if topic.Lanes[i].Subject == subject && topic.Lanes[i].Category == "facts" && topic.Lanes[i].Title == "事实报告" {
			topic.Lanes[i].Claims = append(topic.Lanes[i].Claims, claim)
			return
		}
	}
	topic.Lanes = append(topic.Lanes, WorldlineLane{ID: "lane-" + shortHash(topic.ID+"\x00"+subject), Title: "事实报告", Subject: subject, Category: "facts", Claims: []WorldlineClaim{claim}})
}

func applyWorldlineFeedback(topics *[]WorldlineTopic, event plane.Event) {
	data := event.StructuredData
	action, _ := data["action"].(string)
	topicID, _ := data["target_topic_id"].(string)
	claimID, _ := data["target_claim_id"].(string)
	title, _ := data["title"].(string)
	summary, _ := data["summary"].(string)
	topicIndex, laneIndex := findWorldlineLane(*topics, topicID)
	switch action {
	case "rename_topic":
		if topicIndex >= 0 && strings.TrimSpace(title) != "" {
			(*topics)[topicIndex].Lanes[laneIndex].Title = strings.TrimSpace(title)
		}
	case "review", "correct", "set_time":
		for ti := range *topics {
			for li := range (*topics)[ti].Lanes {
				for ci := range (*topics)[ti].Lanes[li].Claims {
					claim := &(*topics)[ti].Lanes[li].Claims[ci]
					if claim.ID != claimID {
						continue
					}
					switch action {
					case "review":
						claim.Status = "reviewed"
					case "correct":
						if summary != "" {
							claim.Summary = summary
						}
						claim.ChangeType = "record_correction"
						claim.Status = "reviewed"
					case "set_time":
						if value, ok := data["valid_from"].(string); ok {
							if parsed, err := time.Parse(time.RFC3339, value); err == nil {
								claim.ValidFrom, claim.TimeBasis, claim.TimePrecision = &parsed, "reviewed_valid_time", "open"
							}
						}
						if value, ok := data["valid_until"].(string); ok {
							if parsed, err := time.Parse(time.RFC3339, value); err == nil {
								claim.ValidUntil, claim.TimeBasis, claim.TimePrecision = &parsed, "reviewed_valid_interval", "interval"
							}
						}
						claim.Status = "reviewed"
					}
					claim.Version = event.ID
					return
				}
			}
		}
	case "split_topic":
		if topicIndex < 0 {
			return
		}
		ids := stringSet(data["claim_ids"])
		if len(ids) == 0 || strings.TrimSpace(title) == "" {
			return
		}
		sourceLane := &(*topics)[topicIndex].Lanes[laneIndex]
		moved := []WorldlineClaim{}
		kept := []WorldlineClaim{}
		for _, claim := range sourceLane.Claims {
			if ids[claim.ID] {
				moved = append(moved, claim)
			} else {
				kept = append(kept, claim)
			}
		}
		if len(moved) == 0 {
			return
		}
		sourceLane.Claims = kept
		newLane := WorldlineLane{ID: "lane-split-" + shortHash(event.ID), Title: strings.TrimSpace(title), SourceTopicIDs: append([]string{}, sourceLane.SourceTopicIDs...), Subject: sourceLane.Subject, Category: sourceLane.Category, Claims: moved}
		(*topics)[topicIndex].Lanes = append((*topics)[topicIndex].Lanes, newLane)
	case "merge_topics":
		destinationID, _ := data["destination_topic_id"].(string)
		destTopicIndex, destLaneIndex := findWorldlineLane(*topics, destinationID)
		if topicIndex < 0 || destTopicIndex < 0 || topicIndex == destTopicIndex && laneIndex == destLaneIndex {
			return
		}
		sourceLane := &(*topics)[topicIndex].Lanes[laneIndex]
		destinationLane := &(*topics)[destTopicIndex].Lanes[destLaneIndex]
		if sourceLane.Subject == "unknown" || destinationLane.Subject == "unknown" || sourceLane.Subject != destinationLane.Subject || sourceLane.Category != destinationLane.Category {
			return
		}
		destinationLane.SourceTopicIDs = uniqueSorted(append(destinationLane.SourceTopicIDs, sourceLane.SourceTopicIDs...))
		destinationLane.Claims = append(destinationLane.Claims, sourceLane.Claims...)
		(*topics)[topicIndex].Lanes = append((*topics)[topicIndex].Lanes[:laneIndex], (*topics)[topicIndex].Lanes[laneIndex+1:]...)
	case "move_claim":
		destinationID, _ := data["destination_topic_id"].(string)
		destTopicIndex, destLaneIndex := findWorldlineLane(*topics, destinationID)
		if destTopicIndex < 0 {
			return
		}
		for ti := range *topics {
			for li := range (*topics)[ti].Lanes {
				for ci := range (*topics)[ti].Lanes[li].Claims {
					claim := (*topics)[ti].Lanes[li].Claims[ci]
					if claim.ID != claimID {
						continue
					}
					sourceLane := &(*topics)[ti].Lanes[li]
					destinationLane := &(*topics)[destTopicIndex].Lanes[destLaneIndex]
					if sourceLane.Subject == "unknown" || destinationLane.Subject == "unknown" || sourceLane.Subject != destinationLane.Subject || sourceLane.Category != destinationLane.Category {
						return
					}
					destinationLane.Claims = append(destinationLane.Claims, claim)
					sourceLane.Claims = append(sourceLane.Claims[:ci], sourceLane.Claims[ci+1:]...)
					return
				}
			}
		}
	}
}

func (s *WorldlineService) Feedback(ctx context.Context, scope string, req WorldlineFeedback) (string, error) {
	if s == nil || s.Time == nil || strings.TrimSpace(scope) == "" {
		return "", errors.New("worldline service and profile scope required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(req.ExpectedVersion) == "" {
		return "", errors.New("expected_version required")
	}
	if !validWorldlineAction(req.Action) {
		return "", errors.New("unsupported worldline feedback action")
	}
	allTopics, _, currentVersion, err := s.project(ctx, scope)
	if err != nil {
		return "", err
	}
	// A byte-identical retry uses the deterministic event ID and succeeds even
	// though the prior append changed the projection version.
	data, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	id := "wl-feedback-" + shortHash(scope+"\x00"+req.ExpectedVersion+"\x00"+string(data))
	events, err := s.Time.History(ctx, scope)
	if err != nil {
		return "", err
	}
	for _, event := range events {
		if event.ID == id {
			return currentVersion, nil
		}
	}
	if currentVersion != req.ExpectedVersion {
		return currentVersion, fmt.Errorf("worldline version conflict: current version is %s", currentVersion)
	}
	if err := validateWorldlineFeedback(allTopics, req); err != nil {
		return currentVersion, err
	}
	if req.Action == "change" {
		if claim := findWorldlineClaim(allTopics, req.TargetClaimID); claim != nil {
			if err := s.validateChangeEvidence(ctx, claim, req.Evidence); err != nil {
				return currentVersion, err
			}
		}
		return s.appendRealityChange(ctx, scope, req, allTopics)
	}
	if req.Action == "correct" {
		claim := findWorldlineClaim(allTopics, req.TargetClaimID)
		if claim != nil && claim.Metadata["source_event_id"] != nil {
			if _, err := s.appendTemporalCorrection(ctx, scope, req, id, claim); err != nil {
				return currentVersion, err
			}
			updated, err := s.List(ctx, scope, WorldlineQuery{Status: "all", Limit: 500})
			if err != nil {
				return "", err
			}
			return updated.Version, nil
		}
	}
	refs := feedbackReferences(allTopics, req)
	if len(refs) == 0 {
		return currentVersion, errors.New("feedback requires current source-backed evidence")
	}
	sourceNotes := feedbackSourceNotes(allTopics, req)
	structured := map[string]any{"scope": scope, "origin": "worldline", "action": req.Action, "target_topic_id": req.TargetTopicID, "target_claim_id": req.TargetClaimID, "destination_topic_id": req.DestinationTopicID, "title": req.Title, "summary": req.Summary, "claim_ids": req.ClaimIDs, "source_note_ids": sourceNotes, "source_note_versions": feedbackSourceVersions(allTopics, req)}
	if req.ValidFrom != nil {
		structured["valid_from"] = req.ValidFrom.UTC().Format(time.RFC3339Nano)
	}
	if req.ValidUntil != nil {
		structured["valid_until"] = req.ValidUntil.UTC().Format(time.RFC3339Nano)
	}
	if req.MentionedAt != nil {
		structured["mentioned_at"] = req.MentionedAt.UTC().Format(time.RFC3339Nano)
	}
	structured["references"] = refs
	now := time.Now().UTC()
	event := plane.Event{ID: id, Type: "worldline.feedback", Timestamp: now, ObservedAt: now, RecordedAt: &now, Source: "local-web", Content: "worldline " + req.Action, Confidence: 1, StructuredData: structured}
	conversations := feedbackConversations(allTopics, req)
	if len(conversations) == 1 {
		event.ConversationID = conversations[0]
	}
	if _, err := s.Time.Events.Ingest(ctx, []plane.Event{event}); err != nil {
		return currentVersion, err
	}
	updated, err := s.List(ctx, scope, WorldlineQuery{Status: "all", Limit: 500})
	if err != nil {
		return "", err
	}
	return updated.Version, nil
}

func (s *WorldlineService) appendTemporalCorrection(ctx context.Context, scope string, req WorldlineFeedback, id string, claim *WorldlineClaim) (int, error) {
	knownAt := time.Now().UTC()
	evidence := req.Evidence
	if len(evidence) == 0 {
		for _, ref := range claim.Evidence {
			evidence = append(evidence, EvidenceReference{TurnID: ref.TurnID, Revision: ref.Revision})
		}
	}
	content := strings.TrimSpace(req.Summary)
	correction := plane.Correction{ID: id, Scope: scope, TargetID: req.TargetClaimID, ExpectedHead: claim.Version, Content: &content, KnownAt: knownAt, Source: "local-web", References: evidenceStrings(evidence)}
	return s.Time.Correct(ctx, CorrectionRequest{Correction: correction, Evidence: evidence})
}

func (s *WorldlineService) appendRealityChange(ctx context.Context, scope string, req WorldlineFeedback, topics []WorldlineTopic) (string, error) {
	if len(req.Evidence) == 0 || strings.TrimSpace(req.Summary) == "" {
		return "", errors.New("change requires summary and current evidence")
	}
	data, _ := json.Marshal(req)
	id := "wl-change-" + shortHash(scope+"\x00"+req.ExpectedVersion+"\x00"+string(data))
	now := time.Now().UTC()
	claim := findWorldlineClaim(topics, req.TargetClaimID)
	metadata := map[string]any{"dimension": "topic", "change_type": "reality_change", "target_claim_id": req.TargetClaimID, "conflict_with": []string{req.TargetClaimID}}
	if claim != nil {
		metadata["source_note_ids"] = feedbackSourceNotes(topics, WorldlineFeedback{TargetClaimID: req.TargetClaimID})
		metadata["source_note_versions"] = feedbackSourceVersions(topics, WorldlineFeedback{TargetClaimID: req.TargetClaimID})
	}
	event := plane.Event{ID: id, Type: "fact.observed", Timestamp: now, ObservedAt: now, Source: "local-web", Content: strings.TrimSpace(req.Summary), Confidence: 0.5, ValidAt: req.ValidFrom, ValidUntil: req.ValidUntil, StructuredData: metadata}
	_, err := s.Time.ObserveFact(ctx, scope, FactRequest{Event: event, Evidence: req.Evidence})
	if err != nil {
		return "", err
	}
	return id, nil
}

func validWorldlineAction(action string) bool {
	switch action {
	case "rename_topic", "merge_topics", "split_topic", "move_claim", "review", "correct", "change", "set_time":
		return true
	default:
		return false
	}
}

func findWorldlineLane(topics []WorldlineTopic, id string) (int, int) {
	for ti := range topics {
		for li := range topics[ti].Lanes {
			if topics[ti].Lanes[li].ID == id {
				return ti, li
			}
		}
	}
	return -1, -1
}

func validateWorldlineFeedback(topics []WorldlineTopic, req WorldlineFeedback) error {
	targetTI, targetLI := findWorldlineLane(topics, req.TargetTopicID)
	destTI, destLI := findWorldlineLane(topics, req.DestinationTopicID)
	claim := findWorldlineClaim(topics, req.TargetClaimID)
	switch req.Action {
	case "rename_topic":
		if targetTI < 0 || strings.TrimSpace(req.Title) == "" {
			return errors.New("rename_topic requires an existing lane and title")
		}
	case "merge_topics":
		if targetTI < 0 || destTI < 0 || targetTI == destTI && targetLI == destLI {
			return errors.New("merge_topics requires two distinct existing lanes")
		}
		a, b := topics[targetTI].Lanes[targetLI], topics[destTI].Lanes[destLI]
		if a.Subject == "unknown" || b.Subject == "unknown" || a.Subject != b.Subject || a.Category != b.Category {
			return errors.New("merge_topics requires the same confirmed subject and category")
		}
	case "split_topic":
		if targetTI < 0 || strings.TrimSpace(req.Title) == "" || len(req.ClaimIDs) == 0 {
			return errors.New("split_topic requires a lane, new title, and claim_ids")
		}
		for _, id := range req.ClaimIDs {
			found := false
			for _, c := range topics[targetTI].Lanes[targetLI].Claims {
				found = found || c.ID == id
			}
			if !found {
				return errors.New("split_topic claim_ids must belong to the target lane")
			}
		}
	case "move_claim":
		if claim == nil || destTI < 0 {
			return errors.New("move_claim requires a current claim and destination lane")
		}
		sourceTI, sourceLI := claimLane(topics, req.TargetClaimID)
		if sourceTI < 0 || sourceTI == destTI && sourceLI == destLI {
			return errors.New("move_claim must select another lane")
		}
		source, dest := topics[sourceTI].Lanes[sourceLI], topics[destTI].Lanes[destLI]
		if source.Subject == "unknown" || dest.Subject == "unknown" || source.Subject != dest.Subject || source.Category != dest.Category {
			return errors.New("cross-subject or cross-category move is not allowed")
		}
	case "review":
		if claim == nil {
			return errors.New("review requires a current claim")
		}
	case "correct":
		if claim == nil || strings.TrimSpace(req.Summary) == "" {
			return errors.New("correct requires a current claim and replacement summary")
		}
	case "set_time":
		if claim == nil || req.ValidFrom == nil && req.ValidUntil == nil || req.ValidFrom != nil && req.ValidUntil != nil && !req.ValidUntil.After(*req.ValidFrom) {
			return errors.New("set_time requires a current claim and a valid time bound")
		}
	case "change":
		if claim == nil || claim.CategoryKey != "facts" || strings.TrimSpace(req.Summary) == "" || len(req.Evidence) == 0 {
			return errors.New("change requires a current reported fact, replacement summary, and source evidence")
		}
	}
	return nil
}

func claimLane(topics []WorldlineTopic, id string) (int, int) {
	for ti := range topics {
		for li := range topics[ti].Lanes {
			for _, c := range topics[ti].Lanes[li].Claims {
				if c.ID == id {
					return ti, li
				}
			}
		}
	}
	return -1, -1
}

func findWorldlineClaim(topics []WorldlineTopic, id string) *WorldlineClaim {
	for ti := range topics {
		for li := range topics[ti].Lanes {
			for ci := range topics[ti].Lanes[li].Claims {
				if topics[ti].Lanes[li].Claims[ci].ID == id {
					return &topics[ti].Lanes[li].Claims[ci]
				}
			}
		}
	}
	return nil
}

func feedbackClaims(topics []WorldlineTopic, req WorldlineFeedback) []*WorldlineClaim {
	claims := []*WorldlineClaim{}
	for ti := range topics {
		for li := range topics[ti].Lanes {
			lane := &topics[ti].Lanes[li]
			for ci := range lane.Claims {
				claim := &lane.Claims[ci]
				include := false
				switch req.Action {
				case "rename_topic":
					include = lane.ID == req.TargetTopicID
				case "merge_topics":
					include = lane.ID == req.TargetTopicID || lane.ID == req.DestinationTopicID
				case "split_topic":
					include = lane.ID == req.TargetTopicID && containsString(req.ClaimIDs, claim.ID)
				case "move_claim", "review", "correct", "change", "set_time":
					include = claim.ID == req.TargetClaimID
				}
				if include {
					claims = append(claims, claim)
				}
			}
		}
	}
	return claims
}

func feedbackReferences(topics []WorldlineTopic, req WorldlineFeedback) []string {
	refs := []string{}
	for _, claim := range feedbackClaims(topics, req) {
		for _, evidence := range claim.Evidence {
			if evidence.TurnID != "" && evidence.Revision != "" {
				refs = append(refs, evidence.TurnID+"@"+evidence.Revision)
			}
		}
	}
	if len(refs) == 0 {
		refs = evidenceStrings(req.Evidence)
	}
	return uniqueSorted(refs)
}

func feedbackSourceNotes(topics []WorldlineTopic, req WorldlineFeedback) []string {
	ids := []string{}
	for _, claim := range feedbackClaims(topics, req) {
		if id, ok := claim.Metadata["source_note_id"].(string); ok {
			ids = append(ids, id)
		}
		if idsIn, ok := claim.Metadata["source_note_ids"].([]string); ok {
			ids = append(ids, idsIn...)
		}
	}
	return uniqueSorted(ids)
}

func feedbackSourceVersions(topics []WorldlineTopic, req WorldlineFeedback) map[string]string {
	versions := map[string]string{}
	for _, claim := range feedbackClaims(topics, req) {
		id, _ := claim.Metadata["source_note_id"].(string)
		version, _ := claim.Metadata["source_note_version"].(string)
		if id != "" && version != "" {
			versions[id] = version
		}
	}
	return versions
}

func feedbackConversations(topics []WorldlineTopic, req WorldlineFeedback) []string {
	ids := []string{}
	for _, claim := range feedbackClaims(topics, req) {
		for _, evidence := range claim.Evidence {
			ids = append(ids, evidence.ConversationID)
		}
	}
	return uniqueSorted(ids)
}

func feedbackSourcesCurrent(event plane.Event, currentNotes map[string]string) bool {
	ids := stringSet(event.StructuredData["source_note_ids"])
	if len(ids) == 0 {
		// User-entered facts can be backed directly by current transcript
		// evidence and need no parent annotation note.
		refs, _ := sourceReferences(event)
		return event.Type != "worldline.feedback" || len(refs) > 0
	}
	versions := stringMap(event.StructuredData["source_note_versions"])
	for id := range ids {
		if currentNotes[id] == "" || currentNotes[id] != versions[id] {
			return false
		}
	}
	return true
}

func stringMap(v any) map[string]string {
	out := map[string]string{}
	if values, ok := v.(map[string]any); ok {
		for k, value := range values {
			if s, ok := value.(string); ok {
				out[k] = s
			}
		}
	}
	if values, ok := v.(map[string]string); ok {
		for k, value := range values {
			out[k] = value
		}
	}
	return out
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (s *WorldlineService) validateChangeEvidence(ctx context.Context, claim *WorldlineClaim, evidence []EvidenceReference) error {
	if len(evidence) == 0 {
		return errors.New("change requires current source evidence")
	}
	for _, ref := range evidence {
		var recording, scopedValue string
		err := s.Time.Store.DB().QueryRowContext(ctx, `SELECT t.recording_id,t.speaker_scoped_id FROM turns t JOIN attachment_turns at ON at.turn_id=t.id JOIN source_attachments a ON a.id=at.attachment_id AND a.active=1 WHERE t.id=? AND a.transcript_revision=? AND NOT EXISTS(SELECT 1 FROM turn_classes tc WHERE tc.turn_id=t.id AND tc.class='ambient')`, ref.TurnID, ref.Revision).Scan(&recording, &scopedValue)
		if err != nil {
			return fmt.Errorf("change evidence is not current: %w", err)
		}
		projection, err := s.Time.Store.SpeakerProjection(ctx, recording, ref.Revision)
		if err != nil {
			return err
		}
		speaker, confirmed := projection[scopedValue]
		if claim.SubjectKey == "" || strings.HasPrefix(claim.SubjectKey, "unknown:") {
			if !containsString(func() []string {
				ids := []string{}
				for _, old := range claim.Evidence {
					ids = append(ids, old.TurnID+"@"+old.Revision)
				}
				return ids
			}(), ref.TurnID+"@"+ref.Revision) {
				return errors.New("unknown subjects cannot be merged across evidence sources")
			}
		} else if !confirmed || "person:"+speaker != claim.SubjectKey {
			return errors.New("change evidence must match the claim subject")
		}
	}
	return nil
}

func (s *WorldlineService) Generate(ctx context.Context, scope, requestID string) (map[string]any, error) {
	if strings.TrimSpace(scope) == "" || s == nil || s.Time == nil || s.Time.Events == nil {
		return nil, errors.New("worldline service and profile scope required")
	}
	if strings.TrimSpace(requestID) == "" {
		return nil, errors.New("request_id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	jobID := "wl-job-" + shortHash(scope+"\x00"+requestID)
	events, err := s.Time.History(ctx, scope)
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		if event.ID == jobID+"-complete" {
			result := map[string]any{"job_id": jobID, "status": "complete"}
			for k, v := range event.StructuredData {
				if k == "semantic_status" || k == "semantic_model" || k == "semantic_weights_version" || k == "semantic_candidate_pair_count" || k == "semantic_pair_count" || k == "semantic_suggestion_count" || k == "semantic_truncated" || k == "version" {
					result[k] = v
				}
			}
			return result, nil
		}
	}
	existing := map[string]bool{}
	for _, event := range events {
		existing[event.ID] = true
	}
	for _, status := range []string{"pending", "running"} {
		if existing[jobID+"-"+status] {
			continue
		}
		now := time.Now().UTC()
		event := plane.Event{ID: jobID + "-" + status, Type: "worldline.generation", Timestamp: now, ObservedAt: now, Source: "local-web", Content: status, Confidence: 1, StructuredData: map[string]any{"scope": scope, "origin": "worldline", "job_id": jobID, "status": status}}
		if _, err := s.Time.Events.Ingest(ctx, []plane.Event{event}); err != nil {
			return nil, err
		}
	}
	topics, _, version, err := s.project(ctx, scope)
	if err != nil {
		now := time.Now().UTC()
		failed := plane.Event{ID: jobID + "-failed", Type: "worldline.generation", Timestamp: now, ObservedAt: now, Source: "local-web", Content: "failed", Confidence: 1, StructuredData: map[string]any{"scope": scope, "origin": "worldline", "job_id": jobID, "status": "failed"}}
		_, _ = s.Time.Events.Ingest(ctx, []plane.Event{failed})
		return nil, err
	}
	semantic, err := s.evaluateWorldlineSemantics(ctx, scope, topics)
	if err != nil {
		now := time.Now().UTC()
		failed := plane.Event{ID: jobID + "-failed", Type: "worldline.generation", Timestamp: now, ObservedAt: now, Source: "local-web", Content: "failed", Confidence: 1, StructuredData: map[string]any{"scope": scope, "origin": "worldline", "job_id": jobID, "status": "failed"}}
		_, _ = s.Time.Events.Ingest(ctx, []plane.Event{failed})
		return nil, err
	}
	_, _, version, err = s.project(ctx, scope)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	structured := map[string]any{"scope": scope, "origin": "worldline", "job_id": jobID, "status": "complete", "version": version, "semantic_status": semantic.Status, "semantic_candidate_pair_count": semantic.CandidatePairCount, "semantic_pair_count": semantic.PairCount, "semantic_suggestion_count": semantic.SuggestionCount, "semantic_truncated": semantic.Truncated}
	if semantic.Model != "" {
		structured["semantic_model"] = semantic.Model
	}
	if semantic.WeightsVersion != "" {
		structured["semantic_weights_version"] = semantic.WeightsVersion
	}
	event := plane.Event{ID: jobID + "-complete", Type: "worldline.generation", Timestamp: now, ObservedAt: now, Source: "local-web", Content: "complete", Confidence: 1, StructuredData: structured}
	if _, err := s.Time.Events.Ingest(ctx, []plane.Event{event}); err != nil {
		return nil, err
	}
	return map[string]any{"job_id": jobID, "status": "complete", "version": version, "semantic_status": semantic.Status, "semantic_candidate_pair_count": semantic.CandidatePairCount, "semantic_pair_count": semantic.PairCount, "semantic_suggestion_count": semantic.SuggestionCount, "semantic_truncated": semantic.Truncated, "semantic_model": semantic.Model, "semantic_weights_version": semantic.WeightsVersion}, nil
}

func filterWorldline(topics []WorldlineTopic, q WorldlineQuery) []WorldlineTopic {
	query := strings.ToLower(strings.TrimSpace(q.Query))
	out := []WorldlineTopic{}
	for _, topic := range topics {
		copyTopic := WorldlineTopic{ID: topic.ID, Title: topic.Title, SourceTopicIDs: append([]string{}, topic.SourceTopicIDs...), SourceTopicCount: topic.SourceTopicCount, HasMore: topic.HasMore, Lanes: []WorldlineLane{}}
		for _, lane := range topic.Lanes {
			copyLane := WorldlineLane{ID: lane.ID, Title: lane.Title, SourceTopicIDs: append([]string{}, lane.SourceTopicIDs...), Subject: lane.Subject, Category: lane.Category, Claims: []WorldlineClaim{}}
			for _, claim := range lane.Claims {
				if q.Status != "all" && claim.Status != q.Status {
					continue
				}
				if q.From != nil || q.To != nil {
					if claim.ValidFrom == nil && claim.ValidUntil == nil {
						continue
					}
					if q.From != nil && claim.ValidUntil != nil && claim.ValidUntil.Before(*q.From) {
						continue
					}
					if q.To != nil && claim.ValidFrom != nil && claim.ValidFrom.After(*q.To) {
						continue
					}
				}
				if query != "" && !strings.Contains(strings.ToLower(lane.Title), query) && !claimMatches(topic.Title, lane.Subject, claim, query) {
					continue
				}
				copyLane.Claims = append(copyLane.Claims, claim)
			}
			if len(copyLane.Claims) > 0 {
				copyTopic.Lanes = append(copyTopic.Lanes, copyLane)
			}
		}
		if len(copyTopic.Lanes) > 0 {
			out = append(out, copyTopic)
		}
	}
	return out
}

func claimMatches(title, subject string, claim WorldlineClaim, q string) bool {
	if strings.Contains(strings.ToLower(title), q) || strings.Contains(strings.ToLower(subject), q) || strings.Contains(strings.ToLower(claim.Summary), q) {
		return true
	}
	for _, evidence := range claim.Evidence {
		if strings.Contains(strings.ToLower(evidence.Text), q) {
			return true
		}
	}
	return false
}

func finalizeWorldline(topics *[]WorldlineTopic) {
	claimByID := map[string]*WorldlineClaim{}
	for ti := range *topics {
		for li := range (*topics)[ti].Lanes {
			for ci := range (*topics)[ti].Lanes[li].Claims {
				claim := &(*topics)[ti].Lanes[li].Claims[ci]
				claimByID[claim.ID] = claim
			}
		}
	}
	for _, claim := range claimByID {
		for _, otherID := range claim.ConflictWith {
			claim.Status = "conflict"
			if other := claimByID[otherID]; other != nil {
				other.Status = "conflict"
				other.ConflictWith = uniqueSorted(append(other.ConflictWith, claim.ID))
			}
		}
	}
	for ti := range *topics {
		topic := &(*topics)[ti]
		for li := range topic.Lanes {
			lane := &topic.Lanes[li]
			filtered := lane.Claims[:0]
			for _, claim := range lane.Claims {
				if claim.ID == "" || claim.Summary == "" || len(claim.Evidence) == 0 {
					continue
				}
				filtered = append(filtered, claim)
			}
			lane.Claims = filtered
			sort.Slice(lane.Claims, func(i, j int) bool { return lane.Claims[i].ID < lane.Claims[j].ID })
		}
		lanes := topic.Lanes[:0]
		for _, lane := range topic.Lanes {
			if len(lane.Claims) > 0 {
				lanes = append(lanes, lane)
			}
		}
		topic.Lanes = lanes
	}
	filtered := (*topics)[:0]
	for _, topic := range *topics {
		if len(topic.Lanes) > 0 {
			filtered = append(filtered, topic)
		}
	}
	*topics = filtered
}

func worldlineStats(sources []worldlineSource, topics []WorldlineTopic) WorldlineStats {
	stats := WorldlineStats{SourceNotes: len(sources), Topics: len(topics)}
	for _, topic := range topics {
		for _, lane := range topic.Lanes {
			for _, claim := range lane.Claims {
				stats.Claims++
				if claim.ValidFrom != nil || claim.ValidUntil != nil {
					stats.DatedClaims++
				} else {
					stats.UnknownTime++
				}
				if claim.Status == "conflict" {
					stats.Conflicts++
				}
			}
		}
	}
	return stats
}

func worldlineDataRange(topics []WorldlineTopic) WorldlineRange {
	var start, end, validStart, validEnd, mentionedStart, mentionedEnd *time.Time
	for _, topic := range topics {
		for _, lane := range topic.Lanes {
			for _, claim := range lane.Claims {
				setMinMax(&validStart, &validEnd, claim.ValidFrom, claim.ValidUntil)
				setMinMax(&mentionedStart, &mentionedEnd, claim.MentionedAt, claim.MentionedAt)
			}
		}
	}
	for _, p := range []*time.Time{validStart, mentionedStart} {
		if p != nil && (start == nil || p.Before(*start)) {
			v := *p
			start = &v
		}
	}
	for _, p := range []*time.Time{validEnd, mentionedEnd} {
		if p != nil && (end == nil || p.After(*end)) {
			v := *p
			end = &v
		}
	}
	return WorldlineRange{Start: start, End: end, ValidStart: validStart, ValidEnd: validEnd, MentionedStart: mentionedStart, MentionedEnd: mentionedEnd}
}

func setMinMax(start, end **time.Time, from, until *time.Time) {
	if from != nil && (*start == nil || from.Before(**start)) {
		v := *from
		*start = &v
	}
	if until != nil && (*end == nil || until.After(**end)) {
		v := *until
		*end = &v
	}
	if until == nil && from != nil && (*end == nil || from.After(**end)) {
		v := *from
		*end = &v
	}
}

func worldlineFilteredStats(topics []WorldlineTopic) WorldlineStats {
	stats := WorldlineStats{Topics: len(topics)}
	notes := map[string]bool{}
	for _, topic := range topics {
		for _, lane := range topic.Lanes {
			for _, claim := range lane.Claims {
				stats.Claims++
				if claim.ValidFrom != nil || claim.ValidUntil != nil {
					stats.DatedClaims++
				} else {
					stats.UnknownTime++
				}
				if claim.Status == "conflict" {
					stats.Conflicts++
				}
				if id, ok := claim.Metadata["source_note_id"].(string); ok {
					notes[id] = true
				}
				for _, id := range stringSetToSlice(claim.Metadata["source_note_ids"]) {
					notes[id] = true
				}
			}
		}
	}
	stats.SourceNotes = len(notes)
	return stats
}

func stringSetToSlice(v any) []string {
	s := stringSet(v)
	out := make([]string, 0, len(s))
	for id := range s {
		out = append(out, id)
	}
	return out
}

func worldlineVersion(sources []worldlineSource, events []plane.Event) string {
	parts := make([]string, 0, len(sources)+len(events))
	for _, source := range sources {
		parts = append(parts, "note:"+source.claim.ID+":"+source.claim.Version)
		parts = append(parts, "subject:"+source.claim.ID+":"+source.claim.SubjectKey)
		for _, evidence := range source.claim.Evidence {
			if evidence.TurnID != "" && evidence.Revision != "" {
				parts = append(parts, "evidence:"+evidence.TurnID+"@"+evidence.Revision+":"+evidence.SubjectID)
			}
		}
	}
	for _, event := range events {
		if event.Type == "worldline.feedback" || event.Type == "worldline.semantic" || event.Type == "worldline.semantic_review" || event.Type == "worldline.thread_alias" || event.Type == "worldline.thread_group" || event.Type == "fact.observed" || event.Type == "topic.fact" || event.Type == "fact.corrected" || event.Type == "fact.retracted" {
			parts = append(parts, "event:"+event.ID)
		}
	}
	sort.Strings(parts)
	return "wl-" + shortHash(strings.Join(parts, "\n"))
}

func normalizeTopicTitle(title string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(title))), " ")
}

func macroTopic(title string) string {
	rules := []struct {
		name  string
		words []string
	}{
		{"工作与项目", []string{"工作", "项目", "职业", "事业", "公司", "职场", "业务", "产品", "团队", "招聘", "管理", "会议", "老板", "同事"}},
		{"学习与技术", []string{"学习", "技术", "编程", "软件", "人工智能", "机器学习", "代码", "算法", "研究", "课程", "知识", "工具", "开发", "电脑"}},
		{"生活与居住", []string{"生活", "居住", "住房", "房子", "家庭", "家里", "租房", "搬家", "装修", "日常"}},
		{"健康", []string{"健康", "身体", "医疗", "医院", "疾病", "睡眠", "运动", "健身", "心理", "治疗"}},
		{"人际关系", []string{"人际", "关系", "朋友", "亲密", "伴侣", "恋爱", "婚姻", "家庭成员", "社交"}},
		{"财务消费", []string{"财务", "钱", "预算", "消费", "购买", "购物", "投资", "收入", "支出", "价格", "费用"}},
		{"出行", []string{"出行", "旅行", "旅游", "交通", "通勤", "航班", "火车", "开车", "行程", "酒店"}},
	}
	hits := []string{}
	for _, rule := range rules {
		for _, word := range rule.words {
			if strings.Contains(title, strings.ToLower(word)) {
				hits = append(hits, rule.name)
				break
			}
		}
	}
	if len(hits) != 1 {
		return "待归类"
	}
	return hits[0]
}

func capTopicPreview(topic *WorldlineTopic, maxLanes, maxClaims, maxTopicIDs int) {
	total := topicClaimCount(*topic)
	topic.HasMore = total > maxClaims || len(topic.Lanes) > maxLanes || topic.SourceTopicCount > maxTopicIDs
	if len(topic.SourceTopicIDs) > maxTopicIDs {
		topic.SourceTopicIDs = topic.SourceTopicIDs[:maxTopicIDs]
	}
	lanes := []WorldlineLane{}
	remaining := maxClaims
	for _, lane := range topic.Lanes {
		if len(lanes) >= maxLanes || remaining <= 0 {
			topic.HasMore = true
			break
		}
		copyLane := lane
		if len(copyLane.SourceTopicIDs) > maxTopicIDs {
			copyLane.SourceTopicIDs = copyLane.SourceTopicIDs[:maxTopicIDs]
			topic.HasMore = true
		}
		if len(copyLane.Claims) > remaining {
			copyLane.Claims = copyLane.Claims[:remaining]
			topic.HasMore = true
		}
		remaining -= len(copyLane.Claims)
		lanes = append(lanes, copyLane)
	}
	topic.Lanes = lanes
}

func topicClaimCount(topic WorldlineTopic) int {
	n := 0
	for _, lane := range topic.Lanes {
		n += len(lane.Claims)
	}
	return n
}

func pageTopicClaims(topic WorldlineTopic, offset, limit int) WorldlineTopic {
	page := WorldlineTopic{ID: topic.ID, Title: topic.Title, SourceTopicIDs: append([]string{}, topic.SourceTopicIDs...), SourceTopicCount: topic.SourceTopicCount, HasMore: false, Lanes: []WorldlineLane{}}
	index, included := 0, 0
	for _, lane := range topic.Lanes {
		copyLane := WorldlineLane{ID: lane.ID, Title: lane.Title, SourceTopicIDs: append([]string{}, lane.SourceTopicIDs...), Subject: lane.Subject, Category: lane.Category, Claims: []WorldlineClaim{}}
		for _, claim := range lane.Claims {
			if index >= offset && included < limit {
				copyLane.Claims = append(copyLane.Claims, claim)
				included++
			}
			index++
		}
		if len(copyLane.Claims) > 0 {
			page.Lanes = append(page.Lanes, copyLane)
		}
	}
	return page
}
func worldlineTopicID(sourceIDs []string, normalized string) string {
	base := strings.Join(uniqueSorted(sourceIDs), "\x00")
	if base == "" {
		base = normalized
	}
	return "topic-" + shortHash(base)
}
func shortHash(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:12])
}
func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}
func evidenceStrings(evidence []EvidenceReference) []string {
	out := make([]string, 0, len(evidence))
	for _, ref := range evidence {
		if ref.TurnID != "" && ref.Revision != "" {
			out = append(out, ref.TurnID+"@"+ref.Revision)
		}
	}
	return uniqueSorted(out)
}
func stringSet(v any) map[string]bool {
	out := map[string]bool{}
	if values, ok := v.([]any); ok {
		for _, value := range values {
			if s, ok := value.(string); ok {
				out[s] = true
			}
		}
	}
	if values, ok := v.([]string); ok {
		for _, value := range values {
			out[value] = true
		}
	}
	return out
}
