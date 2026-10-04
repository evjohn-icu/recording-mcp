package asrinput

import (
	"errors"
	"fmt"
	"math"
)

// AllowedBoundaryToleranceMS supports provider timestamp rounding at a segment edge.
// It is only a validation allowance: adapters preserve the unmodified source value and warning.
const AllowedBoundaryToleranceMS int64 = 5

// Validate checks only supplier-independent structure. Provider-specific rules remain in adapters.
func Validate(r Result) error {
	if r.ContractVersion != ContractVersion {
		return fmt.Errorf("unsupported contract_version %q", r.ContractVersion)
	}
	s := r.Source
	if s.Provider == "" || s.ProviderRequestID == "" || s.DeviceID == "" || s.RecordingID == "" || s.SegmentID == "" || s.TranscriptRevision == "" {
		return errors.New("normalized result missing required source identity")
	}
	if s.CompletionStatus != "final" {
		return errors.New("normalized result completion_status must be final")
	}
	if s.SegmentOffsetMS < 0 || s.SegmentDurationMS <= 0 || s.SegmentDurationMS > math.MaxInt64-AllowedBoundaryToleranceMS || s.SegmentOffsetMS > math.MaxInt64-s.SegmentDurationMS-AllowedBoundaryToleranceMS {
		return errors.New("invalid segment offset or duration")
	}
	seenID, seenIndex := map[string]bool{}, map[int]bool{}
	for i, t := range r.Turns {
		if t.ID == "" || seenID[t.ID] {
			return fmt.Errorf("turn %d has missing or duplicate id", i)
		}
		seenID[t.ID] = true
		if t.OriginalIndex < 1 || seenIndex[t.OriginalIndex] {
			return fmt.Errorf("turn %s has missing or duplicate original_index", t.ID)
		}
		seenIndex[t.OriginalIndex] = true
		if t.LocalStartMS != nil && (*t.LocalStartMS < 0 || *t.LocalStartMS > s.SegmentDurationMS+AllowedBoundaryToleranceMS) || t.LocalEndMS != nil && (*t.LocalEndMS < 0 || *t.LocalEndMS > s.SegmentDurationMS+AllowedBoundaryToleranceMS) || t.GlobalStartMS != nil && *t.GlobalStartMS < 0 || t.GlobalEndMS != nil && *t.GlobalEndMS < 0 {
			return fmt.Errorf("turn %s contains a negative normalized timestamp", t.ID)
		}
		if (t.LocalStartMS == nil) != (t.GlobalStartMS == nil) || (t.LocalEndMS == nil) != (t.GlobalEndMS == nil) {
			return fmt.Errorf("turn %s global/local timestamp nullability differs", t.ID)
		}
		if t.LocalStartMS != nil && *t.GlobalStartMS != s.SegmentOffsetMS+*t.LocalStartMS || t.LocalEndMS != nil && *t.GlobalEndMS != s.SegmentOffsetMS+*t.LocalEndMS {
			return fmt.Errorf("turn %s global time does not equal segment offset plus local time", t.ID)
		}
		if t.LocalStartMS != nil && t.LocalEndMS != nil && *t.LocalEndMS < *t.LocalStartMS {
			return fmt.Errorf("turn %s end precedes start", t.ID)
		}
		if t.Confidence != nil && (math.IsNaN(*t.Confidence) || math.IsInf(*t.Confidence, 0) || *t.Confidence < 0 || *t.Confidence > 1) {
			return fmt.Errorf("turn %s confidence outside 0..1", t.ID)
		}
	}
	return nil
}
