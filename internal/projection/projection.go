// Package projection derives, at read time, the current context objects a
// consumer should see: people, topics, commitments, preferences and events,
// each with its evidence, provenance and a status of current, superseded or
// unresolved. Nothing is persisted: every call rebuilds the projection from
// the stored rows, so it is always consistent with the current state of the
// memory database and can never drift from it.
//
// The projection is deterministic: no map iteration order reaches the output,
// every collection is sorted before it is emitted, and no clock value feeds a
// derived field. Conflicts are reported, never deleted or silently resolved —
// a truth engine is explicitly out of scope for v0.
package projection

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"recordingmcp/internal/memory"
	"recordingmcp/internal/pipeline"
)

// Kind is the projected object kind.
type Kind string

const (
	KindPerson     Kind = "person"
	KindTopic      Kind = "topic"
	KindCommitment Kind = "commitment"
	KindPreference Kind = "preference"
	KindEvent      Kind = "event"
)

// Status is the currency verdict of one projected object. StatusConflicting is
// reserved for a future truth engine and is never emitted in v0.
type Status string

const (
	StatusCurrent     Status = "current"
	StatusSuperseded  Status = "superseded"
	StatusConflicting Status = "conflicting"
	StatusUnresolved  Status = "unresolved"
)

// Object is one projected context object. Every field is derived from stored
// rows; nothing is inferred beyond the documented rules.
type Object struct {
	ID              string   `json:"id"`
	Kind            Kind     `json:"kind"`
	Subject         string   `json:"subject"`
	Title           string   `json:"title,omitempty"`
	Summary         string   `json:"summary"`
	Status          Status   `json:"status"`
	StatusReason    string   `json:"status_reason,omitempty"`
	Confidence      float64  `json:"confidence"`
	EvidenceTurnIDs []string `json:"evidence_turn_ids"`
	DerivedFrom     []string `json:"derived_from"`
	ConversationIDs []string `json:"conversation_ids"`
	TopicIDs        []string `json:"topic_ids,omitempty"`
	PersonIDs       []string `json:"person_ids,omitempty"`
	LinkIDs         []string `json:"link_ids,omitempty"`
	UpdatedAtMS     *int64   `json:"updated_at_ms,omitempty"`
	ConflictWith    []string `json:"conflict_with,omitempty"`
}

// Conflict reports one supersede relationship the projection found: the later
// evidence negates the earlier. Conflicts are reported, never resolved.
type Conflict struct {
	Subject   string `json:"subject"`
	Kind      Kind   `json:"kind"`
	EarlierID string `json:"earlier_id"`
	LaterID   string `json:"later_id"`
	Cue       string `json:"cue"`
	Detail    string `json:"detail"`
}

// Unresolved reports why something in scope produced no object.
type Unresolved struct {
	Kind    Kind   `json:"kind"`
	Subject string `json:"subject"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail"`
}

// Coverage counts what one projection pass scanned.
type Coverage struct {
	NotesScanned     int `json:"notes_scanned"`
	TopicsScanned    int `json:"topics_scanned"`
	BindingsScanned  int `json:"bindings_scanned"`
	LinksScanned     int `json:"links_scanned"`
	ObjectsProjected int `json:"objects_projected"`
	NotesUnprojected int `json:"notes_unprojected"`
}

// Trace records the rule names that produced the output.
type Trace struct {
	Rules         []string `json:"rules"`
	SupersedeCues []string `json:"supersede_cues"`
}

// Result is the full projection output.
type Result struct {
	Experimental bool         `json:"experimental"`
	Objects      []Object     `json:"objects"`
	Conflicts    []Conflict   `json:"conflicts"`
	Unresolved   []Unresolved `json:"unresolved"`
	Coverage     Coverage     `json:"coverage"`
	Trace        Trace        `json:"trace"`
}

// Request scopes one projection. Every filter is a hard any-of filter; empty
// means unconstrained.
type Request struct {
	ConversationIDs []string
	TopicIDs        []string
	PersonIDs       []string
	Kinds           []Kind
	StartedAfter    *time.Time
	StartedBefore   *time.Time
	// Audit additionally emits stale notes as superseded objects.
	Audit bool
	// Limit caps the emitted objects after sorting (default 20, max 200).
	Limit int
}

// supersedeCues are the deterministic negation cues of the v0 conflict rule.
// Multi-character cues only: a bare "不" must never trigger, because plain
// negation ("不喜欢") is not supersession.
var supersedeCues = []string{
	"取消", "暂时不", "不再", "放弃", "改为", "延期", "不去了", "不搬", "不办",
	"no longer", "cancelled",
}

// traceRules is the fixed rule list; the order is part of the contract.
var traceRules = []string{
	"note_kind_mapping",
	"commitment_conflict_cues",
	"topic_from_membership",
	"person_from_confirmed_bindings",
}

// noteEvidenceAmbientClause mirrors the store's note-ambient rule: a note is
// ambient when its primary evidence turn (ordinal 0) is classified ambient.
const noteEvidenceAmbientClause = `NOT EXISTS(SELECT 1 FROM note_evidence ne JOIN turn_classes tc ON tc.turn_id=ne.turn_id WHERE ne.note_id=n.id AND ne.ordinal=0 AND tc.class='ambient')`

// Build derives the projection for the current state of the memory database.
func Build(ctx context.Context, store *memory.Store, db *sql.DB, req Request) (Result, error) {
	result := Result{
		Experimental: true,
		Objects:      []Object{},
		Conflicts:    []Conflict{},
		Unresolved:   []Unresolved{},
		Trace: Trace{
			Rules:         append([]string(nil), traceRules...),
			SupersedeCues: append([]string(nil), supersedeCues...),
		},
	}
	if store == nil {
		return result, fmt.Errorf("projection: store is required")
	}
	if db == nil {
		return result, fmt.Errorf("projection: db is required")
	}

	// Rule 1: commitment/preference/event objects from current, non-ambient
	// notes, plus rule 6's audit objects from stale notes.
	notes, err := loadNotes(ctx, db, false)
	if err != nil {
		return result, err
	}
	result.Coverage.NotesScanned += len(notes)
	if req.Audit {
		stale, err := loadNotes(ctx, db, true)
		if err != nil {
			return result, err
		}
		result.Coverage.NotesScanned += len(stale)
		notes = append(notes, stale...)
	}

	objects := []Object{}
	primaryStart := map[string]*int64{}
	for _, note := range notes {
		obj, projectable := projectNote(note)
		if !projectable {
			if !note.stale {
				result.Unresolved = append(result.Unresolved, Unresolved{
					Kind:    note.kind,
					Subject: note.id,
					Reason:  "kind_not_projected_in_v0",
					Detail:  fmt.Sprintf("note %s of kind %q has no projection rule in v0", note.id, note.kind),
				})
			}
			continue
		}
		if note.stale {
			obj.Status = StatusSuperseded
			obj.StatusReason = "stale_evidence"
		}
		primaryStart[obj.ID] = note.primaryStartMS
		objects = append(objects, obj)
	}
	notesUnprojected := len(result.Unresolved)

	// Rule 2: deterministic commitment conflicts within each topic group.
	conflicts := applyCommitmentConflicts(objects, primaryStart)

	// Rule 3: topic objects from current membership.
	topics, linksScanned, err := loadTopics(ctx, db)
	if err != nil {
		return result, err
	}
	result.Coverage.TopicsScanned = len(topics)
	result.Coverage.LinksScanned = linksScanned
	objects = append(objects, topics...)

	// Rule 4: person objects from confirmed speaker bindings.
	bindings, err := loadBindings(ctx, db)
	if err != nil {
		return result, err
	}
	result.Coverage.BindingsScanned = len(bindings)
	persons, err := projectPersons(ctx, db, bindings)
	if err != nil {
		return result, err
	}
	objects = append(objects, persons...)

	// Rule 7: hard scope filters, then deterministic ordering and limit.
	startedAt := map[string]*time.Time{}
	objects, err = applyScope(ctx, store, objects, req, startedAt)
	if err != nil {
		return result, err
	}
	sort.SliceStable(objects, func(i, j int) bool {
		if objects[i].Kind != objects[j].Kind {
			return objects[i].Kind < objects[j].Kind
		}
		return objects[i].Subject < objects[j].Subject
	})
	sort.SliceStable(conflicts, func(i, j int) bool {
		if conflicts[i].Subject != conflicts[j].Subject {
			return conflicts[i].Subject < conflicts[j].Subject
		}
		return conflicts[i].EarlierID < conflicts[j].EarlierID
	})
	sort.SliceStable(result.Unresolved, func(i, j int) bool {
		if result.Unresolved[i].Kind != result.Unresolved[j].Kind {
			return result.Unresolved[i].Kind < result.Unresolved[j].Kind
		}
		if result.Unresolved[i].Subject != result.Unresolved[j].Subject {
			return result.Unresolved[i].Subject < result.Unresolved[j].Subject
		}
		return result.Unresolved[i].Reason < result.Unresolved[j].Reason
	})

	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	result.Coverage.ObjectsProjected = len(objects)
	if len(objects) > limit {
		objects = objects[:limit]
	}
	result.Objects = objects
	result.Conflicts = conflicts
	result.Coverage.NotesUnprojected = notesUnprojected
	return result, nil
}

// noteRow is one loaded notes row with its resolved evidence.
type noteRow struct {
	id             string
	conversationID string
	kind           Kind
	summary        string
	topicIDs       []string
	evidence       []string
	primaryStartMS *int64
	stale          bool
}

// loadNotes reads current notes (stale=false) or stale notes (stale=true) with
// the exact current/ambient predicates the retrieval layer uses.
func loadNotes(ctx context.Context, db *sql.DB, stale bool) ([]noteRow, error) {
	query := `SELECT n.id, n.conversation_id, n.kind, n.summary, n.topic_ids_json,
		(SELECT t.global_start_ms FROM note_evidence ne2 JOIN turns t ON t.id=ne2.turn_id WHERE ne2.note_id=n.id AND ne2.ordinal=0)
		FROM notes n WHERE `
	if stale {
		query += memory.NoteStalePredicate("n")
	} else {
		query += `NOT ` + memory.NoteStalePredicate("n") + ` AND ` + noteEvidenceAmbientClause
	}
	query += ` ORDER BY n.id`
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("projection: select notes: %w", err)
	}
	defer rows.Close()

	byID := map[string]*noteRow{}
	out := make([]*noteRow, 0, 16)
	for rows.Next() {
		var (
			n         noteRow
			kind      string
			topics    string
			primarySt sql.NullInt64
		)
		if err := rows.Scan(&n.id, &n.conversationID, &kind, &n.summary, &topics, &primarySt); err != nil {
			return nil, err
		}
		n.kind = Kind(kind)
		if err := json.Unmarshal([]byte(topics), &n.topicIDs); err != nil {
			return nil, fmt.Errorf("projection: decode topic ids of %s: %w", n.id, err)
		}
		if primarySt.Valid {
			v := primarySt.Int64
			n.primaryStartMS = &v
		}
		n.stale = stale
		byID[n.id] = &n
		out = append(out, &n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	if len(out) == 0 {
		result := make([]noteRow, 0, len(out))
		for _, n := range out {
			result = append(result, *n)
		}
		return result, nil
	}
	ids := make([]string, 0, len(out))
	for _, n := range out {
		ids = append(ids, n.id)
	}
	// Chunked so the bind-parameter list can never exceed SQLITE_MAX_VARIABLE_NUMBER.
	const chunk = 400
	for start := 0; start < len(ids); start += chunk {
		end := min(start+chunk, len(ids))
		q := fmt.Sprintf(`SELECT note_id, turn_id FROM note_evidence WHERE note_id IN (%s) ORDER BY note_id, ordinal`, placeholders(end-start))
		evRows, err := db.QueryContext(ctx, q, stringsToAny(ids[start:end])...)
		if err != nil {
			return nil, fmt.Errorf("projection: select note evidence: %w", err)
		}
		for evRows.Next() {
			var noteID, turnID string
			if err := evRows.Scan(&noteID, &turnID); err != nil {
				evRows.Close()
				return nil, err
			}
			if n, ok := byID[noteID]; ok {
				n.evidence = append(n.evidence, turnID)
			}
		}
		if err := evRows.Err(); err != nil {
			evRows.Close()
			return nil, err
		}
		evRows.Close()
	}
	result := make([]noteRow, 0, len(out))
	for _, n := range out {
		result = append(result, *n)
	}
	return result, nil
}

// projectNote maps one note to its projected object. projectable=false means
// the v0 rules do not project this kind.
func projectNote(note noteRow) (Object, bool) {
	var (
		kind     Kind
		conf     float64
		timeless bool
	)
	switch note.kind {
	case "intention":
		kind, conf = KindCommitment, 0.6
	case "request":
		kind, conf = KindCommitment, 0.5
	case "constraint":
		kind, conf = KindPreference, 0.6
	case "observation":
		kind, conf = KindEvent, 0.5
		// An observation is only an event when its primary evidence turn is
		// anchored in time; otherwise the projection refuses to invent one.
		timeless = note.primaryStartMS == nil
	case "suggestion":
		return Object{}, false
	default:
		return Object{}, false
	}
	if timeless {
		return Object{}, false
	}
	subject := ""
	if len(note.topicIDs) > 0 {
		subject = note.topicIDs[0]
	} else {
		subject = "conversation:" + note.conversationID
	}
	return Object{
		ID:              note.id,
		Kind:            kind,
		Subject:         subject,
		Summary:         note.summary,
		Status:          StatusCurrent,
		Confidence:      conf,
		EvidenceTurnIDs: append([]string(nil), note.evidence...),
		DerivedFrom:     []string{"note:" + note.id},
		ConversationIDs: []string{note.conversationID},
		TopicIDs:        append([]string(nil), note.topicIDs...),
	}, true
}

// applyCommitmentConflicts walks the commitment objects of each subject group
// in evidence order and marks the first superseded pair. One conflict per
// group maximum. primaryStart carries each object's primary evidence turn
// start, keyed by object id.
func applyCommitmentConflicts(objects []Object, primaryStart map[string]*int64) []Conflict {
	groups := map[string][]*Object{}
	var order []string
	for i := range objects {
		obj := &objects[i]
		if obj.Kind != KindCommitment {
			continue
		}
		if _, ok := groups[obj.Subject]; !ok {
			order = append(order, obj.Subject)
		}
		groups[obj.Subject] = append(groups[obj.Subject], obj)
	}
	conflicts := []Conflict{}
	for _, subject := range sortedStrings(order) {
		group := groups[subject]
		startOf := func(obj *Object) *int64 { return primaryStart[obj.ID] }
		sort.SliceStable(group, func(a, b int) bool {
			ai, bi := startOf(group[a]), startOf(group[b])
			switch {
			case ai == nil && bi != nil:
				return true
			case ai != nil && bi == nil:
				return false
			case ai != nil && bi != nil && *ai != *bi:
				return *ai < *bi
			}
			return group[a].ID < group[b].ID
		})
		for i := 0; i < len(group); i++ {
			matched := false
			for j := i + 1; j < len(group); j++ {
				earlier, later := group[i], group[j]
				cue, laterCued := firstCue(later.Summary)
				if !laterCued {
					continue
				}
				if _, earlierCued := firstCue(earlier.Summary); earlierCued {
					continue
				}
				earlier.Status = StatusSuperseded
				earlier.StatusReason = "negated_by_later_note"
				earlier.ConflictWith = []string{later.ID}
				later.Status = StatusCurrent
				later.ConflictWith = []string{earlier.ID}
				conflicts = append(conflicts, Conflict{
					Subject:   subject,
					Kind:      KindCommitment,
					EarlierID: earlier.ID,
					LaterID:   later.ID,
					Cue:       cue,
					Detail:    "later note negates an earlier commitment in the same topic group",
				})
				matched = true
				break
			}
			if matched {
				break
			}
		}
	}
	return conflicts
}

// firstCue returns the first supersede cue contained in the summary.
func firstCue(summary string) (string, bool) {
	for _, cue := range supersedeCues {
		if strings.Contains(summary, cue) {
			return cue, true
		}
	}
	return "", false
}

// loadTopics builds the topic objects from current membership and reports how
// many confirmed links were scanned.
func loadTopics(ctx context.Context, db *sql.DB) ([]Object, int, error) {
	query := `SELECT t.id, t.title FROM topics t
WHERE EXISTS(SELECT 1 FROM notes n WHERE NOT ` + memory.NoteStalePredicate("n") + ` AND ` + noteEvidenceAmbientClause + `
	AND EXISTS(SELECT 1 FROM json_each(n.topic_ids_json) WHERE value=t.id))
 OR EXISTS(SELECT 1 FROM turn_topics tt JOIN turns t2 ON t2.id=tt.turn_id WHERE tt.topic_id=t.id
	AND ` + memory.CurrentTurnPredicate("t2") + ` AND NOT EXISTS(SELECT 1 FROM turn_classes tc WHERE tc.turn_id=t2.id AND tc.class='ambient'))
ORDER BY t.id`
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, 0, fmt.Errorf("projection: select topics: %w", err)
	}
	type topicRow struct {
		id, title string
	}
	topics := []topicRow{}
	for rows.Next() {
		var t topicRow
		if err := rows.Scan(&t.id, &t.title); err != nil {
			rows.Close()
			return nil, 0, err
		}
		topics = append(topics, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, err
	}
	rows.Close()

	convRows, err := db.QueryContext(ctx, `SELECT topic_id, conversation_id FROM topic_conversations ORDER BY topic_id, conversation_id`)
	if err != nil {
		return nil, 0, fmt.Errorf("projection: select topic conversations: %w", err)
	}
	topicConvs := map[string][]string{}
	for convRows.Next() {
		var tid, cid string
		if err := convRows.Scan(&tid, &cid); err != nil {
			convRows.Close()
			return nil, 0, err
		}
		topicConvs[tid] = append(topicConvs[tid], cid)
	}
	if err := convRows.Err(); err != nil {
		convRows.Close()
		return nil, 0, err
	}
	convRows.Close()

	turnRows, err := db.QueryContext(ctx, `SELECT tt.topic_id, tt.turn_id FROM turn_topics tt
JOIN turns t ON t.id=tt.turn_id
WHERE `+memory.CurrentTurnPredicate("t")+` AND NOT EXISTS(SELECT 1 FROM turn_classes tc WHERE tc.turn_id=t.id AND tc.class='ambient')
ORDER BY tt.topic_id, tt.turn_id`)
	if err != nil {
		return nil, 0, fmt.Errorf("projection: select topic turns: %w", err)
	}
	topicTurns := map[string][]string{}
	for turnRows.Next() {
		var tid, turnID string
		if err := turnRows.Scan(&tid, &turnID); err != nil {
			turnRows.Close()
			return nil, 0, err
		}
		topicTurns[tid] = append(topicTurns[tid], turnID)
	}
	if err := turnRows.Err(); err != nil {
		turnRows.Close()
		return nil, 0, err
	}
	turnRows.Close()

	links, err := pipeline.ListLinks(ctx, db, pipeline.LinkFilter{Status: pipeline.LinkStatusConfirmed})
	if err != nil {
		return nil, 0, fmt.Errorf("projection: list links: %w", err)
	}
	linksByTopic := map[string][]string{}
	for _, link := range links {
		if link.Stale {
			continue
		}
		if link.FromKind == "topic" {
			linksByTopic[link.FromID] = append(linksByTopic[link.FromID], link.ID)
		}
		if link.ToKind == "topic" {
			linksByTopic[link.ToID] = append(linksByTopic[link.ToID], link.ID)
		}
	}

	out := make([]Object, 0, len(topics))
	for _, t := range topics {
		evidence := topicTurns[t.id]
		if len(evidence) > 5 {
			evidence = evidence[:5]
		}
		out = append(out, Object{
			ID:              t.id,
			Kind:            KindTopic,
			Subject:         t.id,
			Title:           t.title,
			Summary:         t.title,
			Status:          StatusCurrent,
			Confidence:      0.7,
			EvidenceTurnIDs: evidence,
			DerivedFrom:     []string{},
			ConversationIDs: append([]string(nil), topicConvs[t.id]...),
			TopicIDs:        []string{t.id},
			LinkIDs:         sortedUnique(linksByTopic[t.id]),
		})
	}
	return out, len(links), nil
}

// bindingRow is one confirmed speaker binding.
type bindingRow struct {
	id          string
	recordingID string
	revision    string
	speaker     string
	personID    string
}

// loadBindings reads every confirmed speaker binding with a person id,
// deterministically ordered so merged summaries are stable.
func loadBindings(ctx context.Context, db *sql.DB) ([]bindingRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, recording_id, transcript_revision, speaker_scoped_id, person_id
FROM speaker_bindings WHERE status='confirmed' AND person_id IS NOT NULL AND person_id<>''
ORDER BY recording_id, speaker_scoped_id, id`)
	if err != nil {
		return nil, fmt.Errorf("projection: select speaker bindings: %w", err)
	}
	defer rows.Close()
	out := []bindingRow{}
	for rows.Next() {
		var b bindingRow
		if err := rows.Scan(&b.id, &b.recordingID, &b.revision, &b.speaker, &b.personID); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// projectPersons merges confirmed bindings per person and attaches the current
// turns of the binding's own transcript revision as evidence. A person whose
// every evidence turn is superseded is unresolved, never invented.
func projectPersons(ctx context.Context, db *sql.DB, bindings []bindingRow) ([]Object, error) {
	type personAcc struct {
		id        string
		summaries []string
		derived   []string
		evidence  []string
		convs     []string
	}
	byPerson := map[string]*personAcc{}
	var order []string
	for _, b := range bindings {
		acc, ok := byPerson[b.personID]
		if !ok {
			acc = &personAcc{id: b.personID}
			byPerson[b.personID] = acc
			order = append(order, b.personID)
		}
		acc.summaries = append(acc.summaries,
			fmt.Sprintf("confirmed speaker label %s in recording %s", b.speaker, b.recordingID))
		acc.derived = append(acc.derived, "binding:"+b.id)

		rows, err := db.QueryContext(ctx, `SELECT t.id, t.conversation_id FROM turns t
JOIN attachment_turns at ON at.turn_id=t.id
JOIN source_attachments a ON a.id=at.attachment_id
WHERE a.recording_id=? AND t.recording_id=? AND t.speaker_scoped_id=? AND a.transcript_revision=? AND a.active=1
  AND `+memory.CurrentTurnPredicate("t")+`
ORDER BY t.global_start_ms, t.id
LIMIT 5`, b.recordingID, b.recordingID, b.speaker, b.revision)
		if err != nil {
			return nil, fmt.Errorf("projection: select person evidence: %w", err)
		}
		for rows.Next() {
			var turnID, convID string
			if err := rows.Scan(&turnID, &convID); err != nil {
				rows.Close()
				return nil, err
			}
			acc.evidence = append(acc.evidence, turnID)
			acc.convs = append(acc.convs, convID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	out := make([]Object, 0, len(order))
	for _, personID := range sortedStrings(order) {
		acc := byPerson[personID]
		status := StatusCurrent
		statusReason := ""
		if len(acc.evidence) == 0 {
			status = StatusUnresolved
			statusReason = "evidence_superseded"
		}
		out = append(out, Object{
			ID:              personID,
			Kind:            KindPerson,
			Subject:         personID,
			Title:           personID,
			Summary:         strings.Join(sortedUnique(acc.summaries), "; "),
			Status:          status,
			StatusReason:    statusReason,
			Confidence:      1.0,
			EvidenceTurnIDs: sortedUnique(acc.evidence),
			DerivedFrom:     sortedUnique(acc.derived),
			ConversationIDs: sortedUnique(acc.convs),
			PersonIDs:       []string{personID},
		})
	}
	return out, nil
}

// applyScope applies the any-of hard filters and the time bounds. An object
// passes when at least one of its conversations/topics/persons is in scope;
// unknown conversation starts fail any set time bound.
func applyScope(ctx context.Context, store *memory.Store, objects []Object, req Request, startedAt map[string]*time.Time) ([]Object, error) {
	if len(req.ConversationIDs) == 0 && len(req.TopicIDs) == 0 && len(req.PersonIDs) == 0 &&
		len(req.Kinds) == 0 && req.StartedAfter == nil && req.StartedBefore == nil {
		return objects, nil
	}
	convSet := toSet(req.ConversationIDs)
	topicSet := toSet(req.TopicIDs)
	personSet := toSet(req.PersonIDs)
	kindSet := toKinds(req.Kinds)
	out := make([]Object, 0, len(objects))
	for _, obj := range objects {
		if len(convSet) > 0 && !anyAllowed(obj.ConversationIDs, convSet) {
			continue
		}
		if len(topicSet) > 0 && !anyAllowed(obj.TopicIDs, topicSet) {
			continue
		}
		if len(personSet) > 0 && !anyAllowed(obj.PersonIDs, personSet) {
			continue
		}
		if len(kindSet) > 0 && !kindSet[obj.Kind] {
			continue
		}
		if req.StartedAfter != nil || req.StartedBefore != nil {
			inTime := false
			for _, cid := range obj.ConversationIDs {
				started, err := conversationStart(ctx, store, startedAt, cid)
				if err != nil {
					return nil, err
				}
				if memory.WithinStartedRange(started, req.StartedAfter, req.StartedBefore) {
					inTime = true
					break
				}
			}
			if !inTime {
				continue
			}
		}
		out = append(out, obj)
	}
	return out, nil
}

func conversationStart(ctx context.Context, store *memory.Store, memo map[string]*time.Time, conversationID string) (*time.Time, error) {
	if started, ok := memo[conversationID]; ok {
		return started, nil
	}
	started, err := store.ConversationStartedAt(ctx, conversationID)
	if err != nil {
		return nil, fmt.Errorf("projection: conversation start of %s: %w", conversationID, err)
	}
	memo[conversationID] = started
	return started, nil
}

func toSet(in []string) map[string]bool {
	set := map[string]bool{}
	for _, s := range in {
		set[s] = true
	}
	return set
}

func toKinds(in []Kind) map[Kind]bool {
	set := map[Kind]bool{}
	for _, k := range in {
		set[k] = true
	}
	return set
}

func anyAllowed(values []string, set map[string]bool) bool {
	for _, v := range values {
		if set[v] {
			return true
		}
	}
	return false
}

func sortedUnique(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

func stringsToAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}
