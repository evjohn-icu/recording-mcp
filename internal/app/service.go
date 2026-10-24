package app

import (
	"context"
	"path/filepath"
	"strings"

	"recordingmcp/internal/contextbrief"
	"recordingmcp/internal/contextbuild"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/plane"
	"recordingmcp/internal/semantic"
)

// Service is the shared business facade. The MCP server and the local web
// server both call these methods; no transport owns retrieval or write rules.
type Service struct {
	// BriefPolisher is nil unless explicitly configured at startup.
	BriefPolisher contextbrief.Polisher
	Store         *memory.Store
	Semantic      semantic.SemanticRetriever
	Reranker      contextbuild.Reranker
	Assessor      contextbuild.EvidenceAssessor
	// AudioDir is the cloud-sync audio directory. Every served clip must live
	// beneath it; an empty value disables audio serving instead of allowing
	// paths relative to the process working directory.
	AudioDir string
	// Managed retention roots are provided by the profile resolver, never by
	// request parameters. Empty means no managed copy of that type exists.
	ManagedIndexDir  string
	ManagedBackupDir string
	ForgetPlane      *plane.Service

	audioRoot string
}

// New builds the facade. audioDir may be relative; it is resolved once so a
// later working-directory change cannot move the allowed audio root.
func New(store *memory.Store, sem semantic.SemanticRetriever, rerank contextbuild.Reranker, audioDir string) *Service {
	s := &Service{Store: store, Semantic: sem, Reranker: rerank, AudioDir: audioDir}
	if strings.TrimSpace(audioDir) != "" {
		if abs, err := filepath.Abs(audioDir); err == nil {
			s.audioRoot = abs
		} else {
			s.audioRoot = audioDir
		}
	}
	return s
}

// Query reads one combined note/turn page.
func (s *Service) Query(ctx context.Context, q memory.Query) (memory.Page, error) {
	return s.Store.Query(ctx, q)
}

// ConversationContextInput is the shared conversation read request.
type ConversationContextInput struct {
	ConversationID string
	IncludeTurns   bool
	IncludeAmbient bool
	Limit          int
	Offset         int
}

// ConversationContext is the shared conversation read result. The MCP layer
// adds its own Experimental marker; the fields here are transport-neutral.
type ConversationContext struct {
	ConversationID string
	Title          string
	Topics         memory.TopicList
	Context        memory.Page
	Processing     memory.ProcessingStatus
}

// ConversationContext reads one conversation summary, its topics, one context
// page and its processing status, in the order the MCP tool documents.
func (s *Service) ConversationContext(ctx context.Context, in ConversationContextInput) (ConversationContext, error) {
	if in.ConversationID == "" {
		return ConversationContext{}, validationError("conversation_id is required")
	}
	title, err := s.Store.Conversation(ctx, in.ConversationID)
	if err != nil {
		return ConversationContext{}, err
	}
	topics, err := s.Store.Topics(ctx, in.ConversationID)
	if err != nil {
		return ConversationContext{}, err
	}
	page, err := s.Store.Query(ctx, memory.Query{ConversationID: in.ConversationID, IncludeTurns: in.IncludeTurns, IncludeAmbient: in.IncludeAmbient, Limit: in.Limit, Offset: in.Offset})
	if err != nil {
		return ConversationContext{}, err
	}
	status, err := s.Store.GetProcessingStatus(ctx, in.ConversationID)
	if err != nil {
		return ConversationContext{}, err
	}
	return ConversationContext{ConversationID: in.ConversationID, Title: title, Topics: topics, Context: page, Processing: status}, nil
}

// ListConversations reads one cursor page of conversation summaries.
func (s *Service) ListConversations(ctx context.Context, f memory.ConversationFilter) (memory.ConversationPage, error) {
	return s.Store.ListConversations(ctx, f)
}

// GetItem reads one note, turn or conversation by stable id.
func (s *Service) GetItem(ctx context.Context, id string, includeStale bool) (memory.ItemDetail, error) {
	if strings.TrimSpace(id) == "" {
		return memory.ItemDetail{}, validationError("id is required")
	}
	return s.Store.GetItem(ctx, id, includeStale)
}

// Topics lists topics, optionally scoped to one conversation.
func (s *Service) Topics(ctx context.Context, conversationID string) (memory.TopicList, error) {
	return s.Store.Topics(ctx, conversationID)
}

// ProcessingStatus reports coverage and indexing status for one conversation.
func (s *Service) ProcessingStatus(ctx context.Context, conversationID string) (memory.ProcessingStatus, error) {
	if strings.TrimSpace(conversationID) == "" {
		return memory.ProcessingStatus{}, validationError("conversation_id is required")
	}
	return s.Store.GetProcessingStatus(ctx, conversationID)
}

// AnnotationHistory lists current and archived annotation revisions.
func (s *Service) AnnotationHistory(ctx context.Context, conversationID string) ([]memory.AnnotationSnapshot, error) {
	if strings.TrimSpace(conversationID) == "" {
		return nil, validationError("conversation_id is required")
	}
	return s.Store.GetAnnotationHistory(ctx, conversationID)
}

// Search runs literal substring search over current notes and turns.
func (s *Service) Search(ctx context.Context, q memory.MemorySearch) (memory.SearchPage, error) {
	if strings.TrimSpace(q.Text) == "" {
		return memory.SearchPage{}, validationError("text is required")
	}
	return s.Store.SearchMemory(ctx, q)
}

// EvidenceContext reads the neighborhood of one turn. audit=true is the
// explicit history path (including superseded revisions); audit=false is the
// current-state path and returns not_found for a stale target.
func (s *Service) EvidenceContext(ctx context.Context, turnID string, radius, maxBytes int, audit bool) (memory.EvidenceContext, error) {
	if strings.TrimSpace(turnID) == "" {
		return memory.EvidenceContext{}, validationError("turn_id is required")
	}
	if audit {
		return s.Store.GetEvidenceContext(ctx, turnID, radius, maxBytes)
	}
	return s.Store.GetCurrentEvidenceContext(ctx, turnID, radius, maxBytes)
}

// BuildContext assembles a hybrid context package with the service's fixed
// dependency set.
func (s *Service) BuildContext(ctx context.Context, req contextbuild.Request) (contextbuild.Package, error) {
	return contextbuild.Build(ctx, contextbuild.Deps{Store: s.Store, DB: s.Store.DB(), Reranker: s.Reranker, Assessor: s.Assessor, Semantic: s.Semantic}, req)
}

// ApplyAnnotations applies one full annotation revision. It is a passthrough:
// the MCP tool's semantics, including expected_version and typed errors, stay
// exactly the store's.
func (s *Service) ApplyAnnotations(ctx context.Context, conversationID, expectedVersion string, a memory.Annotations) error {
	return s.Store.ApplyAnnotations(ctx, conversationID, expectedVersion, a)
}
