package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"recordingmcp/internal/retention"
	"strings"
)

// HighlightAudioClip resolves only a server-owned anchor; clients cannot choose
// paths. The retention read lock spans source checks through the final stream.
func (s *Service) HighlightAudioClip(ctx context.Context, id string) (*AudioClip, error) {
	release, err := retention.Lock(ctx, s.audioRoot, false)
	if err != nil {
		return nil, err
	}
	handedOff := false
	defer func() {
		if !handedOff {
			release()
		}
	}()
	h, err := s.GetHighlight(ctx, id)
	if err != nil {
		return nil, err
	}
	if h.StartMS == nil {
		return nil, audioError(CodeAudioOffsetUnknown, "highlight has no audio timing", 409)
	}
	start := *h.StartMS
	end := start
	if h.EndMS != nil {
		end = *h.EndMS
	}
	src, err := s.newAudioSource(ctx, h.ConversationID, h.RecordingID)
	if err != nil {
		return nil, err
	}
	switch src.state {
	case AudioDeleted:
		return nil, audioError(CodeAudioDeleted, "audio deleted", 410)
	case AudioMissing:
		return nil, audioError(CodeAudioMissing, "audio missing", 404)
	case AudioUnavailable:
		return nil, audioError(CodeAudioUnavailable, "audio unavailable", 404)
	}
	if h.AnchorKind == "point" {
		start -= 5000
		if start < 0 {
			start = 0
		}
		end = *h.StartMS + 5000
		if end > src.size/pcmBytesPerMS {
			end = src.size / pcmBytesPerMS
		}
	}
	if end-start > 120000 {
		end = start + 120000
	}
	if start < 0 || end <= start || end > src.size/pcmBytesPerMS {
		return nil, audioError(CodeAudioOffsetOutOfRange, "highlight audio out of range", 409)
	}
	var forbidden int
	err = s.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM forget_tombstones f WHERE (f.recording_id=? OR (f.scope='conversation' AND f.conversation_id=?)) AND (f.scope IN ('recording','conversation') OR f.start_ms IS NULL OR f.end_ms IS NULL OR (? < f.end_ms AND ? > f.start_ms))`, h.RecordingID, h.ConversationID, start, end).Scan(&forbidden)
	if err != nil {
		return nil, err
	}
	if forbidden > 0 {
		return nil, &Error{Code: "source_forgotten", Status: 410, Message: "audio intersects forgotten source"}
	}
	from, to, ok := src.clipBounds(&start, &end)
	if !ok {
		return nil, audioError(CodeAudioOffsetOutOfRange, "highlight audio out of range", 409)
	}
	root, err := os.OpenRoot(s.audioRoot)
	if err != nil {
		return nil, audioError(CodeAudioMissing, "audio missing", 404)
	}
	defer root.Close()
	rel, err := filepath.Rel(s.audioRoot, src.path)
	if err != nil || filepath.IsAbs(rel) || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, audioError(CodeAudioMissing, "audio missing", 404)
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, audioError(CodeAudioMissing, "audio missing", 404)
	}
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() < to {
		f.Close()
		return nil, audioError(CodeAudioMissing, "audio missing", 404)
	}
	if _, err = f.Seek(from, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	handedOff = true
	return &AudioClip{release: release, RecordingID: h.RecordingID, StartMS: start, EndMS: end, PCM: f, Size: to - from}, nil
}
