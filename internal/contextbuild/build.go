// Package contextbuild assembles deterministic, task-scoped context packages
// from the memory store. Every build runs the same branches in the same order
// so that the same database snapshot and request always produce the same
// package: lexical recall, bounded topic/conversation navigation, expansion
// through confirmed topic links, optional raw-evidence windows, source-evidence
// deduplication, a byte/item budget and a set of coverage gaps.
//
// The builder never emits chain-of-thought. The optional Reranker only
// reorders the deterministically assembled items; it can never add or remove
// them, and a missing or failing reranker leaves the deterministic result in
// place while flagging the package as degraded.
package contextbuild

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"recordingmcp/internal/decision"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/pipeline"
)

const (
	defaultMaxBytes = 16000
	minMaxBytes     = 1
	maxMaxBytes     = 262144

	defaultMaxItems = 12
	minMaxItems     = 1
	maxMaxItems     = 64

	// lexicalLimitFactor scales the lexical recall limit with MaxItems. The
	// floor keeps short requests from under-fetching candidates.
	lexicalLimitFactor = 3
	lexicalLimitFloor  = 36

	maxRequestTopics    = 4
	maxNavTopics        = 2
	maxNavConversations = 2
	maxLinkTopics       = 6
	maxLinksPerTopic    = 8
	maxEvidenceItems    = 3
	evidenceRadius      = 1

	analysisProcessorVersion = "extract-v1"

	degradeNoReranker = "decision rerank not configured; deterministic assembly"
)

// Request describes one context build. At least one of Purpose-adjacent
// selectors (Query, ConversationID, TopicIDs) must be non-empty.
type Request struct {
	Purpose            string     `json:"purpose,omitempty"`
	Query              string     `json:"query,omitempty"`
	ConversationID     string     `json:"conversation_id,omitempty"`
	TopicIDs           []string   `json:"topic_ids,omitempty"`
	StartedAfter       *time.Time `json:"started_after,omitempty"`
	StartedBefore      *time.Time `json:"started_before,omitempty"`
	MaxBytes           int        `json:"max_bytes,omitempty"`
	MaxItems           int        `json:"max_items,omitempty"`
	IncludeRawEvidence bool       `json:"include_raw_evidence,omitempty"`
}

// EvidenceRef is one raw transcript turn exposed as supporting evidence.
// UsedByItemIDs names every selected item that cited the turn.
type EvidenceRef struct {
	TurnID          string   `json:"turn_id"`
	Text            string   `json:"text"`
	SpeakerScopedID *string  `json:"speaker_scoped_id"`
	GlobalStartMS   *int64   `json:"global_start_ms"`
	GlobalEndMS     *int64   `json:"global_end_ms"`
	UsedByItemIDs   []string `json:"used_by_item_ids"`
}

// Gap is one deterministic coverage caveat.
type Gap struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// Package is the assembled context.
type Package struct {
	Items         []memory.Item `json:"items"`
	Evidence      []EvidenceRef `json:"evidence"`
	Coverage      Coverage      `json:"coverage"`
	Gaps          []Gap         `json:"gaps"`
	Truncated     bool          `json:"truncated"`
	Degraded      bool          `json:"degraded"`
	DegradeReason string        `json:"degrade_reason"`
	Trace         Trace         `json:"trace"`
}

// TimeRange echoes the requested bounds and reports whether the returned
// package covers conversations whose recording start time is unknown.
type TimeRange struct {
	StartedAfter        *time.Time `json:"started_after"`
	StartedBefore       *time.Time `json:"started_before"`
	UnknownTimeIncluded bool       `json:"unknown_time_included"`
}

// Branches records which assembly branches produced content.
type Branches struct {
	Lexical  bool `json:"lexical"`
	Links    bool `json:"links"`
	Evidence bool `json:"evidence"`
}

// Scanned counts the distinct candidates examined before the budget.
type Scanned struct {
	Notes int `json:"notes"`
	Turns int `json:"turns"`
}

// Coverage reports what the package covers.
type Coverage struct {
	Conversations []string  `json:"conversations"`
	TopicIDs      []string  `json:"topic_ids"`
	TimeRange     TimeRange `json:"time_range"`
	Branches      Branches  `json:"branches"`
	Scanned       Scanned   `json:"scanned"`
}

// LexicalTrace records the lexical recall.
type LexicalTrace struct {
	Query  string   `json:"query"`
	HitIDs []string `json:"hit_ids"`
}

// LinkExpansion records one confirmed link traversed during assembly.
type LinkExpansion struct {
	LinkID   string `json:"link_id"`
	Relation string `json:"relation"`
	FromID   string `json:"from_id"`
	ToID     string `json:"to_id"`
}

// BudgetTrace records the realized budget usage.
type BudgetTrace struct {
	BytesUsed    int `json:"bytes_used"`
	ItemsUsed    int `json:"items_used"`
	EvidenceUsed int `json:"evidence_used"`
}

// Trace is the assembly audit trail. It contains no model reasoning.
type Trace struct {
	Lexical        LexicalTrace    `json:"lexical"`
	LinksExpanded  []LinkExpansion `json:"links_expanded"`
	StaleFiltered  []string        `json:"stale_filtered"`
	DecisionRerank bool            `json:"decision_rerank"`
	Budget         BudgetTrace     `json:"budget"`
}

// Reranker optionally reorders the deterministically assembled items. It must
// return a permutation of the items it was given.
type Reranker interface {
	Rerank(ctx context.Context, query string, items []memory.Item) ([]memory.Item, error)
}

// DecisionReranker adapts the decision engine into a Reranker. It asks one
// score question per item and orders items by the returned score descending,
// breaking ties with the original deterministic order. It never fabricates or
// drops items.
type DecisionReranker struct {
	Engine decision.Engine
}

// rerankRubric is the ordered score rubric used for every rerank question.
var rerankRubric = []string{"irrelevant", "weak", "relevant", "critical"}

// Rerank implements Reranker.
func (d DecisionReranker) Rerank(ctx context.Context, query string, items []memory.Item) ([]memory.Item, error) {
	if d.Engine == nil {
		return nil, errors.New("contextbuild: decision engine is required")
	}
	if len(items) == 0 {
		return items, nil
	}
	questions := make([]decision.Question, len(items))
	for i := range items {
		questions[i] = decision.Question{
			ID:           fmt.Sprintf("item_%d", i),
			Type:         decision.TypeScore,
			Instructions: fmt.Sprintf("Rate how relevant item_%d is to the query %q against the rubric.", i, query),
			Rubric:       append([]string(nil), rerankRubric...),
		}
	}
	result, err := d.Engine.Evaluate(ctx, rerankState(query, items), questions)
	if err != nil {
		return nil, err
	}
	type scoredItem struct {
		item  memory.Item
		score float64
		index int
	}
	scored := make([]scoredItem, len(items))
	for i, item := range items {
		answer, ok := result.Answers[questions[i].ID]
		if !ok {
			return nil, fmt.Errorf("contextbuild: decision engine returned no answer for %s", questions[i].ID)
		}
		scored[i] = scoredItem{item: item, score: answer.Score, index: i}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].index < scored[j].index
	})
	out := make([]memory.Item, len(items))
	for i, s := range scored {
		out[i] = s.item
	}
	return out, nil
}

// rerankState renders the items into the single state string the decision
// engine sees. It is deterministic and contains only item content, never
// instructions to reason step by step.
func rerankState(query string, items []memory.Item) string {
	var b strings.Builder
	b.WriteString("query: ")
	b.WriteString(query)
	b.WriteString("\n")
	for i, item := range items {
		fmt.Fprintf(&b, "item_%d kind=%s id=%s\n", i, item.Kind, item.ID)
		if item.Summary != "" {
			fmt.Fprintf(&b, "summary: %s\n", item.Summary)
		}
		if item.Content != "" {
			fmt.Fprintf(&b, "content: %s\n", item.Content)
		}
	}
	return b.String()
}

// candidate is one recalled item plus the signal that produced it.
type candidate struct {
	item     memory.Item
	score    float64
	evidence []string
}

// Build assembles a deterministic context package.
func Build(ctx context.Context, store *memory.Store, db *sql.DB, rerank Reranker, req Request) (Package, error) {
	if err := ctx.Err(); err != nil {
		return Package{}, err
	}
	if store == nil {
		return Package{}, errors.New("build_context: store is required")
	}
	if db == nil {
		return Package{}, errors.New("build_context: db is required")
	}

	query := strings.TrimSpace(req.Query)
	convID := strings.TrimSpace(req.ConversationID)
	topics := normalizeIDs(req.TopicIDs)
	if query == "" && convID == "" && len(topics) == 0 {
		return Package{}, errors.New("build_context: at least one of query, conversation_id, topic_ids is required")
	}
	maxBytes := clampInt(req.MaxBytes, defaultMaxBytes, minMaxBytes, maxMaxBytes)
	maxItems := clampInt(req.MaxItems, defaultMaxItems, minMaxItems, maxMaxItems)

	pkg := Package{
		Items:    []memory.Item{},
		Evidence: []EvidenceRef{},
		Gaps:     []Gap{},
		Trace: Trace{
			LinksExpanded: []LinkExpansion{},
			StaleFiltered: []string{},
		},
	}
	pkg.Trace.Lexical.Query = query
	pkg.Trace.Lexical.HitIDs = []string{}

	candidates := newCandidateSet()
	scope := newRequestScope(convID, topics, req.StartedAfter, req.StartedBefore)

	// addItem applies the hard scope and staleness guards before one item can
	// enter the candidate set. Every branch funnels through it, so no branch
	// can leak out-of-scope or superseded content into the package.
	addItem := func(item memory.Item, score float64, evidence []string) (bool, error) {
		if item.Stale {
			return false, nil
		}
		ok, err := scope.allows(ctx, db, item)
		if err != nil || !ok {
			return false, err
		}
		candidates.add(item, score, evidence)
		return true, nil
	}

	// addQueryPage folds one store.Query page into the candidate set. The
	// query view does not populate note evidence, so it is hydrated from
	// note_evidence first: items discovered through navigation or confirmed
	// links must stay citable and take part in the source-evidence dedupe.
	addQueryPage := func(page memory.Page) error {
		evMap, err := hydrateEvidence(ctx, db, page.Items)
		if err != nil {
			return err
		}
		for _, item := range page.Items {
			ev := evMap[item.ID]
			if len(ev) == 0 {
				ev = evidenceOf(item)
			}
			if len(item.EvidenceTurnIDs) == 0 && isNoteKind(item.Kind) {
				item.EvidenceTurnIDs = ev
			}
			if _, err := addItem(item, 0, ev); err != nil {
				return err
			}
		}
		return nil
	}

	// Step 1: recall. Lexical search first so its higher-confidence hits win
	// the (kind, id) dedupe over filter-only navigation. An explicit topic
	// scope narrows the lexical recall itself (query AND topic); the global
	// search only runs when the request leaves topics unconstrained.
	addHits := func(page memory.SearchPage) error {
		for _, hit := range page.Hits {
			item, evidence, err := itemFromHit(ctx, store, hit)
			if err != nil {
				return err
			}
			added, err := addItem(item, hit.Score, evidence)
			if err != nil {
				return err
			}
			if added {
				pkg.Trace.Lexical.HitIDs = append(pkg.Trace.Lexical.HitIDs, hit.ID)
			}
		}
		return nil
	}
	if query != "" && !scope.topicScoped() {
		limit := maxOf(lexicalLimitFactor*maxItems, lexicalLimitFloor)
		page, err := store.SearchMemory(ctx, memory.MemorySearch{
			Text:          query,
			Scope:         "both",
			Limit:         limit,
			StartedAfter:  req.StartedAfter,
			StartedBefore: req.StartedBefore,
		})
		if err != nil {
			return Package{}, err
		}
		if err := addHits(page); err != nil {
			return Package{}, err
		}
	}
	for _, tid := range firstN(topics, maxRequestTopics) {
		if query != "" {
			page, err := store.SearchMemory(ctx, memory.MemorySearch{
				Text:          query,
				Scope:         "both",
				TopicID:       tid,
				Limit:         maxItems,
				StartedAfter:  req.StartedAfter,
				StartedBefore: req.StartedBefore,
			})
			if err != nil {
				return Package{}, err
			}
			if err := addHits(page); err != nil {
				return Package{}, err
			}
		} else {
			page, err := store.Query(ctx, memory.Query{TopicID: tid, IncludeTurns: true, Limit: maxItems})
			if err != nil {
				return Package{}, err
			}
			if err := addQueryPage(page); err != nil {
				return Package{}, err
			}
		}
	}
	if convID != "" {
		page, err := store.Query(ctx, memory.Query{ConversationID: convID, IncludeTurns: true, Limit: maxItems})
		if err != nil {
			return Package{}, err
		}
		if err := addQueryPage(page); err != nil {
			return Package{}, err
		}
	}

	// Step 2: navigation. Expand a bounded number of unseen topics and
	// conversations discovered in the recalled items.
	requestTopics := map[string]bool{}
	for _, tid := range topics {
		requestTopics[tid] = true
	}
	navTopics := []string{}
	for _, tid := range candidates.topicIDs() {
		if requestTopics[tid] || !scope.topicAllowed(tid) {
			continue
		}
		navTopics = append(navTopics, tid)
	}
	for _, tid := range firstN(navTopics, maxNavTopics) {
		page, err := store.Query(ctx, memory.Query{TopicID: tid, Limit: maxItems})
		if err != nil {
			return Package{}, err
		}
		if err := addQueryPage(page); err != nil {
			return Package{}, err
		}
	}
	navConversations := []string{}
	for _, cid := range candidates.conversationIDs() {
		if cid == convID || !scope.conversationAllowed(cid) {
			continue
		}
		navConversations = append(navConversations, cid)
	}
	for _, cid := range firstN(navConversations, maxNavConversations) {
		page, err := store.Query(ctx, memory.Query{ConversationID: cid, Limit: maxItems})
		if err != nil {
			return Package{}, err
		}
		if err := addQueryPage(page); err != nil {
			return Package{}, err
		}
	}

	// Step 3: confirmed-link expansion across at most six distinct topics.
	linkTopics := firstN(candidates.topicIDs(), maxLinkTopics)
	seenLinks := map[string]bool{}
	for _, tid := range linkTopics {
		links, err := pipeline.ListLinks(ctx, db, pipeline.LinkFilter{
			Status:  pipeline.LinkStatusConfirmed,
			TopicID: tid,
			Limit:   maxLinksPerTopic,
		})
		if err != nil {
			return Package{}, err
		}
		for _, link := range links {
			if seenLinks[link.ID] {
				continue
			}
			// A confirmed link is a review verdict, not current truth: when any
			// of its evidence turns has been superseded the link is audit-only
			// and must not navigate current retrieval.
			if link.Stale {
				continue
			}
			other := otherTopic(link, tid)
			if other == "" {
				continue
			}
			// A link can never pull the package out of the requested scope:
			// a target topic outside an explicit topic scope is not traversed,
			// and the target query is narrowed to the requested conversation.
			if !scope.topicAllowed(other) {
				continue
			}
			seenLinks[link.ID] = true
			pkg.Trace.LinksExpanded = append(pkg.Trace.LinksExpanded, LinkExpansion{
				LinkID:   link.ID,
				Relation: link.Relation,
				FromID:   link.FromID,
				ToID:     link.ToID,
			})
			q := memory.Query{TopicID: other, Limit: maxItems}
			if convID != "" {
				q.ConversationID = convID
			}
			page, err := store.Query(ctx, q)
			if err != nil {
				return Package{}, err
			}
			if err := addQueryPage(page); err != nil {
				return Package{}, err
			}
		}
	}

	pkg.Coverage.Scanned = candidates.scanned()

	// Step 5 (before budgeting): rank the candidates and drop items whose
	// evidence is already fully covered by a better-ranked item.
	ordered := dedupeBySourceEvidence(candidates.sorted())

	// Step 6: budget the ranked items, then (step 4) attach evidence windows
	// for up to three selected items while the byte budget allows.
	selected := []memory.Item{}
	evidence := []EvidenceRef{}
	evidenceIndex := map[string]int{}
	bytesUsed := 0
	truncated := false
	evidenceFetched := 0
	for _, c := range ordered {
		if len(selected) >= maxItems {
			truncated = true
			break
		}
		itemBytes := len(c.item.Summary) + len(c.item.Content)
		if bytesUsed+itemBytes > maxBytes {
			truncated = true
			break
		}
		selected = append(selected, c.item)
		bytesUsed += itemBytes

		if !req.IncludeRawEvidence || evidenceFetched >= maxEvidenceItems {
			continue
		}
		turnID := c.firstEvidenceTurn()
		if turnID == "" {
			continue
		}
		remaining := maxBytes - bytesUsed
		if remaining <= 0 {
			truncated = true
			continue
		}
		evidenceFetched++
		// Current-state evidence only: the window comes from the current-only
		// lookup, so a superseded neighbour can never enter the package. Scope
		// is inherited from the citing item (it already passed the hard scope,
		// and the window is the same recording as that item).
		window, err := store.GetCurrentEvidenceContext(ctx, turnID, evidenceRadius, remaining)
		if err != nil {
			return Package{}, err
		}
		for _, turn := range window.Turns {
			if idx, ok := evidenceIndex[turn.ID]; ok {
				evidence[idx].UsedByItemIDs = appendUnique(evidence[idx].UsedByItemIDs, c.item.ID)
				continue
			}
			if bytesUsed+len(turn.Text) > maxBytes {
				truncated = true
				break
			}
			evidenceIndex[turn.ID] = len(evidence)
			evidence = append(evidence, EvidenceRef{
				TurnID:          turn.ID,
				Text:            turn.Text,
				SpeakerScopedID: turn.SpeakerScopedID,
				GlobalStartMS:   turn.GlobalStartMS,
				GlobalEndMS:     turn.GlobalEndMS,
				UsedByItemIDs:   []string{c.item.ID},
			})
			bytesUsed += len(turn.Text)
		}
	}

	// Step 8: optional shadow rerank. A missing or failing reranker degrades
	// the package but never empties it.
	items := selected
	if len(items) == 0 {
		items = []memory.Item{}
	}
	if rerank == nil {
		pkg.Degraded = true
		pkg.DegradeReason = degradeNoReranker
	} else if len(items) > 0 {
		reordered, err := rerank.Rerank(ctx, query, items)
		if err != nil {
			pkg.Degraded = true
			pkg.DegradeReason = "decision rerank failed: " + err.Error()
		} else {
			pkg.Trace.DecisionRerank = true
			items = reordered
			if items == nil {
				items = []memory.Item{}
			}
		}
	}

	pkg.Items = items
	pkg.Evidence = evidence
	pkg.Truncated = truncated
	pkg.Trace.Budget = BudgetTrace{
		BytesUsed:    bytesUsed,
		ItemsUsed:    len(items),
		EvidenceUsed: len(evidence),
	}
	pkg.Coverage.Conversations = coveredConversations(items, convID)
	pkg.Coverage.TopicIDs = coveredTopics(items)
	if scope.topicScoped() || scope.conversationID != "" {
		inScope := make([]string, 0, len(pkg.Coverage.Conversations))
		for _, cid := range pkg.Coverage.Conversations {
			if scope.conversationAllowed(cid) {
				inScope = append(inScope, cid)
			}
		}
		pkg.Coverage.Conversations = inScope
		topicScope := make([]string, 0, len(pkg.Coverage.TopicIDs))
		for _, tid := range pkg.Coverage.TopicIDs {
			if scope.topicAllowed(tid) {
				topicScope = append(topicScope, tid)
			}
		}
		pkg.Coverage.TopicIDs = topicScope
	}
	pkg.Coverage.TimeRange = TimeRange{
		StartedAfter:  req.StartedAfter,
		StartedBefore: req.StartedBefore,
	}
	pkg.Coverage.Branches = Branches{
		Lexical:  query != "",
		Links:    len(pkg.Trace.LinksExpanded) > 0,
		Evidence: len(evidence) > 0,
	}

	if unknown, err := hasUnknownTimeConversation(ctx, db, pkg.Coverage.Conversations); err != nil {
		return Package{}, err
	} else {
		pkg.Coverage.TimeRange.UnknownTimeIncluded = unknown
	}

	if err := collectGaps(ctx, db, &pkg, truncated); err != nil {
		return Package{}, err
	}
	return pkg, nil
}

// candidateSet accumulates deduplicated candidates in first-seen order.
type candidateSet struct {
	order []*candidate
	index map[string]int
}

func newCandidateSet() *candidateSet {
	return &candidateSet{index: map[string]int{}}
}

func candidateKey(item memory.Item) string { return item.Kind + "\x00" + item.ID }

func (c *candidateSet) add(item memory.Item, score float64, evidence []string) {
	key := candidateKey(item)
	if idx, ok := c.index[key]; ok {
		if score > c.order[idx].score {
			c.order[idx].score = score
		}
		if len(c.order[idx].evidence) == 0 && len(evidence) > 0 {
			c.order[idx].evidence = evidence
		}
		return
	}
	c.index[key] = len(c.order)
	c.order = append(c.order, &candidate{item: item, score: score, evidence: evidence})
}

func (c *candidateSet) topicIDs() []string {
	set := map[string]bool{}
	for _, cand := range c.order {
		for _, tid := range cand.item.TopicIDs {
			if tid != "" {
				set[tid] = true
			}
		}
	}
	return sortedKeys(set)
}

func (c *candidateSet) conversationIDs() []string {
	set := map[string]bool{}
	for _, cand := range c.order {
		if cand.item.ConversationID != "" {
			set[cand.item.ConversationID] = true
		}
	}
	return sortedKeys(set)
}

func (c *candidateSet) scanned() Scanned {
	var s Scanned
	for _, cand := range c.order {
		switch {
		case isNoteKind(cand.item.Kind):
			s.Notes++
		case cand.item.Kind == "turn":
			s.Turns++
		}
	}
	return s
}

// sorted ranks candidates deterministically: notes before turns before
// conversations, then higher score, then id ascending.
func (c *candidateSet) sorted() []candidate {
	out := make([]candidate, 0, len(c.order))
	for _, cand := range c.order {
		out = append(out, *cand)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := kindRank(out[i].item.Kind), kindRank(out[j].item.Kind)
		if ri != rj {
			return ri < rj
		}
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].item.ID < out[j].item.ID
	})
	return out
}

// dedupeBySourceEvidence drops, in rank order, any item whose evidence turns
// are already fully covered by an earlier item. Items with no evidence are
// always kept (the empty set carries no information).
func dedupeBySourceEvidence(ordered []candidate) []candidate {
	seen := map[string]bool{}
	out := make([]candidate, 0, len(ordered))
	for _, cand := range ordered {
		ev := evidenceOf(cand.item)
		if len(cand.evidence) > 0 {
			ev = cand.evidence
		}
		if len(ev) > 0 {
			subset := true
			for _, id := range ev {
				if !seen[id] {
					subset = false
					break
				}
			}
			if subset {
				continue
			}
		}
		out = append(out, cand)
		for _, id := range ev {
			seen[id] = true
		}
	}
	return out
}

func (c candidate) firstEvidenceTurn() string {
	if c.item.Kind == "turn" {
		// Item.ID is the stored turn row id; Item.TurnID carries the original
		// transcript reference, which evidence lookups do not accept.
		if c.item.ID != "" {
			return c.item.ID
		}
		return c.item.TurnID
	}
	if len(c.item.EvidenceTurnIDs) > 0 {
		return c.item.EvidenceTurnIDs[0]
	}
	if len(c.evidence) > 0 {
		return c.evidence[0]
	}
	return ""
}

// hydrateEvidence resolves evidence turn ids (ordered by ordinal) for note
// items whose stored view carries none, in one query per page. Items with no
// evidence rows yield nothing, matching the legacy no-evidence convention.
func hydrateEvidence(ctx context.Context, db *sql.DB, items []memory.Item) (map[string][]string, error) {
	ids := []string{}
	for _, it := range items {
		if isNoteKind(it.Kind) && len(it.EvidenceTurnIDs) == 0 {
			ids = append(ids, it.ID)
		}
	}
	out := map[string][]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT ne.note_id, ne.turn_id FROM note_evidence ne WHERE ne.note_id IN (`+placeholders(len(ids))+`) ORDER BY ne.note_id, ne.ordinal`, stringsToAny(ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var noteID, turnID string
		if err := rows.Scan(&noteID, &turnID); err != nil {
			return nil, err
		}
		out[noteID] = append(out[noteID], turnID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// itemFromHit resolves a search hit into its full item. Note evidence ids come
// from the hit when present; otherwise they are hydrated from the stored item
// or its evidence window, so items discovered through navigation and link
// expansion also carry their source turns.
func itemFromHit(ctx context.Context, store *memory.Store, hit memory.SearchHit) (memory.Item, []string, error) {
	detail, err := store.GetItem(ctx, hit.ID, false)
	if err != nil {
		return memory.Item{}, nil, err
	}
	evidence := hit.EvidenceTurnIDs
	if len(evidence) == 0 {
		evidence = evidenceOf(detail.Item)
	}
	if len(evidence) == 0 {
		ids := make([]string, 0, len(detail.Evidence))
		for _, ev := range detail.Evidence {
			if ev.ID != "" {
				ids = append(ids, ev.ID)
			}
		}
		evidence = ids
	}
	if len(detail.Item.EvidenceTurnIDs) == 0 && detail.Item.Kind != "turn" {
		detail.Item.EvidenceTurnIDs = evidence
	}
	return detail.Item, evidence, nil
}

// evidenceOf returns the source evidence turn ids of an item: the turn's own
// id for turn items and the note's evidence list otherwise.
func evidenceOf(item memory.Item) []string {
	switch {
	case item.Kind == "turn":
		// Item.ID is the stored turn row id; Item.TurnID carries the original
		// transcript reference, which evidence lookups do not accept.
		if item.ID != "" {
			return []string{item.ID}
		}
		if item.TurnID != "" {
			return []string{item.TurnID}
		}
		return nil
	case isNoteKind(item.Kind):
		return item.EvidenceTurnIDs
	default:
		return nil
	}
}

func isNoteKind(kind string) bool { return kind != "turn" && kind != "conversation" }

func kindRank(kind string) int {
	switch {
	case isNoteKind(kind):
		return 0
	case kind == "turn":
		return 1
	default:
		return 2
	}
}

// otherTopic returns the topic endpoint of a link that is not tid.
func otherTopic(link pipeline.MemoryLink, tid string) string {
	if link.FromKind == "topic" && link.FromID != tid {
		return link.FromID
	}
	if link.ToKind == "topic" && link.ToID != tid {
		return link.ToID
	}
	return ""
}

// collectGaps appends the deterministic coverage gaps that apply.
func collectGaps(ctx context.Context, db *sql.DB, pkg *Package, truncated bool) error {
	if len(pkg.Items) == 0 {
		pkg.Gaps = append(pkg.Gaps, Gap{Kind: "no_matches", Detail: "no current memory matched the query"})
	}
	if truncated {
		pkg.Gaps = append(pkg.Gaps, Gap{Kind: "budget_exhausted", Detail: "item or byte budget exhausted before all candidates were added"})
	}

	conversations := pkg.Coverage.Conversations
	if len(conversations) > 0 {
		stale, err := staleArtifactIDs(ctx, db, conversations)
		if err != nil {
			return err
		}
		pkg.Trace.StaleFiltered = stale
		if len(stale) > 0 {
			pkg.Gaps = append(pkg.Gaps, Gap{
				Kind:   "stale_pending_reprocessing",
				Detail: fmt.Sprintf("%d stale artifact(s) pending reprocessing", len(stale)),
			})
		}

		unanalysed, err := unanalysedTurnCount(ctx, db, conversations)
		if err != nil {
			return err
		}
		if unanalysed > 0 {
			pkg.Gaps = append(pkg.Gaps, Gap{
				Kind:   "unanalysed_turns",
				Detail: fmt.Sprintf("%d turn(s) lack %s analysis coverage", unanalysed, analysisProcessorVersion),
			})
		}

		incomplete, err := recordingsWithoutAttachments(ctx, db, conversations)
		if err != nil {
			return err
		}
		if incomplete > 0 {
			pkg.Gaps = append(pkg.Gaps, Gap{
				Kind:   "transcript_incomplete",
				Detail: fmt.Sprintf("%d recording(s) have no source attachment", incomplete),
			})
		}
	}

	unknown := 0
	for _, item := range pkg.Items {
		if item.SpeakerScopedID == nil {
			unknown++
		}
	}
	for _, ref := range pkg.Evidence {
		if ref.SpeakerScopedID == nil {
			unknown++
		}
	}
	if unknown > 0 {
		pkg.Gaps = append(pkg.Gaps, Gap{
			Kind:   "speaker_identity_unknown",
			Detail: fmt.Sprintf("%d returned item(s)/evidence turn(s) have an unknown speaker identity", unknown),
		})
	}
	return nil
}

func staleArtifactIDs(ctx context.Context, db *sql.DB, conversations []string) ([]string, error) {
	query := fmt.Sprintf(`SELECT DISTINCT artifact_id FROM derived_artifacts WHERE stale=1 AND conversation_id IN (%s) ORDER BY artifact_id`, placeholders(len(conversations)))
	rows, err := db.QueryContext(ctx, query, stringsToAny(conversations)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func unanalysedTurnCount(ctx context.Context, db *sql.DB, conversations []string) (int, error) {
	query := fmt.Sprintf(`SELECT COUNT(*) FROM turns t WHERE t.conversation_id IN (%s) AND `+memory.CurrentTurnPredicate("t")+` AND NOT EXISTS(SELECT 1 FROM analysis_coverage ac WHERE ac.turn_id=t.id AND ac.processor_version=?)`, placeholders(len(conversations)))
	args := append(stringsToAny(conversations), analysisProcessorVersion)
	var count int
	if err := db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func recordingsWithoutAttachments(ctx context.Context, db *sql.DB, conversations []string) (int, error) {
	query := fmt.Sprintf(`SELECT COUNT(*) FROM recordings r WHERE r.conversation_id IN (%s) AND NOT EXISTS(SELECT 1 FROM source_attachments a WHERE a.recording_id=r.id)`, placeholders(len(conversations)))
	var count int
	if err := db.QueryRowContext(ctx, query, stringsToAny(conversations)...).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func hasUnknownTimeConversation(ctx context.Context, db *sql.DB, conversations []string) (bool, error) {
	if len(conversations) == 0 {
		return false, nil
	}
	query := fmt.Sprintf(`SELECT COUNT(*) FROM conversations c LEFT JOIN recordings r ON r.id=c.recording_id WHERE c.id IN (%s) AND r.recording_started_at IS NULL`, placeholders(len(conversations)))
	var count int
	if err := db.QueryRowContext(ctx, query, stringsToAny(conversations)...).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func coveredConversations(items []memory.Item, requestConversation string) []string {
	set := map[string]bool{}
	for _, item := range items {
		if item.ConversationID != "" {
			set[item.ConversationID] = true
		}
	}
	if requestConversation != "" {
		set[requestConversation] = true
	}
	return sortedKeys(set)
}

func coveredTopics(items []memory.Item) []string {
	set := map[string]bool{}
	for _, item := range items {
		for _, tid := range item.TopicIDs {
			if tid != "" {
				set[tid] = true
			}
		}
	}
	return sortedKeys(set)
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

func normalizeIDs(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, raw := range in {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func appendUnique(in []string, value string) []string {
	for _, existing := range in {
		if existing == value {
			return in
		}
	}
	return append(in, value)
}

func firstN(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return in[:n]
}

func maxOf(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clampInt(v, def, lo, hi int) int {
	if v <= 0 {
		v = def
	}
	if v < lo {
		v = lo
	}
	if v > hi {
		v = hi
	}
	return v
}
