// Package eval implements a deterministic, offline evaluation harness for the
// context-building pipeline. It ingests a hand-curated synthetic ASR corpus,
// runs the extract and publish stages with the synthetic decision/generation
// engines (no network, no model weights), inserts curated topic links and then
// measures how well contextbuild.Build serves a fixed question set.
//
// Everything is deterministic: fixture ids and timestamps are fixed, the
// processing order is stable and no wall-clock value ever feeds a fixture
// reference. The only non-deterministic value produced is latency_ms.
package eval

import (
	"encoding/json"
	"fmt"
	"os"

	"recordingmcp/internal/asrinput"
)

// Corpus is the top-level shape of fixtures/eval/corpus.json.
type Corpus struct {
	Description   string         `json:"description"`
	Conversations []Conversation `json:"conversations"`
}

// Conversation is one synthetic conversation. Its Segments are full
// asr-input/0.1 snapshots, so a segment decodes straight into
// asrinput.Result without loss.
type Conversation struct {
	ID          string            `json:"conversation_id"`
	Description string            `json:"description"`
	Segments    []asrinput.Result `json:"segments"`
}

// LoadCorpus reads and validates the corpus fixture.
func LoadCorpus(path string) (Corpus, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Corpus{}, fmt.Errorf("read corpus: %w", err)
	}
	var corpus Corpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		return Corpus{}, fmt.Errorf("decode corpus: %w", err)
	}
	if len(corpus.Conversations) == 0 {
		return Corpus{}, fmt.Errorf("corpus has no conversations")
	}
	seenConv := map[string]bool{}
	seenSegment := map[string]bool{}
	for ci, conv := range corpus.Conversations {
		if conv.ID == "" {
			return Corpus{}, fmt.Errorf("conversation %d has no id", ci)
		}
		if seenConv[conv.ID] {
			return Corpus{}, fmt.Errorf("duplicate conversation id %q", conv.ID)
		}
		seenConv[conv.ID] = true
		if len(conv.Segments) == 0 {
			return Corpus{}, fmt.Errorf("conversation %q has no segments", conv.ID)
		}
		for si, seg := range conv.Segments {
			if err := validateSegment(conv.ID, si, seg); err != nil {
				return Corpus{}, err
			}
			key := seg.Source.RecordingID + "/" + seg.Source.SegmentID + "/" + seg.Source.TranscriptRevision
			if seenSegment[key] {
				return Corpus{}, fmt.Errorf("duplicate segment revision %q", key)
			}
			seenSegment[key] = true
		}
	}
	return corpus, nil
}

// SegmentCount returns the total number of segment snapshots in the corpus.
func (c Corpus) SegmentCount() int {
	n := 0
	for _, conv := range c.Conversations {
		n += len(conv.Segments)
	}
	return n
}

func validateSegment(convID string, idx int, seg asrinput.Result) error {
	if seg.ContractVersion != asrinput.ContractVersion {
		return fmt.Errorf("conversation %q segment %d: contract_version %q, want %q",
			convID, idx, seg.ContractVersion, asrinput.ContractVersion)
	}
	src := seg.Source
	if src.DeviceID == "" || src.RecordingID == "" || src.ConversationID == "" || src.SegmentID == "" {
		return fmt.Errorf("conversation %q segment %d: device/recording/conversation/segment id required", convID, idx)
	}
	if src.TranscriptRevision == "" {
		return fmt.Errorf("conversation %q segment %d: transcript_revision required", convID, idx)
	}
	if src.CompletionStatus != "final" {
		return fmt.Errorf("conversation %q segment %d: completion_status %q, want final", convID, idx, src.CompletionStatus)
	}
	if src.SegmentDurationMS <= 0 {
		return fmt.Errorf("conversation %q segment %d: segment_duration_ms must be positive", convID, idx)
	}
	if len(seg.Turns) == 0 {
		return fmt.Errorf("conversation %q segment %d: no turns", convID, idx)
	}
	seen := map[string]bool{}
	for ti, turn := range seg.Turns {
		if turn.ID == "" {
			return fmt.Errorf("conversation %q segment %d turn %d: id required", convID, idx, ti)
		}
		if seen[turn.ID] {
			return fmt.Errorf("conversation %q segment %d: duplicate turn id %q", convID, idx, turn.ID)
		}
		seen[turn.ID] = true
	}
	return nil
}
