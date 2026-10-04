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
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/memory"
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
