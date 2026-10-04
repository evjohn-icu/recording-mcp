package asrinput

import (
	"math"
	"testing"
)

func TestValidateNeutralRangeAndOffsetRules(t *testing.T) {
	offset, duration, start, end := int64(300000), int64(1000), int64(0), int64(1002)
	r := Result{ContractVersion: ContractVersion, Source: Source{Provider: "x", ProviderRequestID: "req", DeviceID: "d", RecordingID: "r", SegmentID: "s", SegmentOffsetMS: offset, SegmentDurationMS: duration, TranscriptRevision: "v1", CompletionStatus: "final"}, Turns: []Turn{{ID: "turn-1", OriginalIndex: 1, Text: "x", LocalStartMS: &start, LocalEndMS: &end, GlobalStartMS: &offset, GlobalEndMS: ptr(offset + end)}}}
	if err := Validate(r); err != nil {
		t.Fatalf("generic boundary tolerance should allow source value without rewriting: %v", err)
	}
	badLocal := duration + AllowedBoundaryToleranceMS + 1
	r.Turns[0].LocalEndMS = &badLocal
	r.Turns[0].GlobalEndMS = ptr(offset + badLocal)
	if err := Validate(r); err == nil {
		t.Fatal("timestamp past generic tolerance accepted")
	}
	r.Source.SegmentOffsetMS = math.MaxInt64 - duration
	r.Turns[0].LocalEndMS = &end
	r.Turns[0].GlobalEndMS = ptr(math.MaxInt64)
	if err := Validate(r); err == nil {
		t.Fatal("offset leaves no tolerance space; overflow risk accepted")
	}
}

func ptr(v int64) *int64 { return &v }
