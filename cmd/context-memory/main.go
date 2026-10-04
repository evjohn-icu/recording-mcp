package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/pipeline"
)

type ConversationInput struct {
	ConversationID string `json:"conversation_id"`
	IncludeTurns   bool   `json:"include_turns,omitempty"`
	Limit          int    `json:"limit,omitempty"`
	Offset         int    `json:"offset,omitempty"`
}
type ItemInput struct {
	ID string `json:"id"`
}
type TopicsInput struct {
	ConversationID string `json:"conversation_id,omitempty"`
}
type StatusInput struct {
	ConversationID string `json:"conversation_id"`
}
type IngestSourceInput struct {
	Provider           string  `json:"provider"`
	ProviderRequestID  string  `json:"provider_request_id"`
	DeviceID           string  `json:"device_id"`
	RecordingID        string  `json:"recording_id"`
	ConversationID     string  `json:"conversation_id,omitempty"`
	SegmentID          string  `json:"segment_id"`
	SegmentOffsetMS    int64   `json:"segment_offset_ms"`
	SegmentDurationMS  int64   `json:"segment_duration_ms"`
	TranscriptRevision string  `json:"transcript_revision"`
	SupersedesRevision string  `json:"supersedes_revision,omitempty"`
	CompletionStatus   string  `json:"completion_status"`
	RecordingStartedAt *string `json:"recording_started_at,omitempty"`
	CaptureTimezone    *string `json:"capture_timezone,omitempty"`
	SourceAudioSHA256  *string `json:"source_audio_sha256,omitempty"`
	SourceRef          *string `json:"source_ref,omitempty"`
	ReceivedAt         *string `json:"received_at,omitempty"`
	Model              *string `json:"model,omitempty"`
	ResourceID         *string `json:"resource_id,omitempty"`
	ServerLogID        *string `json:"server_log_id,omitempty"`
	RawEnvelope        any     `json:"raw_envelope,omitempty"`
}
type IngestTurnInput struct {
	ID              string   `json:"id"`
	OriginalIndex   int      `json:"original_index"`
	Text            string   `json:"text"`
	LocalStartMS    *int64   `json:"local_start_ms,omitempty"`
	LocalEndMS      *int64   `json:"local_end_ms,omitempty"`
	GlobalStartMS   *int64   `json:"global_start_ms,omitempty"`
	GlobalEndMS     *int64   `json:"global_end_ms,omitempty"`
	SpeakerScopedID *string  `json:"speaker_scoped_id,omitempty"`
	SpeakerEntityID *string  `json:"speaker_entity_id,omitempty"`
	Confidence      *float64 `json:"confidence,omitempty"`
	Warnings        []string `json:"warnings,omitempty"`
}
type IngestInput struct {
	ContractVersion string            `json:"contract_version"`
	Source          IngestSourceInput `json:"source"`
	Turns           []IngestTurnInput `json:"turns"`
	RawSource       any               `json:"raw_source,omitempty"`
	Warnings        []string          `json:"warnings,omitempty"`
}
type ApplyAnnotationsInput struct {
	ConversationID    string                       `json:"conversation_id"`
	ExpectedVersion   string                       `json:"expected_version"`
	AnnotationVersion string                       `json:"annotation_version"`
	AnnotationBasis   string                       `json:"annotation_basis,omitempty"`
	Summary           string                       `json:"summary,omitempty"`
	Title             string                       `json:"title"`
	RecordingID       string                       `json:"recording_id,omitempty"`
	Topics            []memory.TopicAnnotation     `json:"topics"`
	Notes             []memory.NoteAnnotation      `json:"notes"`
	TurnTopics        []memory.TurnTopicAnnotation `json:"turn_topics"`
}
type HistoryInput struct {
	ConversationID string `json:"conversation_id"`
}
type HistoryResult struct {
	Experimental   bool                        `json:"experimental"`
	ConversationID string                      `json:"conversation_id"`
	Revisions      []memory.AnnotationSnapshot `json:"revisions"`
}
type ConversationResult struct {
	Experimental   bool                    `json:"experimental"`
	ConversationID string                  `json:"conversation_id"`
	Title          string                  `json:"title"`
	Topics         memory.TopicList        `json:"topics"`
	Context        memory.Page             `json:"context"`
	Processing     memory.ProcessingStatus `json:"processing"`
}
type ListConversationsInput struct {
	TopicID       string `json:"topic_id,omitempty"`
	Status        string `json:"status,omitempty"`
	StartedAfter  string `json:"started_after,omitempty"`
	StartedBefore string `json:"started_before,omitempty"`
	Limit         int    `json:"limit,omitempty"`
	Cursor        string `json:"cursor,omitempty"`
}
type GetEvidenceContextInput struct {
	TurnID   string `json:"turn_id"`
	Radius   int    `json:"radius,omitempty"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}
type SearchMemoryInput struct {
	Text          string `json:"text"`
	Scope         string `json:"scope,omitempty"`
	TopicID       string `json:"topic_id,omitempty"`
	StartedAfter  string `json:"started_after,omitempty"`
	StartedBefore string `json:"started_before,omitempty"`
	Limit         int    `json:"limit,omitempty"`
	Cursor        string `json:"cursor,omitempty"`
}
type ListMemoryLinksInput struct {
	Status  string `json:"status,omitempty"`
	TopicID string `json:"topic_id,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}
type ReviewMemoryLinkInput struct {
	LinkID string `json:"link_id"`
	Action string `json:"action"`
}
type MemoryLinkView struct {
	LinkID          string   `json:"link_id"`
	FromKind        string   `json:"from_kind"`
	FromID          string   `json:"from_id"`
	FromTitle       string   `json:"from_title,omitempty"`
	ToKind          string   `json:"to_kind"`
	ToID            string   `json:"to_id"`
	ToTitle         string   `json:"to_title,omitempty"`
	Relation        string   `json:"relation"`
	Reason          string   `json:"reason"`
	EvidenceTurnIDs []string `json:"evidence_turn_ids"`
	Confidence      float64  `json:"confidence"`
	Status          string   `json:"status"`
	Model           string   `json:"model,omitempty"`
	CreatedAtMS     int64    `json:"created_at_ms"`
}
type MemoryLinkList struct {
	Links []MemoryLinkView `json:"links"`
}

func memoryLinkView(l pipeline.MemoryLink) MemoryLinkView {
	return MemoryLinkView{
		LinkID: l.ID, FromKind: l.FromKind, FromID: l.FromID, FromTitle: l.FromTitle,
		ToKind: l.ToKind, ToID: l.ToID, ToTitle: l.ToTitle, Relation: l.Relation,
		Reason: l.Reason, EvidenceTurnIDs: l.EvidenceTurnIDs, Confidence: l.Confidence,
		Status: l.Status, Model: l.Model, CreatedAtMS: l.CreatedAt.UnixMilli(),
	}
}

// parseRFC3339 converts an optional RFC3339 timestamp string into a *time.Time.
// An empty value yields nil so callers can distinguish "unset" from a zero time.
func parseRFC3339(label, value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, fmt.Errorf("%s must be an RFC3339 timestamp: %w", label, err)
	}
	return &t, nil
}

func newServer(store *memory.Store) *mcp.Server {
	no := false
	s := mcp.NewServer(&mcp.Implementation{Name: "context-memory-experimental", Version: "0.1-experimental"}, &mcp.ServerOptions{Instructions: "Experimental local conversation memory for private audio transcripts. Results preserve source evidence and are not verified facts. Speaker labels are recording-scoped and never identify real people. No audio is downloaded or processed by this server."})
	read := func(name, desc string) *mcp.Tool {
		return &mcp.Tool{Name: name, Description: "EXPERIMENTAL: " + desc, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &no}}
	}
	mcp.AddTool(s, read("query_context", "Find annotated discussion notes and optionally stored transcript turns. query is literal Unicode lowercase substring matching. Filters are ANDed. A speaker filter requires recording_id. Turns are excluded unless include_turns=true."), func(ctx context.Context, _ *mcp.CallToolRequest, in memory.Query) (*mcp.CallToolResult, memory.Page, error) {
		p, e := store.Query(ctx, in)
		return nil, p, e
	})
	mcp.AddTool(s, read("get_conversation_context", "Read one conversation summary, topics and annotated notes; include_turns explicitly includes transcript text. Offset pagination applies to the combined context page."), func(ctx context.Context, _ *mcp.CallToolRequest, in ConversationInput) (*mcp.CallToolResult, ConversationResult, error) {
		if in.ConversationID == "" {
			return nil, ConversationResult{}, fmt.Errorf("conversation_id is required")
		}
		title, e := store.Conversation(ctx, in.ConversationID)
		if e != nil {
			return nil, ConversationResult{}, e
		}
		topics, e := store.Topics(ctx, in.ConversationID)
		if e != nil {
			return nil, ConversationResult{}, e
		}
		page, e := store.Query(ctx, memory.Query{ConversationID: in.ConversationID, IncludeTurns: in.IncludeTurns, Limit: in.Limit, Offset: in.Offset})
		if e != nil {
			return nil, ConversationResult{}, e
		}
		status, e := store.GetProcessingStatus(ctx, in.ConversationID)
		if e != nil {
			return nil, ConversationResult{}, e
		}
		return nil, ConversationResult{Experimental: true, ConversationID: in.ConversationID, Title: title, Topics: topics, Context: page, Processing: status}, nil
	})
	mcp.AddTool(s, read("get_context_item", "Read one annotated note, a complete stored transcript turn, or the conversation summary by stable ID. Note results include their source turns."), func(ctx context.Context, _ *mcp.CallToolRequest, in ItemInput) (*mcp.CallToolResult, memory.ItemDetail, error) {
		if in.ID == "" {
			return nil, memory.ItemDetail{}, fmt.Errorf("id is required")
		}
		r, e := store.GetItem(ctx, in.ID)
		return nil, r, e
	})
	mcp.AddTool(s, read("list_topics", "List the topics available in the local memory database, optionally scoped to one conversation."), func(ctx context.Context, _ *mcp.CallToolRequest, in TopicsInput) (*mcp.CallToolResult, memory.TopicList, error) {
		r, e := store.Topics(ctx, in.ConversationID)
		return nil, r, e
	})
	write := func(name, desc string) *mcp.Tool {
		return &mcp.Tool{Name: name, Description: "EXPERIMENTAL: " + desc, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, IdempotentHint: true, OpenWorldHint: &no}}
	}
	mcp.AddTool(s, write("ingest_transcript", "Store one provider-neutral final ASR segment snapshot. Replaying identical input is safe; no audio is fetched or transcribed."), func(ctx context.Context, req *mcp.CallToolRequest, _ IngestInput) (*mcp.CallToolResult, memory.IngestReport, error) {
		// Decode the original wire JSON: schema DTO any fields would round large
		// provider integers through float64 and change immutable source evidence.
		var normalized asrinput.Result
		if e := json.Unmarshal(req.Params.Arguments, &normalized); e != nil {
			return nil, memory.IngestReport{}, e
		}
		r, e := store.IngestTranscript(ctx, normalized)
		return nil, r, e
	})
	mcp.AddTool(s, read("get_processing_status", "Report received transcript coverage, topic-linked and unclassified turns. Pending means analysis has not been completed."), func(ctx context.Context, _ *mcp.CallToolRequest, in StatusInput) (*mcp.CallToolResult, memory.ProcessingStatus, error) {
		if in.ConversationID == "" {
			return nil, memory.ProcessingStatus{}, fmt.Errorf("conversation_id is required")
		}
		r, e := store.GetProcessingStatus(ctx, in.ConversationID)
		return nil, r, e
	})
	mcp.AddTool(s, write("apply_annotations", "Apply a new interpretation revision to a conversation. Supply expected_version for optimistic concurrency; evidence accepts exact full turn IDs or original transcript turn IDs."), func(ctx context.Context, _ *mcp.CallToolRequest, in ApplyAnnotationsInput) (*mcp.CallToolResult, map[string]any, error) {
		a := memory.Annotations{ConversationID: in.ConversationID, AnnotationVersion: in.AnnotationVersion, AnnotationBasis: in.AnnotationBasis, Summary: in.Summary, Title: in.Title, RecordingID: in.RecordingID, Topics: in.Topics, Notes: in.Notes, TurnTopics: in.TurnTopics}
		e := store.ApplyAnnotations(ctx, in.ConversationID, in.ExpectedVersion, a)
		return nil, map[string]any{"conversation_id": in.ConversationID, "annotation_version": in.AnnotationVersion, "applied": e == nil}, e
	})
	mcp.AddTool(s, read("get_annotation_history", "List current and archived annotation revisions for a conversation."), func(ctx context.Context, _ *mcp.CallToolRequest, in HistoryInput) (*mcp.CallToolResult, HistoryResult, error) {
		if in.ConversationID == "" {
			return nil, HistoryResult{}, fmt.Errorf("conversation_id is required")
		}
		r, e := store.GetAnnotationHistory(ctx, in.ConversationID)
		return nil, HistoryResult{Experimental: true, ConversationID: in.ConversationID, Revisions: r}, e
	})
	mcp.AddTool(s, read("list_conversations", "列出本地记忆库中的会话：可按主题、分析状态与录音开始时间过滤，并按录音开始时间倒序游标分页返回摘要。"), func(ctx context.Context, _ *mcp.CallToolRequest, in ListConversationsInput) (*mcp.CallToolResult, memory.ConversationPage, error) {
		after, e := parseRFC3339("started_after", in.StartedAfter)
		if e != nil {
			return nil, memory.ConversationPage{}, e
		}
		before, e := parseRFC3339("started_before", in.StartedBefore)
		if e != nil {
			return nil, memory.ConversationPage{}, e
		}
		r, e := store.ListConversations(ctx, memory.ConversationFilter{Limit: in.Limit, Cursor: in.Cursor, TopicID: in.TopicID, Status: in.Status, StartedAfter: after, StartedBefore: before})
		return nil, r, e
	})
	mcp.AddTool(s, read("get_evidence_context", "读取某个转录轮次及其同一录音内相邻轮次的上下文，用于核对原始证据；超出 max_bytes 预算时会截断并置 Truncated。"), func(ctx context.Context, _ *mcp.CallToolRequest, in GetEvidenceContextInput) (*mcp.CallToolResult, memory.EvidenceContext, error) {
		if in.TurnID == "" {
			return nil, memory.EvidenceContext{}, fmt.Errorf("turn_id is required")
		}
		r, e := store.GetEvidenceContext(ctx, in.TurnID, in.Radius, in.MaxBytes)
		return nil, r, e
	})
	mcp.AddTool(s, read("search_memory", "在原始转录与派生笔记中执行大小写不敏感的子串检索；scope 可选 raw（仅转录）、derived（仅笔记）或 both（全部），并可按主题与录音开始时间过滤。"), func(ctx context.Context, _ *mcp.CallToolRequest, in SearchMemoryInput) (*mcp.CallToolResult, memory.SearchPage, error) {
		if strings.TrimSpace(in.Text) == "" {
			return nil, memory.SearchPage{}, fmt.Errorf("text is required")
		}
		after, e := parseRFC3339("started_after", in.StartedAfter)
		if e != nil {
			return nil, memory.SearchPage{}, e
		}
		before, e := parseRFC3339("started_before", in.StartedBefore)
		if e != nil {
			return nil, memory.SearchPage{}, e
		}
		r, e := store.SearchMemory(ctx, memory.MemorySearch{Text: in.Text, Scope: in.Scope, Limit: in.Limit, Cursor: in.Cursor, TopicID: in.TopicID, StartedAfter: after, StartedBefore: before})
		return nil, r, e
	})
	mcp.AddTool(s, read("list_memory_links", "列出话题之间的记忆关联（proposed 建议 / confirmed 已确认 / rejected 已拒绝），可按状态与话题过滤；返回端点标题、理由、证据轮次与置信度，用于人工复核和避免重复提议。"), func(ctx context.Context, _ *mcp.CallToolRequest, in ListMemoryLinksInput) (*mcp.CallToolResult, MemoryLinkList, error) {
		switch in.Status {
		case "", pipeline.LinkStatusProposed, pipeline.LinkStatusConfirmed, pipeline.LinkStatusRejected:
		default:
			return nil, MemoryLinkList{}, fmt.Errorf("status must be proposed, confirmed, rejected or empty")
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 50
		}
		if limit > 200 {
			limit = 200
		}
		links, e := pipeline.ListLinks(ctx, store.DB(), pipeline.LinkFilter{Status: in.Status, TopicID: in.TopicID, Limit: limit})
		if e != nil {
			return nil, MemoryLinkList{}, e
		}
		views := make([]MemoryLinkView, 0, len(links))
		for _, l := range links {
			views = append(views, memoryLinkView(l))
		}
		return nil, MemoryLinkList{Links: views}, nil
	})
	mcp.AddTool(s, write("review_memory_link", "复核一条记忆关联建议。confirm 把 proposed 变为 confirmed；reject 把 proposed 或 confirmed 变为 rejected；reopen 把 confirmed 或 rejected 退回 proposed 以便重新考虑；重复提交同一动作是幂等的，复核只改状态、不删除记录，也不合并话题。"), func(ctx context.Context, _ *mcp.CallToolRequest, in ReviewMemoryLinkInput) (*mcp.CallToolResult, MemoryLinkView, error) {
		if strings.TrimSpace(in.LinkID) == "" {
			return nil, MemoryLinkView{}, fmt.Errorf("link_id is required")
		}
		l, e := pipeline.ReviewLink(ctx, store.DB(), in.LinkID, in.Action)
		if e != nil {
			return nil, MemoryLinkView{}, e
		}
		return nil, memoryLinkView(l), nil
	})
	return s
}
func main() {
	log.SetOutput(os.Stderr)
	db := flag.String("db", "data/context-memory.db", "experimental SQLite memory database")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatalf("unexpected arguments: %v", flag.Args())
	}
	if *db != ":memory:" {
		if e := os.MkdirAll(filepath.Dir(*db), 0700); e != nil {
			log.Fatal(e)
		}
	}
	store, e := memory.Open(*db)
	if e != nil {
		log.Fatal(e)
	}
	defer store.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if e = newServer(store).Run(ctx, &mcp.StdioTransport{}); e != nil && ctx.Err() == nil {
		log.Fatal(e)
	}
}
