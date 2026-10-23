package app

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"recordingmcp/internal/plane"
)

// WorldlineThreadList is a read-only projection grouped by exact normalized
// source topic title. It is intentionally separate from editable macro topics.
type WorldlineThreadList struct {
	Version                  string            `json:"version"`
	Total                    int               `json:"total"`
	CrossConversationThreads int               `json:"cross_conversation_threads"`
	Next                     *int              `json:"next_offset,omitempty"`
	Range                    WorldlineRange    `json:"range"`
	Threads                  []WorldlineThread `json:"threads"`
}

type WorldlineThread struct {
	ID                  string     `json:"id"`
	ManualGroup         bool       `json:"manual_group,omitempty"`
	Title               string     `json:"title"`
	MacroTopicID        string     `json:"macro_topic_id"`
	MacroTopicTitle     string     `json:"macro_topic_title"`
	SourceTopicIDs      []string   `json:"source_topic_ids"`
	ConversationCount   int        `json:"conversation_count"`
	EntryCount          int        `json:"entry_count"`
	DatedEntryCount     int        `json:"dated_entry_count"`
	UnknownEntryCount   int        `json:"unknown_entry_count"`
	SemanticReviewCount int        `json:"semantic_review_count"`
	LastMentionedAt     *time.Time `json:"last_mentioned_at,omitempty"`
	FirstMentionedAt    *time.Time `json:"first_mentioned_at,omitempty"`
}

type WorldlineThreadEntry struct {
	ID                 string                       `json:"id"`
	ClaimID            string                       `json:"claim_id"`
	Version            string                       `json:"version"`
	Summary            string                       `json:"summary"`
	Relation           string                       `json:"relation"`
	RelationReason     string                       `json:"relation_reason"`
	MentionedAt        *time.Time                   `json:"mentioned_at,omitempty"`
	TimeKnown          bool                         `json:"time_known"`
	Status             string                       `json:"status"`
	Category           string                       `json:"category"`
	MacroTopicID       string                       `json:"macro_topic_id"`
	LaneID             string                       `json:"lane_id"`
	ConversationIDs    []string                     `json:"conversation_ids"`
	Evidence           []WorldlineEvidence          `json:"evidence"`
	Claim              WorldlineClaim               `json:"claim"`
	SemanticSuggestion *WorldlineSemanticSuggestion `json:"semantic_suggestion,omitempty"`
	SubjectKey         string                       `json:"-"`
}

type WorldlineThreadDetail struct {
	Version             string                 `json:"version"`
	Thread              WorldlineThread        `json:"thread"`
	Entries             []WorldlineThreadEntry `json:"entries"`
	UnknownEntries      []WorldlineThreadEntry `json:"unknown_entries"`
	EntriesTotal        int                    `json:"entries_total"`
	UnknownEntriesTotal int                    `json:"unknown_entries_total"`
	EntriesNextOffset   *int                   `json:"entries_next_offset,omitempty"`
	UnknownNextOffset   *int                   `json:"unknown_entries_next_offset,omitempty"`
}

func (s *WorldlineService) Threads(ctx context.Context, scope string, q WorldlineQuery) (WorldlineThreadList, error) {
	if s == nil || s.Time == nil || strings.TrimSpace(scope) == "" {
		return WorldlineThreadList{}, errors.New("worldline service and profile scope required")
	}
	if q.Offset < 0 || q.Limit < 1 || q.Limit > 100 {
		return WorldlineThreadList{}, errors.New("offset must be non-negative and limit must be between 1 and 100")
	}
	if q.Status == "" {
		q.Status = "all"
	}
	if q.Status != "all" && q.Status != "unconfirmed" && q.Status != "reviewed" && q.Status != "conflict" {
		return WorldlineThreadList{}, errors.New("invalid status")
	}
	if q.From != nil && q.To != nil && q.From.After(*q.To) {
		return WorldlineThreadList{}, errors.New("from must not follow to")
	}
	topics, _, version, err := s.project(ctx, scope)
	if err != nil {
		return WorldlineThreadList{}, err
	}
	built := buildWorldlineThreadDetails(topics)
	if err := s.applyThreadAliases(ctx, scope, built); err != nil {
		return WorldlineThreadList{}, err
	}
	built = activeThreadBuilds(built)
	if err := s.applySemanticSuggestions(ctx, scope, built); err != nil {
		return WorldlineThreadList{}, err
	}
	built = filterThreadBuilds(built, q)
	threads := make([]WorldlineThread, 0, len(built))
	for _, b := range built {
		threads = append(threads, b.thread)
	}
	filtered := threads
	sortWorldlineThreads(filtered)
	result := WorldlineThreadList{Version: version, Total: len(filtered), Range: threadMentionRange(filtered), Threads: []WorldlineThread{}}
	for _, thread := range filtered {
		if thread.ConversationCount > 1 {
			result.CrossConversationThreads++
		}
	}
	if q.Offset >= len(filtered) {
		return result, nil
	}
	end := q.Offset + q.Limit
	if end > len(filtered) {
		end = len(filtered)
	}
	result.Threads = filtered[q.Offset:end]
	if end < len(filtered) {
		result.Next = &end
	}
	return result, nil
}

func sortWorldlineThreads(filtered []WorldlineThread) {
	sort.Slice(filtered, func(i, j int) bool {
		a, b := filtered[i], filtered[j]
		if a.SemanticReviewCount != b.SemanticReviewCount {
			return a.SemanticReviewCount > b.SemanticReviewCount
		}
		if a.ConversationCount != b.ConversationCount {
			return a.ConversationCount > b.ConversationCount
		}
		if a.LastMentionedAt == nil && b.LastMentionedAt != nil {
			return false
		}
		if a.LastMentionedAt != nil && b.LastMentionedAt == nil {
			return true
		}
		if a.LastMentionedAt != nil && b.LastMentionedAt != nil && !a.LastMentionedAt.Equal(*b.LastMentionedAt) {
			return a.LastMentionedAt.After(*b.LastMentionedAt)
		}
		if normalizeTopicTitle(a.Title) != normalizeTopicTitle(b.Title) {
			return normalizeTopicTitle(a.Title) < normalizeTopicTitle(b.Title)
		}
		return a.ID < b.ID
	})
}

func (s *WorldlineService) Thread(ctx context.Context, scope, id string, q WorldlineQuery, entriesOffset, unknownOffset, limit int) (WorldlineThreadDetail, error) {
	if s == nil || s.Time == nil || strings.TrimSpace(scope) == "" {
		return WorldlineThreadDetail{}, errors.New("worldline service and profile scope required")
	}
	if entriesOffset < 0 || unknownOffset < 0 || limit < 1 || limit > 500 {
		return WorldlineThreadDetail{}, errors.New("offsets must be non-negative and limit must be between 1 and 500")
	}
	topics, _, version, err := s.project(ctx, scope)
	if err != nil {
		return WorldlineThreadDetail{}, err
	}
	if q.Status == "" {
		q.Status = "all"
	}
	built := buildWorldlineThreadDetails(topics)
	if err := s.applyThreadAliases(ctx, scope, built); err != nil {
		return WorldlineThreadDetail{}, err
	}
	built = activeThreadBuilds(built)
	if err := s.applySemanticSuggestions(ctx, scope, built); err != nil {
		return WorldlineThreadDetail{}, err
	}
	built = filterThreadBuilds(built, q)
	for _, thread := range built {
		if thread.thread.ID != id && !thread.legacyIDs[id] {
			continue
		}
		result := WorldlineThreadDetail{Version: version, Thread: thread.thread, EntriesTotal: len(thread.entries), UnknownEntriesTotal: len(thread.unknown), Entries: pageThreadEntries(thread.entries, entriesOffset, limit), UnknownEntries: pageThreadEntries(thread.unknown, unknownOffset, limit)}
		if next := entriesOffset + limit; next < len(thread.entries) {
			result.EntriesNextOffset = &next
		}
		if next := unknownOffset + limit; next < len(thread.unknown) {
			result.UnknownNextOffset = &next
		}
		return result, nil
	}
	return WorldlineThreadDetail{}, errors.New("worldline thread not found")
}

// applyThreadAliases overlays source-backed, reversible display names and
// manual groups. Grouping only combines thread projections; claims and people
// remain source records and legacy IDs stay resolvable through legacyIDs.
func (s *WorldlineService) applyThreadAliases(ctx context.Context, scope string, built []threadBuild) error {
	events, err := s.Time.History(ctx, scope)
	if err != nil {
		return err
	}
	byID := map[string]*threadBuild{}
	originalTitles := map[string]string{}
	sources, err := s.readSources(ctx)
	if err != nil {
		return err
	}
	currentNotes := map[string]string{}
	for _, source := range sources {
		if id, ok := source.claim.Metadata["source_note_id"].(string); ok {
			currentNotes[id] = source.claim.Version
		}
	}
	for i := range built {
		if built[i].legacyIDs == nil {
			built[i].legacyIDs = map[string]bool{built[i].thread.ID: true}
		}
		built[i].legacyIDs[built[i].thread.ID] = true
		byID[built[i].thread.ID] = &built[i]
		originalTitles[built[i].thread.ID] = built[i].thread.Title
		for legacy := range built[i].legacyIDs {
			byID[legacy] = &built[i]
			originalTitles[legacy] = built[i].thread.Title
		}
	}
	for _, ev := range events {
		if ev.Type != "worldline.thread_group" || ev.SourceStatus != "current" || !feedbackSourcesCurrent(ev, currentNotes) {
			continue
		}
		targets := stringMap(ev.StructuredData["legacy_thread_targets"])
		for legacy, target := range targets {
			if b := byID[target]; b != nil {
				byID[legacy] = b
				b.legacyIDs[legacy] = true
				originalTitles[legacy] = b.thread.Title
			}
		}
	}
	latestGroups := map[string]plane.Event{}
	for _, ev := range events {
		if ev.Type == "worldline.thread_group" && ev.SourceStatus == "current" && feedbackSourcesCurrent(ev, currentNotes) {
			gid, _ := ev.StructuredData["group_id"].(string)
			if gid != "" {
				prior, ok := latestGroups[gid]
				if !ok || eventRecordedAfter(ev, prior) {
					latestGroups[gid] = ev
				}
			}
		}
	}
	groupIDs := make([]string, 0, len(latestGroups))
	for gid := range latestGroups {
		groupIDs = append(groupIDs, gid)
	}
	activeGroupIDs := map[string]bool{}
	for gid, ev := range latestGroups {
		action, _ := ev.StructuredData["action"].(string)
		if ev.SourceStatus == "current" && feedbackSourcesCurrent(ev, currentNotes) && action == "group" {
			activeGroupIDs[gid] = true
		}
	}
	sort.Slice(groupIDs, func(i, j int) bool {
		a, b := latestGroups[groupIDs[i]], latestGroups[groupIDs[j]]
		if eventRecordedAfter(a, b) {
			return false
		}
		if eventRecordedAfter(b, a) {
			return true
		}
		return groupIDs[i] < groupIDs[j]
	})
	for _, gid := range groupIDs {
		ev := latestGroups[gid]
		if ev.SourceStatus != "current" {
			continue
		}
		action, _ := ev.StructuredData["action"].(string)
		if action != "group" {
			continue
		}
		ids := stringSlice(ev.StructuredData["thread_ids"])
		var base *threadBuild
		for _, id := range ids {
			b := byID[id]
			if b == nil {
				continue
			}
			if base == nil {
				base = b
				continue
			}
			if b == base || b.suppressed {
				continue
			}
			for _, entry := range b.entries {
				if !base.seen[entry.ID] {
					base.entries = append(base.entries, entry)
					base.seen[entry.ID] = true
				}
			}
			for _, entry := range b.unknown {
				if !base.seen[entry.ID] {
					base.unknown = append(base.unknown, entry)
					base.seen[entry.ID] = true
				}
			}
			for id := range b.titles {
				base.titles[id] = true
			}
			for conv := range b.conversations {
				base.conversations[conv] = true
			}
			for old := range b.legacyIDs {
				base.legacyIDs[old] = true
				byID[old] = base
			}
			b.suppressed = true
		}
		if base == nil {
			continue
		}
		oldID := base.thread.ID
		base.legacyIDs[oldID] = true
		base.legacyIDs[gid] = true
		byID[gid] = base
		title, _ := ev.StructuredData["title"].(string)
		base.thread.ID = gid
		base.thread.ManualGroup = true
		base.groupedAt = worldlineEventTime(ev)
		if strings.TrimSpace(title) != "" {
			base.thread.Title = title
		}
		base.thread.SourceTopicIDs = base.thread.SourceTopicIDs[:0]
		for sourceID := range base.titles {
			base.thread.SourceTopicIDs = append(base.thread.SourceTopicIDs, sourceID)
		}
		sort.Strings(base.thread.SourceTopicIDs)
		originalTitles[gid] = base.thread.Title
	}
	for _, ev := range events {
		if ev.Type != "worldline.thread_alias" || ev.SourceStatus != "current" || !feedbackSourcesCurrent(ev, currentNotes) {
			continue
		}
		id, _ := ev.StructuredData["thread_id"].(string)
		title, _ := ev.StructuredData["title"].(string)
		if strings.HasPrefix(id, "thread-group-") && !activeGroupIDs[id] {
			continue
		}
		if b := byID[id]; b != nil {
			for _, legacy := range stringSlice(ev.StructuredData["legacy_thread_ids"]) {
				b.legacyIDs[legacy] = true
				byID[legacy] = b
				if _, ok := originalTitles[legacy]; !ok {
					originalTitles[legacy] = b.thread.Title
				}
			}
		}
		if b := byID[id]; b != nil {
			if !b.groupedAt.IsZero() && worldlineEventTime(ev).Before(b.groupedAt) && id != b.thread.ID {
				continue
			}
			if strings.TrimSpace(title) == "" {
				b.thread.Title = originalTitles[id]
			} else {
				b.thread.Title = title
			}
		}
	}
	return nil
}

// RenameThread appends a reversible alias event. Facts, source topic IDs and
// speaker identities remain untouched; an empty title restores the source name.
func (s *WorldlineService) RenameThread(ctx context.Context, scope, id, title, expectedVersion, requestID string) (string, error) {
	if strings.TrimSpace(scope) == "" || strings.TrimSpace(requestID) == "" {
		return "", errors.New("profile scope and request_id required")
	}
	if len([]rune(strings.TrimSpace(title))) > 120 {
		return "", errors.New("title must be at most 120 characters")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	events, err := s.Time.History(ctx, scope)
	if err != nil {
		return "", err
	}
	payload, _ := json.Marshal(struct{ ID, Title string }{id, strings.TrimSpace(title)})
	eventID := "wl-alias-" + shortHash(scope+"\x00"+requestID)
	for _, e := range events {
		if e.ID == eventID {
			if e.StructuredData["request_payload_hash"] == shortHash(string(payload)) {
				return worldlineVersionFromEvents(s, ctx, scope), nil
			}
			return "", errors.New("request_id conflict: existing rename payload differs")
		}
	}
	topics, _, currentVersion, err := s.project(ctx, scope)
	if err != nil {
		return "", err
	}
	if currentVersion != expectedVersion {
		return currentVersion, errors.New("worldline version conflict")
	}
	built := buildWorldlineThreadDetails(topics)
	if err := s.applyThreadAliases(ctx, scope, built); err != nil {
		return "", err
	}
	var target *threadBuild
	for i := range built {
		if built[i].thread.ID == id {
			target = &built[i]
			break
		}
	}
	if target == nil {
		return currentVersion, errors.New("worldline thread not found")
	}
	refs := []EvidenceReference{}
	noteIDs := []string{}
	noteVersions := map[string]string{}
	conversations := map[string]bool{}
	for _, entry := range append(append([]WorldlineThreadEntry{}, target.entries...), target.unknown...) {
		refs = append(refs, evidenceRefs(entry.Evidence)...)
		for _, conversation := range entry.ConversationIDs {
			conversations[conversation] = true
		}
		if nid, ok := entry.Claim.Metadata["source_note_id"].(string); ok {
			noteIDs = append(noteIDs, nid)
			v, _ := entry.Claim.Metadata["source_note_version"].(string)
			noteVersions[nid] = v
		}
	}
	refs = uniqueEvidenceRefs(refs)
	noteIDs = uniqueSorted(noteIDs)
	if len(refs) == 0 {
		return currentVersion, errors.New("rename requires current source evidence")
	}
	now := time.Now().UTC()
	legacyIDs := make([]string, 0, len(target.legacyIDs))
	for old := range target.legacyIDs {
		if old != id {
			legacyIDs = append(legacyIDs, old)
		}
	}
	sort.Strings(legacyIDs)
	data := map[string]any{"scope": scope, "origin": "worldline", "thread_id": id, "title": strings.TrimSpace(title), "request_id": requestID, "legacy_thread_ids": legacyIDs, "source_topic_ids": target.thread.SourceTopicIDs, "request_payload_hash": shortHash(string(payload)), "source_note_ids": noteIDs, "source_note_versions": noteVersions, "references": evidenceStrings(refs)}
	conversationList := make([]string, 0, len(conversations))
	for c := range conversations {
		conversationList = append(conversationList, c)
	}
	sort.Strings(conversationList)
	ev := plane.Event{ID: eventID, Type: "worldline.thread_alias", Timestamp: now, ObservedAt: now, RecordedAt: &now, Source: "local-web", Content: "worldline thread alias", Confidence: 1, StructuredData: data}
	if len(conversationList) == 1 {
		ev.ConversationID = conversationList[0]
	}
	if err = s.appendWorldlineMutation(ctx, scope, expectedVersion, shortHash(string(payload)), ev); err != nil {
		return "", err
	}
	_, _, v, err := s.project(ctx, scope)
	return v, err
}

func worldlineVersionFromEvents(s *WorldlineService, ctx context.Context, scope string) string {
	_, _, v, _ := s.project(ctx, scope)
	return v
}

// GroupThreads manually groups topical projections under a stable ID. The
// source claims and speaker identities are never rewritten or merged.
func (s *WorldlineService) GroupThreads(ctx context.Context, scope string, threadIDs []string, title, action, groupID, expectedVersion, requestID string) (string, error) {
	if s == nil || s.Time == nil || strings.TrimSpace(scope) == "" || strings.TrimSpace(requestID) == "" {
		return "", errors.New("worldline service, profile scope, and request_id required")
	}
	if action != "group" && action != "revoke" {
		return "", errors.New("group action must be group or revoke")
	}
	if action == "group" && (len(uniqueSorted(threadIDs)) < 2 || strings.TrimSpace(title) == "" || len([]rune(strings.TrimSpace(title))) > 120) {
		return "", errors.New("group requires at least two threads and a title of at most 120 characters")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	events, err := s.Time.History(ctx, scope)
	if err != nil {
		return "", err
	}
	payload, _ := json.Marshal(struct {
		Action         string
		ThreadIDs      []string
		Title, GroupID string
	}{action, threadIDs, strings.TrimSpace(title), groupID})
	eventID := "wl-group-" + shortHash(scope+"\x00"+requestID)
	for _, ev := range events {
		if ev.ID == eventID {
			if ev.StructuredData["request_payload_hash"] == shortHash(string(payload)) {
				return worldlineVersionFromEvents(s, ctx, scope), nil
			}
			return "", errors.New("request_id conflict: existing group payload differs")
		}
	}
	topics, _, version, err := s.project(ctx, scope)
	if err != nil {
		return "", err
	}
	if version != expectedVersion {
		return version, errors.New("worldline version conflict")
	}
	built := buildWorldlineThreadDetails(topics)
	if err := s.applyThreadAliases(ctx, scope, built); err != nil {
		return "", err
	}
	built = activeThreadBuilds(built)
	refs := []EvidenceReference{}
	notes := map[string]string{}
	convs := map[string]bool{}
	legacyTargets := map[string]string{}
	if action == "group" {
		found := map[string]*threadBuild{}
		for _, id := range threadIDs {
			for i := range built {
				if built[i].thread.ID == id || built[i].legacyIDs[id] {
					found[id] = &built[i]
					break
				}
			}
			if found[id] == nil {
				return version, errors.New("worldline thread not found")
			}
		}
		for _, b := range found {
			for old := range b.legacyIDs {
				legacyTargets[old] = b.thread.ID
			}
			legacyTargets[b.thread.ID] = b.thread.ID
			for _, entry := range append(append([]WorldlineThreadEntry{}, b.entries...), b.unknown...) {
				refs = append(refs, evidenceRefs(entry.Evidence)...)
				for _, c := range entry.ConversationIDs {
					convs[c] = true
				}
				if id, ok := entry.Claim.Metadata["source_note_id"].(string); ok {
					v, _ := entry.Claim.Metadata["source_note_version"].(string)
					notes[id] = v
				}
			}
		}
		groupID = "thread-group-" + shortHash(scope+"\x00"+requestID)
		if len(threadIDs) > 0 {
			legacyTargets[groupID] = uniqueSorted(threadIDs)[0]
		}
	} else {
		if strings.TrimSpace(groupID) == "" {
			return version, errors.New("group_id required to revoke")
		}
		for _, ev := range events {
			gid, _ := ev.StructuredData["group_id"].(string)
			if ev.Type == "worldline.thread_group" && gid == groupID {
				for old, target := range stringMap(ev.StructuredData["legacy_thread_targets"]) {
					legacyTargets[old] = target
				}
				vals, _ := sourceReferences(ev)
				for _, v := range vals {
					if at := strings.LastIndex(v, "@"); at > 0 {
						refs = append(refs, EvidenceReference{TurnID: v[:at], Revision: v[at+1:]})
					}
				}
				for id, v := range stringMap(ev.StructuredData["source_note_versions"]) {
					notes[id] = v
				}
				for _, c := range stringSlice(ev.StructuredData["source_conversation_ids"]) {
					convs[c] = true
				}
			}
		}
	}
	refs = uniqueEvidenceRefs(refs)
	if len(refs) == 0 {
		return version, errors.New("group change requires current source evidence")
	}
	checked, _, err := s.Time.validateEvidence(ctx, refs)
	if err != nil {
		return version, err
	}
	convIDs := make([]string, 0, len(convs))
	for c := range convs {
		convIDs = append(convIDs, c)
	}
	sort.Strings(convIDs)
	now := time.Now().UTC()
	legacyIDs := make([]string, 0, len(legacyTargets))
	for old := range legacyTargets {
		legacyIDs = append(legacyIDs, old)
	}
	sort.Strings(legacyIDs)
	data := map[string]any{"scope": scope, "origin": "worldline", "action": action, "thread_ids": uniqueSorted(threadIDs), "legacy_thread_ids": legacyIDs, "legacy_thread_targets": legacyTargets, "group_id": groupID, "title": strings.TrimSpace(title), "request_id": requestID, "request_payload_hash": shortHash(string(payload)), "source_note_ids": mapKeys(notes), "source_note_versions": notes, "source_conversation_ids": convIDs, "references": checked}
	ev := plane.Event{ID: eventID, Type: "worldline.thread_group", Timestamp: now, ObservedAt: now, RecordedAt: &now, Source: "local-web", Content: "worldline thread group " + action, Confidence: 1, StructuredData: data}
	if len(convIDs) == 1 {
		ev.ConversationID = convIDs[0]
	}
	if err = s.appendWorldlineMutation(ctx, scope, expectedVersion, shortHash(string(payload)), ev); err != nil {
		return version, err
	}
	return worldlineVersionFromEvents(s, ctx, scope), nil
}

func mapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type threadBuild struct {
	thread           WorldlineThread
	entries, unknown []WorldlineThreadEntry
	seen             map[string]bool
	conversations    map[string]bool
	titles           map[string]bool
	legacyIDs        map[string]bool
	suppressed       bool
	groupedAt        time.Time
}

func worldlineEventTime(ev plane.Event) time.Time {
	if ev.RecordedAt != nil {
		return ev.RecordedAt.UTC()
	}
	return ev.Timestamp.UTC()
}

func activeThreadBuilds(input []threadBuild) []threadBuild {
	out := make([]threadBuild, 0, len(input))
	for _, b := range input {
		if !b.suppressed {
			out = append(out, b)
		}
	}
	return out
}

func filterThreadBuilds(input []threadBuild, q WorldlineQuery) []threadBuild {
	search := strings.ToLower(strings.TrimSpace(q.Query))
	out := make([]threadBuild, 0, len(input))
	for _, original := range input {
		b := original
		b.entries = filterThreadEntries(original.entries, b.thread.Title, q, search, false)
		b.unknown = filterThreadEntries(original.unknown, b.thread.Title, q, search, true)
		if len(b.entries)+len(b.unknown) == 0 {
			continue
		}
		b.thread.EntryCount = len(b.entries) + len(b.unknown)
		b.thread.DatedEntryCount = len(b.entries)
		b.thread.UnknownEntryCount = len(b.unknown)
		b.thread.SemanticReviewCount = 0
		for _, entry := range append(append([]WorldlineThreadEntry{}, b.entries...), b.unknown...) {
			if entry.SemanticSuggestion != nil && entry.SemanticSuggestion.RequiresReview {
				b.thread.SemanticReviewCount++
			}
		}
		b.conversations = map[string]bool{}
		b.thread.LastMentionedAt = nil
		for _, e := range append(append([]WorldlineThreadEntry{}, b.entries...), b.unknown...) {
			for _, id := range e.ConversationIDs {
				b.conversations[id] = true
			}
			if e.MentionedAt != nil && (b.thread.LastMentionedAt == nil || e.MentionedAt.After(*b.thread.LastMentionedAt)) {
				v := *e.MentionedAt
				b.thread.LastMentionedAt = &v
			}
		}
		b.thread.ConversationCount = len(b.conversations)
		b.thread.FirstMentionedAt = nil
		for _, e := range b.entries {
			if e.MentionedAt != nil && (b.thread.FirstMentionedAt == nil || e.MentionedAt.Before(*b.thread.FirstMentionedAt)) {
				v := *e.MentionedAt
				b.thread.FirstMentionedAt = &v
			}
		}
		out = append(out, b)
	}
	return out
}

func filterThreadEntries(entries []WorldlineThreadEntry, title string, q WorldlineQuery, search string, unknown bool) []WorldlineThreadEntry {
	out := make([]WorldlineThreadEntry, 0, len(entries))
	for _, entry := range entries {
		if q.Status != "" && q.Status != "all" && entry.Status != q.Status {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(title), search) && !claimMatches(title, "", entry.Claim, search) {
			continue
		}
		if !unknown && entry.MentionedAt != nil {
			if q.From != nil && entry.MentionedAt.Before(*q.From) {
				continue
			}
			if q.To != nil && entry.MentionedAt.After(*q.To) {
				continue
			}
		}
		out = append(out, entry)
	}
	return out
}

func buildWorldlineThreads(topics []WorldlineTopic) []WorldlineThread {
	built := buildWorldlineThreadDetails(topics)
	out := make([]WorldlineThread, 0, len(built))
	for _, b := range built {
		out = append(out, b.thread)
	}
	return out
}

func buildWorldlineThreadDetails(topics []WorldlineTopic) []threadBuild {
	groups := map[string]*threadBuild{}
	for _, topic := range topics {
		for _, lane := range topic.Lanes {
			for _, claim := range lane.Claims {
				title := strings.Join(strings.Fields(strings.TrimSpace(claim.SourceTopicTitle)), " ")
				if strings.TrimSpace(title) == "" {
					title = claim.TopicTitleFallback()
				}
				key := normalizeTopicTitle(title)
				if key == "" {
					key = "未分类"
					title = key
				}
				g := groups[key]
				if g == nil {
					id := "thread-" + shortHash(key) // legacy title-derived ID, retained as a redirect
					macro := macroTopic(key)
					g = &threadBuild{thread: WorldlineThread{ID: id, Title: title, MacroTopicID: "topic-" + shortHash("macro:"+macro), MacroTopicTitle: macro, SourceTopicIDs: []string{}}, seen: map[string]bool{}, conversations: map[string]bool{}, titles: map[string]bool{}, legacyIDs: map[string]bool{id: true}}
					groups[key] = g
				}
				if title < g.thread.Title {
					g.thread.Title = title
				}
				if claim.SourceTopicID != "" {
					g.titles[claim.SourceTopicID] = true
				}
				identity := claim.ID
				if noteID, ok := claim.Metadata["source_note_id"].(string); ok && noteID != "" {
					identity = "note:" + noteID
				}
				if g.seen[identity] {
					continue
				}
				g.seen[identity] = true
				convs := []string{}
				convSeen := map[string]bool{}
				for _, ev := range claim.Evidence {
					if ev.ConversationID != "" && !convSeen[ev.ConversationID] {
						convSeen[ev.ConversationID] = true
						convs = append(convs, ev.ConversationID)
						g.conversations[ev.ConversationID] = true
					}
				}
				sort.Strings(convs)
				entry := WorldlineThreadEntry{ID: "thread-entry-" + shortHash(key+"\x00"+identity), ClaimID: claim.ID, Version: claim.Version, Summary: claim.Summary, Status: claim.Status, Category: lane.Category, MacroTopicID: topic.ID, LaneID: lane.ID, ConversationIDs: convs, Evidence: append([]WorldlineEvidence{}, claim.Evidence...), Claim: claim, SubjectKey: claim.SubjectKey}
				if claim.MentionedAt != nil {
					v := *claim.MentionedAt
					entry.MentionedAt = &v
					entry.TimeKnown = true
				}
				if entry.TimeKnown {
					g.entries = append(g.entries, entry)
				} else {
					g.unknown = append(g.unknown, entry)
				}
			}
		}
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]threadBuild, 0, len(keys))
	for _, key := range keys {
		g := groups[key]
		g.thread.SourceTopicIDs = make([]string, 0, len(g.titles))
		for id := range g.titles {
			g.thread.SourceTopicIDs = append(g.thread.SourceTopicIDs, id)
		}
		sort.Strings(g.thread.SourceTopicIDs)
		if len(g.thread.SourceTopicIDs) > 0 {
			legacyID := g.thread.ID
			g.thread.ID = "thread-topic-" + shortHash("source-topic:"+g.thread.SourceTopicIDs[0])
			g.legacyIDs[g.thread.ID] = true
			g.legacyIDs[legacyID] = true
		}
		g.thread.ConversationCount = len(g.conversations)
		g.thread.EntryCount = len(g.entries) + len(g.unknown)
		g.thread.DatedEntryCount = len(g.entries)
		g.thread.UnknownEntryCount = len(g.unknown)
		sort.Slice(g.entries, func(i, j int) bool {
			a, b := g.entries[i], g.entries[j]
			if !a.MentionedAt.Equal(*b.MentionedAt) {
				return a.MentionedAt.Before(*b.MentionedAt)
			}
			return a.ID < b.ID
		})
		sort.Slice(g.unknown, func(i, j int) bool { return g.unknown[i].ID < g.unknown[j].ID })
		assignThreadRelations(g.entries, g.unknown)
		for _, e := range g.entries {
			if e.MentionedAt != nil && (g.thread.LastMentionedAt == nil || e.MentionedAt.After(*g.thread.LastMentionedAt)) {
				v := *e.MentionedAt
				g.thread.LastMentionedAt = &v
			}
			if e.MentionedAt != nil && (g.thread.FirstMentionedAt == nil || e.MentionedAt.Before(*g.thread.FirstMentionedAt)) {
				v := *e.MentionedAt
				g.thread.FirstMentionedAt = &v
			}
		}
		out = append(out, *g)
	}
	return out
}

func (c WorldlineClaim) TopicTitleFallback() string {
	if c.TopicID != "" {
		return c.TopicID
	}
	return "未分类"
}

func assignThreadRelations(dated, unknown []WorldlineThreadEntry) {
	last := map[string]string{}
	seenTopic := false
	for i := range dated {
		dated[i].Relation, dated[i].RelationReason = threadRelation(dated[i], last, seenTopic)
		seenTopic = true
		if dated[i].Category == "facts" && dated[i].SubjectKey != "" && !strings.HasPrefix(dated[i].SubjectKey, "unknown:") {
			last[dated[i].SubjectKey] = normalizeTopicTitle(dated[i].Summary)
		}
	}
	for i := range unknown {
		unknown[i].Relation, unknown[i].RelationReason = threadHumanRelation(unknown[i])
		if unknown[i].Relation == "" {
			if unknown[i].Category == "plans" {
				unknown[i].Relation = "plan"
				unknown[i].RelationReason = "source_note_is_non_observation"
			} else if cue, ok := matchingChangeCue(unknown[i].Summary, unknown[i].Evidence); ok {
				unknown[i].Relation = "review_required"
				unknown[i].RelationReason = "explicit_change_cue_" + cue + "_in_source_and_summary; review_only_subject_unconfirmed"
			} else {
				unknown[i].Relation = "unknown_time"
				unknown[i].RelationReason = "mention_timestamp_unavailable"
			}
		}
	}
}

func threadRelation(entry WorldlineThreadEntry, last map[string]string, seenTopic bool) (string, string) {
	if relation, reason := threadHumanRelation(entry); relation != "" {
		return relation, reason
	}
	if entry.Category == "plans" {
		return "plan", "source_note_is_non_observation"
	}
	unknownSubject := entry.SubjectKey == "" || strings.HasPrefix(entry.SubjectKey, "unknown:")
	current := normalizeTopicTitle(entry.Summary)
	prior, hasPrior := last[entry.SubjectKey]
	if !unknownSubject && hasPrior && current == prior {
		return "repeated_statement", "same_subject_and_normalized_summary"
	}
	if cue, ok := matchingChangeCue(entry.Summary, entry.Evidence); ok {
		reason := "explicit_change_cue_" + cue + "_in_source_and_summary; review_only"
		if unknownSubject {
			reason += "_subject_unconfirmed"
		}
		return "review_required", reason
	}
	if !seenTopic {
		if unknownSubject {
			return "first_mention", "first_time_known_topic_mention_identity_unconfirmed"
		}
		return "first_mention", "first_time_known_mention_for_confirmed_subject"
	}
	if unknownSubject {
		return "subsequent_mention", "topic_later_mention_identity_unconfirmed"
	}
	return "subsequent_mention", "later_time_known_mention_without_supported_change"
}

func threadHumanRelation(entry WorldlineThreadEntry) (string, string) {
	switch entry.Claim.ChangeType {
	case "record_correction":
		return "human_correction", "appended_human_record_correction"
	case "reality_change":
		return "human_reality_change", "explicit_human_reported_reality_change"
	}
	if entry.Status == "reviewed" {
		return "human_review", "human_review_feedback"
	}
	return "", ""
}

func matchingChangeCue(summary string, evidence []WorldlineEvidence) (string, bool) {
	cues := []string{"改为", "改成", "不再", "取消", "调整为"}
	for _, ev := range evidence {
		for _, cue := range cues {
			if strings.Contains(summary, cue) && strings.Contains(ev.Text, cue) {
				return cue, true
			}
		}
	}
	if strings.Contains(summary, "原来") && strings.Contains(summary, "现在") {
		for _, ev := range evidence {
			if strings.Contains(ev.Text, "原来") && strings.Contains(ev.Text, "现在") {
				return "原来...现在", true
			}
		}
	}
	return "", false
}

func pageThreadEntries(entries []WorldlineThreadEntry, offset, limit int) []WorldlineThreadEntry {
	if offset >= len(entries) {
		return []WorldlineThreadEntry{}
	}
	end := offset + limit
	if end > len(entries) {
		end = len(entries)
	}
	return append([]WorldlineThreadEntry{}, entries[offset:end]...)
}

func threadMentionRange(threads []WorldlineThread) WorldlineRange {
	var start, end *time.Time
	for _, t := range threads {
		if t.FirstMentionedAt != nil {
			if start == nil || t.FirstMentionedAt.Before(*start) {
				v := *t.FirstMentionedAt
				start = &v
			}
			if end == nil || t.LastMentionedAt.After(*end) {
				v := *t.LastMentionedAt
				end = &v
			}
		}
	}
	return WorldlineRange{Start: start, End: end, MentionedStart: start, MentionedEnd: end}
}
