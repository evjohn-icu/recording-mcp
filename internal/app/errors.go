// Package app is the single business entry point shared by the MCP server and
// the local web server. It composes the memory store, hybrid retrieval and the
// semantic backend so both transports return the same structures and the same
// decisions; neither transport reimplements retrieval or editing.
package app

import (
	"errors"

	"recordingmcp/internal/memory"
)

// Error codes shared by the HTTP layer, its response bodies and its tests.
const (
	CodeAnnotationVersionConflict = "annotation_version_conflict"
	CodeNoteNotFound              = "note_not_found"
	CodeNotFound                  = "not_found"
	CodeValidationFailed          = "validation_failed"
	CodeAudioOffsetUnknown        = "audio_offset_unknown"
	CodeAudioOffsetOutOfRange     = "audio_offset_out_of_range"
	CodeAudioDeleted              = "audio_deleted"
	CodeAudioMissing              = "audio_missing"
	CodeAudioUnavailable          = "audio_unavailable"
	CodeUnauthenticated           = "unauthenticated"
	CodeOriginRejected            = "origin_rejected"
	CodeCSRFRejected              = "csrf_rejected"
	CodeInternal                  = "internal"
)

// Error is a transport-neutral failure with the HTTP status the web layer must
// use. MCP treats every Error as a plain tool error: only its message escapes.
type Error struct {
	Code    string
	Message string
	Status  int
	// CurrentVersion and Current describe the stored annotation view when Code
	// is CodeAnnotationVersionConflict.
	CurrentVersion string
	Current        memory.Annotations
	// CurrentNote is the conflict counterpart of the note the caller tried to
	// edit, resolved through the current logical view. It is nil when the
	// current version no longer contains that note.
	CurrentNote *NoteView
}

func (e *Error) Error() string { return e.Message }

// AsError reports whether err is (or wraps) an app error.
func AsError(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

func validationError(msg string) *Error {
	return &Error{Code: CodeValidationFailed, Status: 400, Message: msg}
}

func notFoundError(msg string) *Error {
	return &Error{Code: CodeNotFound, Status: 404, Message: msg}
}

func audioError(code, msg string, status int) *Error {
	return &Error{Code: code, Status: status, Message: msg}
}

func internalError(err error) *Error {
	return &Error{Code: CodeInternal, Status: 500, Message: err.Error()}
}
