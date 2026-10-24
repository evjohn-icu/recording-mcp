// Package contextbrief builds a deterministic, explicitly scoped brief from
// current local transcript evidence. It does not call a model or persist text.
package contextbrief

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"recordingmcp/internal/memory"
	"recordingmcp/internal/projection"
)

type Scope struct {
	Kind            string   `json:"kind"`
	PersonIDs       []string `json:"person_ids,omitempty"`
	TopicIDs        []string `json:"topic_ids,omitempty"`
	ConversationIDs []string `json:"conversation_ids,omitempty"`
}
type Request struct {
	Scope         Scope      `json:"scope"`
	StartedAfter  *time.Time `json:"started_after,omitempty"`
	StartedBefore *time.Time `json:"started_before,omitempty"`
	MaxItems      int        `json:"max_items,omitempty"`
	MaxEvidence   int        `json:"max_evidence,omitempty"`
	MaxBytes      int        `json:"max_bytes,omitempty"`
}
type Entry struct {
	ID             string   `json:"id"`
	Text           string   `json:"text"`
	EvidenceIDs    []string `json:"evidence_ids"`
	SourceState    string   `json:"source_state"`
	AssertionState string   `json:"assertion_state"`
	Provenance     string   `json:"provenance,omitempty"`
	SourceVersion  string   `json:"source_version,omitempty"`
	TextTruncated  bool     `json:"text_truncated,omitempty"`
}
type Sections struct {
	Background          []Entry `json:"background"`
	PreviousDiscussions []Entry `json:"previous_discussions"`
	FollowUpCandidates  []Entry `json:"follow_up_candidates"`
	ChangesConflicts    []Entry `json:"changes_conflicts"`
	Unknowns            []Entry `json:"unknowns"`
}
type Evidence struct {
	ID                 string     `json:"id"`
	TurnID             string     `json:"turn_id"`
	ConversationID     string     `json:"conversation_id"`
	RecordingID        string     `json:"recording_id"`
	SourceAttachmentID string     `json:"source_attachment_id,omitempty"`
	TranscriptRevision string     `json:"transcript_revision,omitempty"`
	GlobalStartMS      *int64     `json:"global_start_ms,omitempty"`
	GlobalEndMS        *int64     `json:"global_end_ms,omitempty"`
	TopicIDs           []string   `json:"topic_ids"`
	PersonIDs          []string   `json:"person_ids"`
	Text               string     `json:"text"`
	TextTruncated      bool       `json:"text_truncated,omitempty"`
	StartedAt          *time.Time `json:"started_at,omitempty"`
	SourceState        string     `json:"source_state"`
	Provenance         string     `json:"provenance"`
}
type Gap struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}
type Coverage struct {
	EligibleTurns int `json:"eligible_turns"`
	SelectedTurns int `json:"selected_turns"`
	SelectedNotes int `json:"selected_notes"`
	OmittedTurns  int `json:"omitted_turns"`
	OmittedNotes  int `json:"omitted_notes"`
	OmittedItems  int `json:"omitted_items"`
}
type Result struct {
	ContractVersion string     `json:"contract_version"`
	SnapshotID      string     `json:"snapshot_id"`
	Scope           Scope      `json:"scope"`
	Sections        Sections   `json:"sections"`
	Evidence        []Evidence `json:"evidence"`
	Coverage        Coverage   `json:"coverage"`
	Gaps            []Gap      `json:"gaps"`
	Truncated       bool       `json:"truncated"`
	Degraded        bool       `json:"degraded"`
}

type rawTurn struct {
	id, conv, rec, speaker, text, attachment, revision string
	start, end                                         sql.NullInt64
	started                                            sql.NullString
	topics, people                                     []string
}

// Build reads one SQLite snapshot and applies scope as a hard filter. Person
// scopes require confirmed identity bindings; project scopes union explicit
// topic and conversation selectors.
func Build(ctx context.Context, store *memory.Store, req Request) (Result, error) {
	if store == nil {
		return Result{}, errors.New("context brief: store required")
	}
	req = canonicalRequest(req)
	if err := validate(req); err != nil {
		return Result{}, err
	}
	if req.MaxItems == 0 {
		req.MaxItems = 40
	}
	if req.MaxEvidence == 0 {
		req.MaxEvidence = 80
	}
	if req.MaxBytes == 0 {
		req.MaxBytes = 65536
	}
	if err := validateBudgets(req); err != nil {
		return Result{}, err
	}
	tx, err := store.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()
	var retired int
	err = tx.QueryRowContext(ctx, `SELECT retired FROM profile_control WHERE id=1`).Scan(&retired)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return Result{}, err
	}
	if retired != 0 {
		return Result{}, memory.ErrProfileRetired
	}
	filterSQL, filterArgs := scopeFilter(req.Scope)
	query := `SELECT t.id,t.conversation_id,t.recording_id,COALESCE(t.speaker_scoped_id,''),t.content,t.global_start_ms,t.global_end_ms,r.recording_started_at,COALESCE((SELECT a.id FROM attachment_turns at JOIN source_attachments a ON a.id=at.attachment_id WHERE at.turn_id=t.id AND a.active=1 ORDER BY a.id LIMIT 1),''),COALESCE((SELECT a.transcript_revision FROM attachment_turns at JOIN source_attachments a ON a.id=at.attachment_id WHERE at.turn_id=t.id AND a.active=1 ORDER BY a.id LIMIT 1),'') FROM turns t JOIN recordings r ON r.id=t.recording_id WHERE ` + memory.CurrentSpeechTurnPredicate("t") + ` AND ` + filterSQL + ` ORDER BY t.id LIMIT 5001`
	rows, err := tx.QueryContext(ctx, query, filterArgs...)
	if err != nil {
		return Result{}, err
	}
	var all []rawTurn
	for rows.Next() {
		var x rawTurn
		if err = rows.Scan(&x.id, &x.conv, &x.rec, &x.speaker, &x.text, &x.start, &x.end, &x.started, &x.attachment, &x.revision); err != nil {
			rows.Close()
			return Result{}, err
		}
		all = append(all, x)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return Result{}, err
	}
	rows.Close()
	sourceCapped := len(all) > 5000
	if sourceCapped {
		all = all[:5000]
	}
	// Resolve current topic membership and confirmed identities in this snapshot.
	for i := range all {
		rs, e := tx.QueryContext(ctx, `SELECT topic_id FROM turn_topics WHERE turn_id=? ORDER BY topic_id`, all[i].id)
		if e != nil {
			return Result{}, e
		}
		for rs.Next() {
			var v string
			if e = rs.Scan(&v); e != nil {
				rs.Close()
				return Result{}, e
			}
			all[i].topics = append(all[i].topics, v)
		}
		if e = rs.Err(); e != nil {
			rs.Close()
			return Result{}, e
		}
		rs.Close()
		if all[i].speaker != "" {
			rs, e = tx.QueryContext(ctx, `SELECT b.person_id FROM speaker_bindings b WHERE b.recording_id=? AND b.speaker_scoped_id=? AND b.status='confirmed' AND b.person_id IS NOT NULL AND b.transcript_revision=? AND EXISTS(SELECT 1 FROM turns t JOIN attachment_turns at ON at.turn_id=t.id JOIN source_attachments a ON a.id=at.attachment_id WHERE t.id=? AND t.recording_id=b.recording_id AND t.speaker_scoped_id=b.speaker_scoped_id AND a.active=1 AND a.transcript_revision=b.transcript_revision AND `+memory.CurrentTurnPredicate("t")+`) ORDER BY b.version DESC LIMIT 1`, all[i].rec, all[i].speaker, all[i].revision, all[i].id)
			if e != nil {
				return Result{}, e
			}
			for rs.Next() {
				var v string
				if e = rs.Scan(&v); e != nil {
					rs.Close()
					return Result{}, e
				}
				all[i].people = append(all[i].people, v)
			}
			if e = rs.Err(); e != nil {
				rs.Close()
				return Result{}, e
			}
			rs.Close()
		}
	}
	filtered := all[:0]
	unknownTimeExcluded := 0
	selectedByTurn := map[string]Evidence{}
	for _, x := range all {
		if !scopeMatches(req.Scope, x) {
			continue
		}
		var st *time.Time
		if x.started.Valid {
			if t, e := time.Parse(time.RFC3339Nano, x.started.String); e == nil {
				st = &t
			}
		}
		if (req.StartedAfter != nil || req.StartedBefore != nil) && st == nil {
			unknownTimeExcluded++
			continue
		}
		if (req.StartedAfter != nil || req.StartedBefore != nil) && (req.StartedAfter != nil && st.Before(*req.StartedAfter) || req.StartedBefore != nil && st.After(*req.StartedBefore)) {
			continue
		}
		filtered = append(filtered, x)
	}
	all = filtered
	res := Result{ContractVersion: "context-brief/0.1", Scope: req.Scope, Sections: Sections{[]Entry{}, []Entry{}, []Entry{}, []Entry{}, []Entry{}}, Evidence: []Evidence{}, Gaps: []Gap{}}
	if sourceCapped {
		res.Truncated = true
		res.Gaps = append(res.Gaps, Gap{"source_scan_cap", "source scan reached the deterministic 5000 turn safety cap"})
	}
	res.Coverage.EligibleTurns = len(all)
	if unknownTimeExcluded > 0 {
		res.Gaps = append(res.Gaps, Gap{"unknown_time_excluded", fmt.Sprintf("%d in-scope turns had unknown recording start time and were excluded by the requested bounds", unknownTimeExcluded)})
	}
	if len(all) > req.MaxEvidence {
		all = all[:req.MaxEvidence]
		res.Truncated = true
		res.Gaps = append(res.Gaps, Gap{"evidence_budget", "evidence item limit reached"})
	}
	for _, x := range all {
		started := parseDate(x.started)
		eid := "ev:" + shortHash(x.id)
		var startMS, endMS *int64
		if x.start.Valid {
			v := x.start.Int64
			startMS = &v
		}
		if x.end.Valid {
			v := x.end.Int64
			endMS = &v
		}
		ev := Evidence{ID: eid, TurnID: x.id, ConversationID: x.conv, RecordingID: x.rec, SourceAttachmentID: x.attachment, TranscriptRevision: x.revision, GlobalStartMS: startMS, GlobalEndMS: endMS, TopicIDs: x.topics, PersonIDs: x.people, Text: x.text, StartedAt: started, SourceState: "current", Provenance: "transcript_turn"}
		res.Evidence = append(res.Evidence, ev)
		selectedByTurn[x.id] = ev
		e := Entry{ID: "entry:" + shortHash(x.id), Text: x.text, EvidenceIDs: []string{eid}, SourceState: "current", AssertionState: "unverified", Provenance: "transcript_turn"}
		if strings.TrimSpace(x.text) == "" {
			res.Sections.Unknowns = append(res.Sections.Unknowns, e)
		} else {
			res.Sections.Background = append(res.Sections.Background, e)
		}
	}
	// Current user-authored follow-up/question highlights are surfaced only
	// when their exact source turn survived the same hard scope filters.
	selectedTurnIDs := sortedTurnIDs(selectedByTurn)
	inClause := placeholdersOrNull(len(selectedTurnIDs))
	inArgs := stringsToAny(selectedTurnIDs)
	hRows, err := tx.QueryContext(ctx, `SELECT h.id,h.tag,h.note_text,h.turn_id,h.recording_id,h.version FROM memory_highlights h JOIN source_attachments a ON a.id=h.source_attachment_id AND a.recording_id=h.recording_id AND a.conversation_id=h.conversation_id AND a.active=1 WHERE h.tag IN ('follow_up','question') AND h.turn_id IN (`+inClause+`) ORDER BY h.id`, inArgs...)
	if err == nil {
		for hRows.Next() {
			var id, tag, note, turnID, recordingID string
			var version int64
			if err = hRows.Scan(&id, &tag, &note, &turnID, &recordingID, &version); err != nil {
				hRows.Close()
				return Result{}, err
			}
			ev, ok := selectedByTurn[turnID]
			if !ok {
				continue
			}
			body := strings.TrimSpace(note)
			if body == "" {
				continue
			}
			entry := Entry{ID: "highlight:" + shortHash(id), Text: body, EvidenceIDs: []string{ev.ID}, SourceState: "current", AssertionState: "proposed", Provenance: "user_authored_highlight:" + tag, SourceVersion: strconv.FormatInt(version, 10)}
			res.Sections.FollowUpCandidates = append(res.Sections.FollowUpCandidates, entry)
			res.Coverage.SelectedNotes++
		}
		if err = hRows.Err(); err != nil {
			hRows.Close()
			return Result{}, err
		}
		hRows.Close()
	} else if !strings.Contains(err.Error(), "no such table") {
		return Result{}, err
	}
	// A current note is emitted only when every cited turn survived current
	// source filtering, ambient filtering, and the explicit request scope.
	nRows, nErr := tx.QueryContext(ctx, `SELECT n.id,n.conversation_id,n.kind,n.summary,n.annotation_version,n.topic_ids_json FROM notes n WHERE NOT `+memory.NoteStalePredicate("n")+` AND NOT EXISTS(SELECT 1 FROM note_evidence ne JOIN turn_classes tc ON tc.turn_id=ne.turn_id WHERE ne.note_id=n.id AND ne.ordinal=0 AND tc.class='ambient') AND EXISTS(SELECT 1 FROM note_evidence scope_e WHERE scope_e.note_id=n.id AND scope_e.turn_id IN (`+inClause+`)) ORDER BY n.id`, inArgs...)
	if nErr != nil {
		return Result{}, nErr
	}
	for nRows.Next() {
		var id, conv, kind, summary, version, topicJSON string
		if nErr = nRows.Scan(&id, &conv, &kind, &summary, &version, &topicJSON); nErr != nil {
			nRows.Close()
			return Result{}, nErr
		}
		rs, e := tx.QueryContext(ctx, `SELECT turn_id FROM note_evidence WHERE note_id=? ORDER BY ordinal,turn_id`, id)
		if e != nil {
			nRows.Close()
			return Result{}, e
		}
		var refs []string
		var evidenceTurns []string
		valid := true
		for rs.Next() {
			var tid string
			if e = rs.Scan(&tid); e != nil {
				rs.Close()
				nRows.Close()
				return Result{}, e
			}
			ev, ok := selectedByTurn[tid]
			if !ok {
				valid = false
				continue
			}
			refs = append(refs, ev.ID)
			evidenceTurns = append(evidenceTurns, tid)
		}
		if e = rs.Err(); e != nil {
			rs.Close()
			nRows.Close()
			return Result{}, e
		}
		rs.Close()
		if !valid || len(refs) == 0 {
			continue
		}
		var topicIDs []string
		if e := json.Unmarshal([]byte(topicJSON), &topicIDs); e != nil {
			nRows.Close()
			return Result{}, e
		}
		var primaryStart *int64
		if len(evidenceTurns) > 0 {
			if ev, ok := selectedByTurn[evidenceTurns[0]]; ok {
				primaryStart = ev.GlobalStartMS
			}
		}
		projectedNote, projected := projection.ProjectNote(projection.NoteInput{ID: id, ConversationID: conv, Kind: kind, Summary: summary, TopicIDs: topicIDs, EvidenceTurnIDs: evidenceTurns, PrimaryStartMS: primaryStart})
		entry := Entry{ID: "note:" + shortHash(id), Text: summary, EvidenceIDs: refs, SourceState: "current", AssertionState: "unverified", Provenance: "inferred_note:" + kind, SourceVersion: version}
		if projected && projectedNote.Status == projection.StatusProposed {
			entry.AssertionState = "proposed"
		}
		switch kind {
		case "request", "intention", "constraint":
			if projected {
				res.Sections.FollowUpCandidates = append(res.Sections.FollowUpCandidates, entry)
			} else {
				res.Sections.Unknowns = append(res.Sections.Unknowns, entry)
			}
		case "observation":
			if projected {
				res.Sections.PreviousDiscussions = append(res.Sections.PreviousDiscussions, entry)
			} else {
				res.Sections.Unknowns = append(res.Sections.Unknowns, entry)
			}
		case "suggestion":
			res.Sections.Unknowns = append(res.Sections.Unknowns, entry)
		default:
			res.Sections.Unknowns = append(res.Sections.Unknowns, entry)
		}
		res.Coverage.SelectedNotes++
	}
	if nErr = nRows.Err(); nErr != nil {
		nRows.Close()
		return Result{}, nErr
	}
	nRows.Close()
	// Existing contradiction links are retained as hints, with no attempt to
	// decide which side is true. Rejected links are excluded.
	lRows, lErr := tx.QueryContext(ctx, `SELECT id,reason,evidence_turn_ids_json,status FROM memory_links WHERE relation='contradiction' AND status<>'rejected' AND EXISTS(SELECT 1 FROM json_each(memory_links.evidence_turn_ids_json) j WHERE j.value IN (`+inClause+`)) ORDER BY id`, inArgs...)
	if lErr == nil {
		for lRows.Next() {
			var id, reason, raw, status string
			if lErr = lRows.Scan(&id, &reason, &raw, &status); lErr != nil {
				lRows.Close()
				return Result{}, lErr
			}
			var turnIDs []string
			if lErr = json.Unmarshal([]byte(raw), &turnIDs); lErr != nil {
				lRows.Close()
				return Result{}, lErr
			}
			refs := []string{}
			valid := len(turnIDs) > 0
			for _, tid := range turnIDs {
				ev, ok := selectedByTurn[tid]
				if !ok {
					valid = false
					break
				}
				refs = append(refs, ev.ID)
			}
			if !valid {
				continue
			}
			txt := strings.TrimSpace(reason)
			if txt == "" {
				txt = "Stored contradiction hint; both sides remain unresolved."
			}
			res.Sections.ChangesConflicts = append(res.Sections.ChangesConflicts, Entry{ID: "link:" + shortHash(id), Text: txt, EvidenceIDs: refs, SourceState: "current", AssertionState: "unverified", Provenance: "existing_conflict_hint:" + status})
			res.Coverage.SelectedNotes++
		}
		if lErr = lRows.Err(); lErr != nil {
			lRows.Close()
			return Result{}, lErr
		}
		lRows.Close()
	} else if !strings.Contains(lErr.Error(), "no such table") {
		return Result{}, lErr
	}
	res.Coverage.SelectedTurns = len(res.Evidence)
	res.Coverage.OmittedTurns = res.Coverage.EligibleTurns - res.Coverage.SelectedTurns
	if res.Coverage.OmittedTurns > 0 {
		res.Truncated = true
		res.Gaps = append(res.Gaps, Gap{"scope_or_budget", "some eligible turns were omitted"})
	}
	if omitted, omittedNotes := trimByItems(&res.Sections, req.MaxItems); omitted > 0 {
		res.Truncated = true
		res.Coverage.OmittedItems += omitted
		res.Coverage.OmittedNotes += omittedNotes
	}
	res.Evidence = pruneEvidence(res.Evidence, res.Sections)
	for {
		res.Coverage.SelectedTurns = len(res.Evidence)
		res.Coverage.OmittedTurns = res.Coverage.EligibleTurns - res.Coverage.SelectedTurns
		res.Coverage.SelectedNotes = countNotes(res.Sections)
		addEmptySectionGaps(&res)
		res.SnapshotID = ""
		h := sha256.Sum256(mustJSON(struct {
			Template string  `json:"template"`
			Request  Request `json:"request"`
			Result   Result  `json:"result"`
		}{"context-brief/0.1", req, res}))
		res.SnapshotID = hex.EncodeToString(h[:])
		if len(mustJSON(res))+1 <= req.MaxBytes {
			return res, nil
		}
		res.Truncated = true
		res.Gaps = appendUniqueGap(res.Gaps, Gap{"byte_budget", "output limited by byte budget"})
		if shortenLongestText(&res) {
			continue
		}
		if hasEntries(res.Sections) {
			wasNote := popEntry(&res.Sections)
			res.Coverage.OmittedItems++
			if wasNote > 0 {
				res.Coverage.OmittedNotes++
			}
			res.Evidence = pruneEvidence(res.Evidence, res.Sections)
			continue
		}
		if len(res.Evidence) > 0 {
			res.Evidence = res.Evidence[:len(res.Evidence)-1]
			continue
		}
		return Result{}, fmt.Errorf("context brief: max_bytes too small for response skeleton")
	}
}

func entriesWithEvidence(entries []Entry, evidence []Evidence) []Entry {
	ids := map[string]bool{}
	for _, e := range evidence {
		ids[e.ID] = true
	}
	out := entries[:0]
	for _, e := range entries {
		valid := len(e.EvidenceIDs) > 0
		for _, id := range e.EvidenceIDs {
			valid = valid && ids[id]
		}
		if valid {
			out = append(out, e)
		}
	}
	return out
}

func sortedTurnIDs(m map[string]Evidence) []string {
	out := make([]string, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
func placeholdersOrNull(n int) string {
	if n == 0 {
		return "NULL"
	}
	return placeholders(n)
}
func stringsToAny(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func shortenLongestText(r *Result) bool {
	type candidate struct {
		text      *string
		truncated *bool
	}
	cs := []candidate{}
	for i := range r.Evidence {
		cs = append(cs, candidate{&r.Evidence[i].Text, &r.Evidence[i].TextTruncated})
	}
	for _, p := range []*[]Entry{&r.Sections.Background, &r.Sections.PreviousDiscussions, &r.Sections.FollowUpCandidates, &r.Sections.ChangesConflicts, &r.Sections.Unknowns} {
		for i := range *p {
			cs = append(cs, candidate{&(*p)[i].Text, &(*p)[i].TextTruncated})
		}
	}
	best := -1
	for i, c := range cs {
		if best < 0 || len(*c.text) > len(*cs[best].text) {
			best = i
		}
	}
	if best < 0 || len(*cs[best].text) <= 128 {
		return false
	}
	c := cs[best]
	*c.text = truncateUTF8(*c.text, len(*c.text)/2)
	*c.truncated = true
	return true
}
func truncateUTF8(s string, target int) string {
	for len(s) > target {
		_, size := utf8.DecodeLastRuneInString(s)
		if size < 1 {
			break
		}
		s = s[:len(s)-size]
	}
	return strings.TrimSpace(s)
}

func trimByItems(s *Sections, max int) (int, int) {
	sections := []*[]Entry{&s.Background, &s.PreviousDiscussions, &s.FollowUpCandidates, &s.ChangesConflicts, &s.Unknowns}
	remaining := max
	omitted, notes := 0, 0
	for _, p := range sections {
		if remaining <= 0 {
			for _, e := range *p {
				omitted++
				if e.Provenance != "transcript_turn" {
					notes++
				}
			}
			*p = []Entry{}
			continue
		}
		if len(*p) > remaining {
			for _, e := range (*p)[remaining:] {
				omitted++
				if e.Provenance != "transcript_turn" {
					notes++
				}
			}
			*p = (*p)[:remaining]
		}
		remaining -= len(*p)
	}
	return omitted, notes
}
func popEntry(s *Sections) int {
	for _, p := range []*[]Entry{&s.Unknowns, &s.ChangesConflicts, &s.FollowUpCandidates, &s.PreviousDiscussions, &s.Background} {
		if len(*p) > 0 {
			e := (*p)[len(*p)-1]
			*p = (*p)[:len(*p)-1]
			if e.Provenance != "transcript_turn" {
				return 1
			}
			return 0
		}
	}
	return 0
}
func hasEntries(s Sections) bool {
	return len(s.Background)+len(s.PreviousDiscussions)+len(s.FollowUpCandidates)+len(s.ChangesConflicts)+len(s.Unknowns) > 0
}
func countNotes(s Sections) int {
	n := 0
	for _, a := range [][]Entry{s.Background, s.PreviousDiscussions, s.FollowUpCandidates, s.ChangesConflicts, s.Unknowns} {
		for _, e := range a {
			if e.Provenance != "transcript_turn" {
				n++
			}
		}
	}
	return n
}
func addEmptySectionGaps(r *Result) {
	for _, x := range []struct {
		name  string
		items []Entry
	}{{"background", r.Sections.Background}, {"previous_discussions", r.Sections.PreviousDiscussions}, {"follow_up_candidates", r.Sections.FollowUpCandidates}, {"changes_conflicts", r.Sections.ChangesConflicts}, {"unknowns", r.Sections.Unknowns}} {
		if len(x.items) == 0 {
			r.Gaps = appendUniqueGap(r.Gaps, Gap{"empty_section", x.name + " has no current in-scope evidence"})
		}
	}
}
func validate(r Request) error {
	if r.Scope.Kind != "person" && r.Scope.Kind != "project" && r.Scope.Kind != "topic" && r.Scope.Kind != "conversation" {
		return errors.New("context brief: explicit scope kind required")
	}
	switch r.Scope.Kind {
	case "person":
		if len(r.Scope.TopicIDs)+len(r.Scope.ConversationIDs) > 0 {
			return errors.New("person scope only accepts person_ids")
		}
	case "topic":
		if len(r.Scope.TopicIDs) == 0 || len(r.Scope.PersonIDs)+len(r.Scope.ConversationIDs) > 0 {
			return errors.New("topic scope only accepts topic_ids")
		}
	case "conversation":
		if len(r.Scope.ConversationIDs) == 0 || len(r.Scope.PersonIDs)+len(r.Scope.TopicIDs) > 0 {
			return errors.New("conversation scope only accepts conversation_ids")
		}
	}
	n := len(r.Scope.PersonIDs) + len(r.Scope.TopicIDs) + len(r.Scope.ConversationIDs)
	if n == 0 {
		return errors.New("context brief: explicit scope ids required")
	}
	if r.Scope.Kind == "person" && len(r.Scope.PersonIDs) == 0 || r.Scope.Kind == "project" && (len(r.Scope.TopicIDs)+len(r.Scope.ConversationIDs) == 0 || len(r.Scope.PersonIDs) > 0) {
		return errors.New("context brief: scope ids do not match kind")
	}
	if r.StartedAfter != nil && r.StartedBefore != nil && r.StartedAfter.After(*r.StartedBefore) {
		return errors.New("context brief: invalid time range")
	}
	for _, ids := range [][]string{r.Scope.PersonIDs, r.Scope.TopicIDs, r.Scope.ConversationIDs} {
		if len(ids) > 100 {
			return errors.New("context brief: at most 100 ids per scope selector")
		}
		for _, id := range ids {
			if strings.TrimSpace(id) == "" || len(id) > 256 {
				return errors.New("context brief: invalid scope id")
			}
		}
	}
	return nil
}
func scopeMatches(s Scope, x rawTurn) bool {
	conv := contains(s.ConversationIDs, x.conv)
	topic := intersects(s.TopicIDs, x.topics)
	person := intersects(s.PersonIDs, x.people)
	if len(s.PersonIDs) > 0 && !person {
		return false
	}
	if s.Kind == "person" {
		return person
	}
	if s.Kind == "topic" {
		return topic
	}
	if s.Kind == "conversation" {
		return conv
	}
	if s.Kind == "project" && (len(s.TopicIDs) > 0 || len(s.ConversationIDs) > 0) {
		return (len(s.TopicIDs) > 0 && topic) || (len(s.ConversationIDs) > 0 && conv)
	}
	return false
}

func validateBudgets(r Request) error {
	if r.MaxItems < 1 || r.MaxItems > 200 || r.MaxEvidence < 1 || r.MaxEvidence > 400 || r.MaxBytes < 256 || r.MaxBytes > 262144 {
		return errors.New("context brief: budgets exceed supported limits")
	}
	return nil
}
func canonicalRequest(r Request) Request {
	r.Scope.PersonIDs = sortedUnique(r.Scope.PersonIDs)
	r.Scope.TopicIDs = sortedUnique(r.Scope.TopicIDs)
	r.Scope.ConversationIDs = sortedUnique(r.Scope.ConversationIDs)
	if r.StartedAfter != nil {
		t := r.StartedAfter.UTC()
		r.StartedAfter = &t
	}
	if r.StartedBefore != nil {
		t := r.StartedBefore.UTC()
		r.StartedBefore = &t
	}
	return r
}
func sortedUnique(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	n := 0
	for _, v := range out {
		if n == 0 || out[n-1] != v {
			out[n] = v
			n++
		}
	}
	return out[:n]
}
func scopeFilter(s Scope) (string, []any) {
	clauses := []string{}
	args := []any{}
	if s.Kind == "person" {
		clauses = append(clauses, `EXISTS(SELECT 1 FROM speaker_bindings b JOIN attachment_turns at ON at.turn_id=t.id JOIN source_attachments a ON a.id=at.attachment_id WHERE b.recording_id=t.recording_id AND b.speaker_scoped_id=t.speaker_scoped_id AND b.status='confirmed' AND b.person_id IS NOT NULL AND b.person_id<>'' AND b.version=(SELECT MAX(b2.version) FROM speaker_bindings b2 WHERE b2.recording_id=b.recording_id AND b2.speaker_scoped_id=b.speaker_scoped_id AND b2.transcript_revision=b.transcript_revision AND b2.status='confirmed' AND b2.person_id IS NOT NULL AND b2.person_id<>'') AND b.person_id IN (`+placeholders(len(s.PersonIDs))+`) AND a.active=1 AND a.transcript_revision=b.transcript_revision)`)
		for _, x := range s.PersonIDs {
			args = append(args, x)
		}
		return strings.Join(clauses, " AND "), args
	}
	union := []string{}
	if len(s.TopicIDs) > 0 {
		union = append(union, `EXISTS(SELECT 1 FROM turn_topics tt WHERE tt.turn_id=t.id AND tt.topic_id IN (`+placeholders(len(s.TopicIDs))+`))`)
		for _, x := range s.TopicIDs {
			args = append(args, x)
		}
	}
	if len(s.ConversationIDs) > 0 {
		union = append(union, `t.conversation_id IN (`+placeholders(len(s.ConversationIDs))+`)`)
		for _, x := range s.ConversationIDs {
			args = append(args, x)
		}
	}
	return "(" + strings.Join(union, " OR ") + ")", args
}
func placeholders(n int) string {
	p := make([]string, n)
	for i := range p {
		p[i] = "?"
	}
	return strings.Join(p, ",")
}
func contains(a []string, x string) bool {
	for _, v := range a {
		if v == x {
			return true
		}
	}
	return false
}
func intersects(a, b []string) bool {
	for _, x := range a {
		if contains(b, x) {
			return true
		}
	}
	return false
}
func parseDate(s sql.NullString) *time.Time {
	if !s.Valid {
		return nil
	}
	t, e := time.Parse(time.RFC3339Nano, s.String)
	if e != nil {
		return nil
	}
	return &t
}
func shortHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:10]) }
func mustJSON(v any) []byte     { b, _ := json.Marshal(v); return b }
func appendUniqueGap(a []Gap, g Gap) []Gap {
	for _, x := range a {
		if x.Kind == g.Kind && x.Detail == g.Detail {
			return a
		}
	}
	return append(a, g)
}
func pruneEvidence(e []Evidence, s Sections) []Evidence {
	used := map[string]bool{}
	for _, a := range append(append(append(append(s.Background, s.PreviousDiscussions...), s.FollowUpCandidates...), s.ChangesConflicts...), s.Unknowns...) {
		for _, id := range a.EvidenceIDs {
			used[id] = true
		}
	}
	out := e[:0]
	for _, x := range e {
		if used[x.ID] {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TurnID < out[j].TurnID })
	return out
}
