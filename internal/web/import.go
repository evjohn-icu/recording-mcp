package web

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"recordingmcp/internal/app"
	"recordingmcp/internal/ingest"
	"recordingmcp/internal/memory"
)

// maxUploadBody leaves room for multipart framing around the 2 GiB file.
const maxUploadBody = ingest.MaxUploadBytes + (10 << 20)

// handleImport accepts one multipart file, streams it to the importer and
// returns 202 for a new job or 200 for a re-upload of identical bytes.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if s.importer == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: errorDetail{
			Code: "import_disabled", Message: "this server has no audio importer configured",
			Details: map[string]any{"action": "restart context-web with --asr-provider and an audio directory"},
		}})
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		writeJSON(w, http.StatusUnsupportedMediaType, errorBody{Error: errorDetail{
			Code: "unsupported_media_type", Message: "expected multipart/form-data",
			Details: map[string]any{"action": "upload the audio as the multipart field named file"},
		}})
		return
	}
	reader, err := r.MultipartReader()
	if err != nil {
		writeImportError(w, err)
		return
	}
	var fileParts int
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeImportError(w, err)
			return
		}
		if part.FormName() != "file" {
			_, _ = io.Copy(io.Discard, part)
			part.Close()
			continue
		}
		fileParts++
		if fileParts > 1 {
			part.Close()
			writeJSON(w, http.StatusBadRequest, errorBody{Error: errorDetail{
				Code: "multiple_files", Message: "only one audio file per upload is supported",
				Details: map[string]any{"action": "upload the files one at a time"},
			}})
			return
		}
		if strings.TrimSpace(part.FileName()) == "" {
			part.Close()
			writeJSON(w, http.StatusBadRequest, errorBody{Error: errorDetail{
				Code: "missing_filename", Message: "the file part has no filename",
				Details: map[string]any{"action": "select a .wav/.m4a/.mp3 file"},
			}})
			return
		}
		result, err := s.importer.Import(r.Context(), part, part.FileName(), -1)
		part.Close()
		if err != nil {
			writeImportError(w, err)
			return
		}
		status := http.StatusAccepted
		if result.Duplicate {
			status = http.StatusOK
		}
		writeJSON(w, status, importResponse(result))
		return
	}
	writeJSON(w, http.StatusBadRequest, errorBody{Error: errorDetail{
		Code: "missing_file", Message: "no file part found",
		Details: map[string]any{"action": "upload the audio as the multipart field named file"},
	}})
}

type importAudio struct {
	FileName   string `json:"filename"`
	Bytes      int64  `json:"bytes"`
	DurationMS int64  `json:"duration_ms"`
	Format     string `json:"format"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
}

type importResultBody struct {
	ConversationID string      `json:"conversation_id"`
	RecordingID    string      `json:"recording_id"`
	JobID          string      `json:"job_id"`
	Duplicate      bool        `json:"duplicate"`
	Audio          importAudio `json:"audio"`
}

func importResponse(result ingest.ImportResult) importResultBody {
	return importResultBody{
		ConversationID: result.ConversationID,
		RecordingID:    result.RecordingID,
		JobID:          result.JobID,
		Duplicate:      result.Duplicate,
		Audio: importAudio{
			FileName: result.FileName, Bytes: result.Bytes, DurationMS: result.Audio.DurationMS,
			Format: result.Audio.Format, SampleRate: result.Audio.SampleRate, Channels: result.Audio.Channels,
		},
	}
}

// writeImportError maps importer and transport failures onto the JSON error
// envelope. Every failure carries a next action where one exists.
func writeImportError(w http.ResponseWriter, err error) {
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		writeJSON(w, http.StatusRequestEntityTooLarge, errorBody{Error: errorDetail{
			Code: "file_too_large", Message: "audio upload exceeds the 2 GiB limit",
			Details: map[string]any{"action": "compress or trim the audio before uploading"},
		}})
		return
	}
	var ierr *ingest.Error
	if errors.As(err, &ierr) {
		status := ierr.Status
		if status == 0 {
			status = http.StatusBadRequest
		}
		detail := errorDetail{Code: ierr.Code, Message: ierr.Message}
		if ierr.Action != "" {
			detail.Details = map[string]any{"action": ierr.Action}
		}
		writeJSON(w, status, errorBody{Error: detail})
		return
	}
	if errors.Is(err, memory.ErrImportConflict) {
		writeJSON(w, http.StatusConflict, errorBody{Error: errorDetail{
			Code: "import_conflict", Message: "this audio identity conflicts with stored data",
			Details: map[string]any{"action": "restart with the original audio directory or remove the conflicting profile data"},
		}})
		return
	}
	writeJSON(w, http.StatusInternalServerError, errorBody{Error: errorDetail{
		Code: app.CodeInternal, Message: err.Error(),
	}})
}
