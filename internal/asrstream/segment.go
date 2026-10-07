package asrstream

import "fmt"

// Segment is one recognition window over the source audio, in milliseconds
// from the start of the recording.
type Segment struct {
	Index   int
	StartMS int64
	EndMS   int64
}

// PlanSegments splits a recording into fixed windows that share overlapMS of
// audio with their neighbours, so speech straddling a boundary is not lost.
// It mirrors the reference runner: windows advance by segmentMS-overlapMS and
// the last window is clamped to the recording duration.
func PlanSegments(durationMS, segmentMS, overlapMS int64) ([]Segment, error) {
	if segmentMS <= 0 {
		return nil, fmt.Errorf("asrstream: segment length must be positive, got %dms", segmentMS)
	}
	if overlapMS < 0 || overlapMS >= segmentMS {
		return nil, fmt.Errorf("asrstream: overlap must be in [0, segment length), got %dms", overlapMS)
	}
	var segments []Segment
	step := segmentMS - overlapMS
	for start := int64(0); start < durationMS; start += step {
		end := start + segmentMS
		if end > durationMS {
			end = durationMS
		}
		segments = append(segments, Segment{Index: len(segments) + 1, StartMS: start, EndMS: end})
		if end >= durationMS {
			break
		}
	}
	return segments, nil
}
