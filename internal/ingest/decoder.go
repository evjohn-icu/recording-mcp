package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// FFmpegDecoder shells out to ffprobe/ffmpeg with argument arrays, never a
// shell, so a filename can never become a command.
type FFmpegDecoder struct {
	FFmpeg  string
	FFprobe string
}

func (d FFmpegDecoder) ffmpeg() string {
	if strings.TrimSpace(d.FFmpeg) == "" {
		return "ffmpeg"
	}
	return d.FFmpeg
}

func (d FFmpegDecoder) ffprobe() string {
	if strings.TrimSpace(d.FFprobe) == "" {
		return "ffprobe"
	}
	return d.FFprobe
}

type probeOutput struct {
	Format struct {
		Duration   string `json:"duration"`
		FormatName string `json:"format_name"`
	} `json:"format"`
	Streams []struct {
		CodecType  string `json:"codec_type"`
		SampleRate string `json:"sample_rate"`
		Channels   int    `json:"channels"`
	} `json:"streams"`
}

// Probe returns the first audio stream's rate/channels and the container
// duration. A file without an audio stream or without a positive duration is
// rejected: an extension whitelist alone never proves the bytes are audio.
func (d FFmpegDecoder) Probe(ctx context.Context, path string) (AudioInfo, error) {
	cmd := exec.CommandContext(ctx, d.ffprobe(),
		"-v", "error",
		"-show_entries", "format=duration,format_name",
		"-show_entries", "stream=codec_type,sample_rate,channels",
		"-of", "json", path,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return AudioInfo{}, fmt.Errorf("ffprobe: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var parsed probeOutput
	if err := json.Unmarshal(out, &parsed); err != nil {
		return AudioInfo{}, fmt.Errorf("ffprobe output: %w", err)
	}
	info := AudioInfo{Format: parsed.Format.FormatName}
	seconds, err := strconv.ParseFloat(parsed.Format.Duration, 64)
	if err != nil || seconds <= 0 {
		return AudioInfo{}, fmt.Errorf("ffprobe: invalid duration %q", parsed.Format.Duration)
	}
	info.DurationMS = int64(seconds * 1000)
	foundAudio := false
	for _, stream := range parsed.Streams {
		if stream.CodecType != "audio" {
			continue
		}
		foundAudio = true
		info.SampleRate, _ = strconv.Atoi(stream.SampleRate)
		info.Channels = stream.Channels
		break
	}
	if !foundAudio {
		return AudioInfo{}, errors.New("ffprobe: no audio stream")
	}
	if info.DurationMS <= 0 {
		return AudioInfo{}, errors.New("ffprobe: zero duration")
	}
	return info, nil
}

// ToPCM decodes src to 16 kHz mono signed 16-bit little-endian PCM at dst. The
// output is written to a sibling temp file and renamed, so a killed worker can
// never leave a half-written PCM file that a later attempt would trust.
func (d FFmpegDecoder) ToPCM(ctx context.Context, src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	_ = os.Remove(tmp)
	cmd := exec.CommandContext(ctx, d.ffmpeg(),
		"-nostdin", "-v", "error", "-y",
		"-i", src,
		"-ac", "1", "-ar", "16000",
		"-f", "s16le", "-acodec", "pcm_s16le",
		tmp,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	st, err := os.Stat(tmp)
	if err != nil {
		return err
	}
	if st.Size() == 0 {
		_ = os.Remove(tmp)
		return errors.New("ffmpeg: produced empty PCM")
	}
	return os.Rename(tmp, dst)
}
