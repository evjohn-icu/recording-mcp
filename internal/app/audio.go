package app

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"recordingmcp/internal/memory"
	"recordingmcp/internal/retention"
)

// pcmBytesPerMS is the fixed ingest contract: 16 kHz mono s16le .pcm is 32
// bytes per millisecond of recording time (contracts/ingest-v1/README.md).
const pcmBytesPerMS = 32

// syncRow is the local download state of one provider recording, resolved
// through the source_attachments provenance bridge:
//
//	memory recording id M <- source_attachments.recording_id
//	provider pair (R,D)   <- json_extract(source_json,'$.recording_id'/'$.device_id')
//	sync_recordings       <- (recording_id=R, device_id=D)
//
// The memory id is a hash and sync_recordings stores the provider UUID, so no
// direct join exists; recordings.source_file is caller-supplied text and is
// never used to locate a file.
type syncRow struct {
	providerRecordingID string
	deviceID            string
	state               string
	stitchedPath        sql.NullString // sync_recordings.stitched_path is nullable
}

// syncRowForRecording returns ok=false when the recording has no context-sync
// provenance or no matching sync row.
func (s *Service) syncRowForRecording(ctx context.Context, conversationID, memoryRecordingID string) (syncRow, bool, error) {
	var device, provider sql.NullString
	err := s.Store.DB().QueryRowContext(ctx,
		`SELECT json_extract(source_json,'$.device_id'), json_extract(source_json,'$.recording_id')
		 FROM source_attachments WHERE conversation_id=? AND recording_id=? ORDER BY id LIMIT 1`,
		conversationID, memoryRecordingID).Scan(&device, &provider)
	if errors.Is(err, sql.ErrNoRows) {
		return syncRow{}, false, nil
	}
	if err != nil {
		return syncRow{}, false, err
	}
	if !device.Valid || !provider.Valid || strings.TrimSpace(device.String) == "" || strings.TrimSpace(provider.String) == "" {
		return syncRow{}, false, nil
	}
	if memory.RecordingID(device.String, provider.String) != memoryRecordingID {
		return syncRow{}, false, nil
	}
	row := syncRow{providerRecordingID: provider.String, deviceID: device.String}
	err = s.Store.DB().QueryRowContext(ctx,
		`SELECT state, stitched_path FROM sync_recordings WHERE recording_id=? AND device_id=?`,
		row.providerRecordingID, row.deviceID).Scan(&row.state, &row.stitchedPath)
	if errors.Is(err, sql.ErrNoRows) {
		return syncRow{}, false, nil
	}
	if err != nil {
		return syncRow{}, false, err
	}
	return row, true, nil
}

// audioSource is the availability of one recording's local audio, resolved
// once per request. state is one of available|deleted|missing|unavailable.
type audioSource struct {
	path  string
	size  int64
	state string
}

func (s *Service) newAudioSource(ctx context.Context, conversationID, recordingID string) (audioSource, error) {
	src := audioSource{state: AudioUnavailable}
	var cleanupPending bool
	if err := s.Store.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM forget_tombstones WHERE (recording_id=? OR (scope='conversation' AND conversation_id=?)) AND status!='complete')`, recordingID, conversationID).Scan(&cleanupPending); err != nil {
		return src, err
	}
	if cleanupPending {
		return src, nil
	}
	row, ok, err := s.syncRowForRecording(ctx, conversationID, recordingID)
	if err != nil {
		return src, err
	}
	if !ok {
		return src, nil
	}
	if row.state == "deleted" {
		src.state = AudioDeleted
		return src, nil
	}
	if !row.stitchedPath.Valid || strings.TrimSpace(row.stitchedPath.String) == "" {
		return src, nil
	}
	path, size, ok := s.resolveAudioPath(row.stitchedPath.String)
	if !ok {
		src.state = AudioMissing
		return src, nil
	}
	src.path, src.size, src.state = path, size, AudioAvailable
	return src, nil
}

// resolveAudioPath accepts only an existing .pcm file inside the audio
// directory. Anything else (wrong suffix, traversal, symlink target recorded
// outside the tree) is treated as a missing file, never served.
func (s *Service) resolveAudioPath(stitched string) (string, int64, bool) {
	if s.audioRoot == "" {
		return "", 0, false
	}
	if !strings.HasSuffix(strings.ToLower(stitched), ".pcm") {
		return "", 0, false
	}
	abs, err := filepath.Abs(stitched)
	if err != nil {
		return "", 0, false
	}
	rel, err := filepath.Rel(s.audioRoot, abs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", 0, false
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.Mode().IsRegular() {
		return "", 0, false
	}
	return abs, info.Size(), true
}

// status reports the audio status of one turn offset.
func (a audioSource) status(start, end *int64) string {
	if start == nil || end == nil {
		return AudioOffsetUnknown
	}
	switch a.state {
	case AudioDeleted, AudioMissing, AudioUnavailable:
		return a.state
	}
	if _, _, ok := a.clipBounds(start, end); !ok {
		return AudioOutOfRange
	}
	return AudioAvailable
}

// clipBounds converts a turn's millisecond range into file byte offsets. The
// end is clamped to the file size; a start at or past the end of the file is
// out of range instead of an empty clip.
func (a audioSource) clipBounds(start, end *int64) (int64, int64, bool) {
	if start == nil || end == nil || a.state != AudioAvailable {
		return 0, 0, false
	}
	if *end <= *start {
		return 0, 0, false
	}
	if *start < 0 || *end < 0 || *start > int64(^uint64(0)>>1)/pcmBytesPerMS || *end > int64(^uint64(0)>>1)/pcmBytesPerMS {
		return 0, 0, false
	}
	from := *start * pcmBytesPerMS
	to := *end * pcmBytesPerMS
	if from < 0 || from >= a.size {
		return 0, 0, false
	}
	if to > a.size {
		to = a.size
	}
	if to <= from {
		return 0, 0, false
	}
	return from, to, true
}

// AudioClip is one playable slice of a recording: the caller writes a WAV
// header and then copies Size bytes from PCM, which is already positioned at
// StartMS.
type AudioClip struct {
	release     func()
	RecordingID string
	StartMS     int64
	EndMS       int64
	PCM         *os.File
	Size        int64 // PCM bytes, excluding the 44-byte WAV header
}

// Close releases the clip's file handle.
func (c *AudioClip) Close() error {
	if c == nil || c.PCM == nil {
		return nil
	}
	err := c.PCM.Close()
	if c.release != nil {
		c.release()
		c.release = nil
	}
	return err
}

// AudioClip resolves and opens one turn's audio. Only current-state turns are
// playable: a stale revision's turn is not part of what the UI shows.
func (s *Service) AudioClip(ctx context.Context, turnID string) (*AudioClip, error) {
	release, lockErr := retention.Lock(ctx, s.audioRoot, false)
	if lockErr != nil {
		return nil, lockErr
	}
	handedOff := false
	defer func() {
		if !handedOff {
			release()
		}
	}()
	if strings.TrimSpace(turnID) == "" {
		return nil, validationError("turn_id is required")
	}
	var conversationID, recordingID string
	var start, end sql.NullInt64
	err := s.Store.DB().QueryRowContext(ctx,
		`SELECT conversation_id, recording_id, global_start_ms, global_end_ms FROM turns t WHERE t.id=? AND `+memory.CurrentTurnPredicate("t"),
		turnID).Scan(&conversationID, &recordingID, &start, &end)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundError("turn not found: " + turnID)
	}
	if err != nil {
		return nil, err
	}
	if !start.Valid || !end.Valid {
		return nil, audioError(CodeAudioOffsetUnknown, "该片段缺少时间信息，无法定位音频", 409)
	}
	src, err := s.newAudioSource(ctx, conversationID, recordingID)
	if err != nil {
		return nil, err
	}
	switch src.state {
	case AudioDeleted:
		return nil, audioError(CodeAudioDeleted, "录音已删除（转写与归纳保留）", 410)
	case AudioMissing:
		return nil, audioError(CodeAudioMissing, "本机找不到音频文件", 404)
	case AudioUnavailable:
		return nil, audioError(CodeAudioUnavailable, "此录音没有本地音频", 404)
	}
	from, to, ok := src.clipBounds(&start.Int64, &end.Int64)
	if !ok {
		return nil, audioError(CodeAudioOffsetOutOfRange, "音频时间超出录音范围", 409)
	}
	root, err := os.OpenRoot(s.audioRoot)
	if err != nil {
		return nil, audioError(CodeAudioMissing, "本机找不到音频文件", 404)
	}
	defer root.Close()
	rel, err := filepath.Rel(s.audioRoot, src.path)
	if err != nil || filepath.IsAbs(rel) || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, audioError(CodeAudioMissing, "本机找不到音频文件", 404)
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, audioError(CodeAudioMissing, "本机找不到音频文件", 404)
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, audioError(CodeAudioMissing, "本机找不到音频文件", 404)
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		f.Close()
		return nil, audioError(CodeAudioMissing, "本机找不到音频文件", 404)
	}
	handedOff = true
	return &AudioClip{release: release, RecordingID: recordingID, StartMS: start.Int64, EndMS: end.Int64, PCM: f, Size: to - from}, nil
}

// WriteWAVHeader writes the 44-byte RIFF/WAVE header of 16 kHz mono s16le PCM
// with dataBytes of payload. All integers are little endian.
func WriteWAVHeader(w io.Writer, dataBytes int64) error {
	const (
		bitsPerSample = 16
		channels      = 1
		sampleRate    = 16000
	)
	if dataBytes < 0 || dataBytes > int64(^uint32(0))-36 {
		return errors.New("wav data size exceeds RIFF limits")
	}
	blockAlign := channels * bitsPerSample / 8
	var h [44]byte
	copy(h[0:4], "RIFF")
	binary.LittleEndian.PutUint32(h[4:8], uint32(36+dataBytes))
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:20], 16)
	binary.LittleEndian.PutUint16(h[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:24], channels)
	binary.LittleEndian.PutUint32(h[24:28], sampleRate)
	binary.LittleEndian.PutUint32(h[28:32], uint32(sampleRate*blockAlign))
	binary.LittleEndian.PutUint16(h[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(h[34:36], bitsPerSample)
	copy(h[36:40], "data")
	binary.LittleEndian.PutUint32(h[40:44], uint32(dataBytes))
	_, err := w.Write(h[:])
	return err
}
