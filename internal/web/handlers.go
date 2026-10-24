package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"recordingmcp/internal/app"
	"recordingmcp/internal/contextbuild"
	"recordingmcp/internal/memory"
)

// SearchHitView decorates one search hit with the conversation title the web
// UI shows. The hit itself is the store's structure, unchanged.
type SearchHitView struct {
	memory.SearchHit
	ConversationTitle string `json:"conversation_title"`
}

// SearchPageView is the web search page: the store's page plus titles.
type SearchPageView struct {
	Hits          []SearchHitView `json:"hits"`
	NextCursor    string          `json:"next_cursor,omitempty"`
	Degraded      bool            `json:"degraded"`
	DegradeReason string          `json:"degrade_reason,omitempty"`
}

// HybridView is the hybrid search response: the contextbuild package plus the
// titles of every conversation it mentions.
type HybridView struct {
	contextbuild.Package
	ConversationTitles map[string]string `json:"conversation_titles"`
}

// HistoryView is the annotation-history response.
type HistoryView struct {
	Experimental   bool                        `json:"experimental"`
	ConversationID string                      `json:"conversation_id"`
	Revisions      []memory.AnnotationSnapshot `json:"revisions"`
}

func (s *Server) handleConversations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := queryInt(r, "limit", 0, 0, 100)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	status := strings.TrimSpace(q.Get("status"))
	switch status {
	case "", "pending", "partial":
	default:
		writeBadRequest(w, "status must be pending, partial or empty")
		return
	}
	after, err := queryTime(r, "started_after")
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	before, err := queryTime(r, "started_before")
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	page, err := s.svc.ListConversations(r.Context(), memory.ConversationFilter{
		Limit:         limit,
		Cursor:        q.Get("cursor"),
		TopicID:       strings.TrimSpace(q.Get("topic_id")),
		Status:        status,
		StartedAfter:  after,
		StartedBefore: before,
	})
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	if page.Items == nil {
		page.Items = []memory.ConversationSummary{}
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleConversation(w http.ResponseWriter, r *http.Request) {
	view, err := s.svc.ConversationView(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleTurns(w http.ResponseWriter, r *http.Request) {
	recordingID := strings.TrimSpace(r.URL.Query().Get("recording_id"))
	if recordingID == "" {
		writeBadRequest(w, "recording_id is required")
		return
	}
	ambient, err := queryBool(r, "include_ambient")
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	offset, err := queryInt(r, "offset", 0, 0, 1<<30)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	limit, err := queryInt(r, "limit", 100, 1, 500)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	page, err := s.svc.TurnPage(r.Context(), r.PathValue("id"), recordingID, ambient, offset, limit)
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	revisions, err := s.svc.AnnotationHistory(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	if revisions == nil {
		revisions = []memory.AnnotationSnapshot{}
	}
	writeJSON(w, http.StatusOK, HistoryView{Experimental: true, ConversationID: id, Revisions: revisions})
}

func (s *Server) handleEditNote(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		ExpectedVersion string `json:"expected_version"`
		NewVersion      string `json:"new_version"`
		Summary         string `json:"summary"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	res, err := s.svc.EditNote(r.Context(), r.PathValue("id"), r.PathValue("rowID"), body.ExpectedVersion, body.NewVersion, body.Summary)
	if err != nil {
		var details map[string]any
		var appErr *app.Error
		if errors.As(err, &appErr) && appErr.Code == app.CodeAnnotationVersionConflict {
			details = conflictDetails(appErr, map[string]any{
				"submitted": map[string]string{
					"expected_version": body.ExpectedVersion,
					"new_version":      body.NewVersion,
					"summary":          body.Summary,
				},
			})
		}
		s.writeError(w, r, err, details)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleItem(w http.ResponseWriter, r *http.Request) {
	detail, err := s.svc.GetWebItem(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleEvidence(w http.ResponseWriter, r *http.Request) {
	radius, err := queryInt(r, "radius", 0, 0, 50)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	maxBytes, err := queryInt(r, "max_bytes", 0, 0, 1<<20)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	ev, err := s.svc.WebEvidenceContext(r.Context(), r.PathValue("id"), radius, maxBytes)
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeBadRequest(w, "q is required")
		return
	}
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	switch scope {
	case "", "raw", "derived", "both":
	default:
		writeBadRequest(w, "scope must be raw, derived or both")
		return
	}
	limit, err := queryInt(r, "limit", 0, 0, 100)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	ambient, err := queryBool(r, "include_ambient")
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	after, err := queryTime(r, "started_after")
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	before, err := queryTime(r, "started_before")
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	page, err := s.svc.Search(r.Context(), memory.MemorySearch{
		Text:           q,
		Scope:          scope,
		Limit:          limit,
		Cursor:         r.URL.Query().Get("cursor"),
		TopicID:        strings.TrimSpace(r.URL.Query().Get("topic_id")),
		StartedAfter:   after,
		StartedBefore:  before,
		IncludeStale:   false, // the GUI never resurrects superseded memory
		IncludeAmbient: ambient,
	})
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	ids := make([]string, 0, len(page.Hits))
	for _, hit := range page.Hits {
		ids = append(ids, hit.ConversationID)
	}
	titles, err := s.conversationTitles(r.Context(), ids)
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	view := SearchPageView{Hits: make([]SearchHitView, 0, len(page.Hits)), NextCursor: page.NextCursor, Degraded: page.Degraded, DegradeReason: page.DegradeReason}
	for _, hit := range page.Hits {
		view.Hits = append(view.Hits, SearchHitView{SearchHit: hit, ConversationTitle: titles[hit.ConversationID]})
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleHybrid(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeBadRequest(w, "q is required")
		return
	}
	mode := strings.TrimSpace(r.URL.Query().Get("mode"))
	switch mode {
	case "", "hybrid", "lexical":
	default:
		writeBadRequest(w, "mode must be hybrid or lexical")
		return
	}
	version := strings.TrimSpace(r.URL.Query().Get("retrieval_version"))
	if version != "" && version != "v1" && version != "v2" {
		writeBadRequest(w, "retrieval_version must be v1 or v2")
		return
	}
	poolLimit, err := queryInt(r, "rerank_candidate_limit", 64, 1, 128)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	maxBytes, err := queryInt(r, "max_bytes", 16000, 1, 262144)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	maxItems, err := queryInt(r, "max_items", 20, 1, 64)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	after, err := queryTime(r, "started_after")
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	before, err := queryTime(r, "started_before")
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	req := contextbuild.Request{
		Purpose:              "web_search",
		Query:                q,
		ConversationID:       strings.TrimSpace(r.URL.Query().Get("conversation_id")),
		Mode:                 mode,
		StartedAfter:         after,
		StartedBefore:        before,
		MaxItems:             maxItems,
		MaxBytes:             maxBytes,
		RetrievalVersion:     version,
		RerankCandidateLimit: poolLimit,
	}
	if topic := strings.TrimSpace(r.URL.Query().Get("topic_id")); topic != "" {
		req.TopicIDs = []string{topic}
	}
	pkg, err := s.svc.BuildContext(r.Context(), req)
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	if pkg.Items == nil {
		pkg.Items = []memory.Item{}
	}
	ids := make([]string, 0, len(pkg.Items))
	for _, item := range pkg.Items {
		ids = append(ids, item.ConversationID)
	}
	titles, err := s.conversationTitles(r.Context(), ids)
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, HybridView{Package: pkg, ConversationTitles: titles})
}

func (s *Server) handleTopics(w http.ResponseWriter, r *http.Request) {
	topics, err := s.svc.Topics(r.Context(), strings.TrimSpace(r.URL.Query().Get("conversation_id")))
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	if topics.Topics == nil {
		topics.Topics = []memory.Topic{}
	}
	writeJSON(w, http.StatusOK, topics)
}

// conversationTitles resolves display titles for the given conversations in
// one query. A conversation that no longer exists keeps an empty title so the
// UI can say so instead of inventing one.
func (s *Server) conversationTitles(ctx context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	unique := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
		out[id] = ""
	}
	if len(unique) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(unique))
	for _, id := range unique {
		args = append(args, id)
	}
	rows, err := s.svc.Store.DB().QueryContext(ctx, `SELECT id,title FROM conversations WHERE id IN (`+placeholders(len(unique))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, title string
		if err := rows.Scan(&id, &title); err != nil {
			return nil, err
		}
		out[id] = title
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "?"
	}
	return strings.Join(parts, ",")
}

// queryInt parses an optional integer query parameter and clamps it into
// [lo,hi]; an unparsable value is a validation error, never a silently
// ignored filter.
func queryInt(r *http.Request, name string, def, lo, hi int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	if v < lo {
		v = lo
	}
	if v > hi {
		v = hi
	}
	return v, nil
}

// queryBool parses an optional flag; anything that is not clearly false is
// rejected so a typo cannot silently drop the filter.
func queryBool(r *http.Request, name string) (bool, error) {
	raw := strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name)))
	switch raw {
	case "", "0", "false", "no", "off":
		return false, nil
	case "1", "true", "yes", "on":
		return true, nil
	}
	return false, fmt.Errorf("%s must be a boolean", name)
}

// queryTime parses an optional RFC3339 timestamp. The UI always sends an
// explicit offset, so a bare date is rejected instead of being guessed.
func queryTime(r *http.Request, name string) (*time.Time, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, fmt.Errorf("%s must be an RFC3339 timestamp", name)
	}
	return &t, nil
}

func requireJSON(w http.ResponseWriter, r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = ct[:i]
	}
	if !strings.EqualFold(strings.TrimSpace(ct), "application/json") {
		writeBadRequest(w, "Content-Type must be application/json")
		return false
	}
	return true
}

func decodeJSON(r *http.Request, out any) error {
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		return fmt.Errorf("invalid JSON body: %v", err)
	}
	return nil
}
