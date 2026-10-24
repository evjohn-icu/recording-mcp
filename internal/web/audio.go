package web

import (
	"bytes"
	"io"
	"net/http"
	"strconv"

	"recordingmcp/internal/app"
)

// handleAudio streams one turn's audio slice as a self-contained WAV file. The
// clip is resolved to a byte range of the recording's PCM and written with the
// header first; failures are JSON errors, never a truncated audio body.
func (s *Server) handleAudio(w http.ResponseWriter, r *http.Request) {
	clip, err := s.svc.AudioClip(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	defer clip.Close()
	writeAudioClip(w, r, clip)
}

func writeAudioClip(w http.ResponseWriter, r *http.Request, clip *app.AudioClip) {
	var header bytes.Buffer
	if err := app.WriteWAVHeader(&header, clip.Size); err != nil {
		writeBadRequest(w, "audio clip exceeds WAV size limit")
		return
	}
	h := w.Header()
	h.Set("X-Audio-Start-MS", strconv.FormatInt(clip.StartMS, 10))
	h.Set("X-Audio-End-MS", strconv.FormatInt(clip.EndMS, 10))
	h.Set("X-Recording-ID", clip.RecordingID)
	h.Set("Content-Type", "audio/wav")
	h.Set("Content-Length", strconv.FormatInt(44+clip.Size, 10))
	h.Set("Cache-Control", "no-store")
	// Ranges are not advertised: each clip is its own small resource and the
	// browser plays it whole.
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(header.Bytes()); err != nil {
		return
	}
	_, _ = io.CopyN(w, clip.PCM, clip.Size)
}
