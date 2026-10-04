package memory

import (
	"bytes"
	"encoding/json"
)

// Transcript is the private normalized output produced by asr_full.py.
type Transcript struct {
	AudioFileName   string  `json:"audio_file_name"`
	DurationSeconds float64 `json:"duration_seconds"`
	Coverage        struct {
		StartMS         int64   `json:"start_ms"`
		EndMS           int64   `json:"end_ms"`
		DurationSeconds float64 `json:"duration_seconds,omitempty"`
	} `json:"coverage"`
	Turns []Turn `json:"turns"`
}
type Turn struct {
	ID               string  `json:"id"`
	SegmentIndex     int     `json:"segment_index"`
	SegmentStartMS   int64   `json:"segment_start_ms"`
	SegmentEndMS     int64   `json:"segment_end_ms"`
	LocalStartMS     *int64  `json:"local_start_ms"`
	LocalEndMS       *int64  `json:"local_end_ms"`
	GlobalStartMS    *int64  `json:"global_start_ms"`
	GlobalEndMS      *int64  `json:"global_end_ms"`
	SpeakerLocalID   *string `json:"speaker_local_id"`
	SpeakerScopedID  *string `json:"speaker_scoped_id"`
	Text             string  `json:"text"`
	Definite         *bool   `json:"definite"`
	OverlapDuplicate bool    `json:"overlap_duplicate"`
	DuplicateOf      *string `json:"duplicate_of"`
}
type Annotations struct {
	Version           int                   `json:"version,omitempty"`
	AnnotationVersion string                `json:"annotation_version,omitempty"`
	AnnotationBasis   string                `json:"annotation_basis,omitempty"`
	Summary           string                `json:"summary,omitempty"`
	ConversationID    string                `json:"conversation_id"`
	Title             string                `json:"title"`
	RecordingID       string                `json:"recording_id"`
	Topics            []TopicAnnotation     `json:"topics"`
	Notes             []NoteAnnotation      `json:"notes"`
	TurnTopics        []TurnTopicAnnotation `json:"turn_topics"`
}
type TopicAnnotation struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}
type NoteAnnotation struct {
	ID              string   `json:"id"`
	Kind            string   `json:"kind"`
	TopicIDs        []string `json:"topic_ids"`
	Summary         string   `json:"summary"`
	EvidenceTurnIDs []string `json:"evidence_turn_ids"`
}
type TurnTopicAnnotation struct {
	TurnID   string   `json:"turn_id"`
	TopicIDs []string `json:"topic_ids"`
}
type ImportReport struct {
	ConversationID            string `json:"conversation_id"`
	RecordingID               string `json:"recording_id"`
	Inserted                  bool   `json:"inserted"`
	RetainedTurns             int    `json:"retained_turns"`
	ExcludedOverlapDuplicates int    `json:"excluded_overlap_duplicates"`
	ExcludedBlankTurns        int    `json:"excluded_blank_turns"`
	Topics                    int    `json:"topics"`
	Notes                     int    `json:"notes"`
	SourceSHA256              string `json:"source_sha256"`
	TranscriptVersion         string `json:"transcript_version"`
}
type Item struct {
	ID                 string   `json:"id"`
	Kind               string   `json:"kind"`
	ConversationID     string   `json:"conversation_id"`
	RecordingID        string   `json:"recording_id,omitempty"`
	Title              string   `json:"title,omitempty"`
	Summary            string   `json:"summary,omitempty"`
	TopicIDs           []string `json:"topic_ids,omitempty"`
	EvidenceTurnIDs    []string `json:"evidence_turn_ids,omitempty"`
	Content            string   `json:"content,omitempty"`
	SpeakerScopedID    *string  `json:"speaker_scoped_id,omitempty"`
	SpeakerEntityID    *string  `json:"speaker_entity_id"`
	GlobalStartMS      *int64   `json:"global_start_ms"`
	GlobalEndMS        *int64   `json:"global_end_ms"`
	SourceSHA256       string   `json:"source_sha256,omitempty"`
	TurnID             string   `json:"turn_id,omitempty"`
	TranscriptVersion  string   `json:"transcript_version,omitempty"`
	ClaimMode          string   `json:"claim_mode,omitempty"`
	InterpretationNote string   `json:"interpretation_note,omitempty"`
	AnnotationVersion  string   `json:"annotation_version,omitempty"`
}
type Query struct {
	Query           string `json:"query,omitempty"`
	TopicID         string `json:"topic_id,omitempty"`
	ConversationID  string `json:"conversation_id,omitempty"`
	RecordingID     string `json:"recording_id,omitempty"`
	SpeakerScopedID string `json:"speaker_scoped_id,omitempty"`
	IncludeTurns    bool   `json:"include_turns,omitempty"`
	Limit           int    `json:"limit,omitempty"`
	Offset          int    `json:"offset,omitempty"`
}
type Page struct {
	Experimental bool   `json:"experimental"`
	Items        []Item `json:"items"`
	Total        int    `json:"total"`
	NextOffset   *int   `json:"next_offset,omitempty"`
}
type ItemDetail struct {
	Experimental bool   `json:"experimental"`
	Item         Item   `json:"item"`
	Evidence     []Item `json:"evidence,omitempty"`
}
type Topic struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	ConversationID string `json:"conversation_id"`
}
type TopicList struct {
	Experimental bool    `json:"experimental"`
	Topics       []Topic `json:"topics"`
}

func decodeStrict(raw []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}
