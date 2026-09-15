// Package kobraerr holds the client-facing error taxonomy of the HTTP
// surfaces (Launcher spec §22): the JSON error envelope those surfaces return,
// the constructors that build it, and the one writer, WriteEnvelope, that
// serialises it.
//
// It is deliberately not the error type of the whole launcher. The HTTP
// surfaces — the data API, static serving and the server's middleware — and
// the storage engine use it because they must produce the §22 envelope and obey
// §22.4's no-paths-in-messages rule. The other subsystems (port, sidecar,
// browser, paths) report plain errors and sentinel values and never serialise
// anything to a client.
//
// A KobraError carries both halves of an error: the client-facing envelope
// (Code, HTTP, Message, Detail, RetryAfter) and the internal Cause, which is
// logged but never serialised. Subsystems construct these with the helpers
// below; the HTTP layer renders them with WriteEnvelope.
//
// The §22.4 rule — no message or detail may contain an absolute filesystem
// path, the OS username, a stack trace, or a token — is why the constructors
// take fixed, caller-written messages instead of wrapping a lower-level
// error's Error() string. Internal details belong in Cause only.
package kobraerr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// Error codes. These are the API error enum from data-api.schema.json.
const (
	CodeBadIdentifier  = "bad_identifier"
	CodeMalformedBody  = "malformed_body"
	CodeNoSession      = "no_session"
	CodeBadOrigin      = "bad_origin"
	CodeBadCSRF        = "bad_csrf"
	CodeNotFound       = "not_found"
	CodeConflict       = "conflict"
	CodeTooLarge       = "too_large"
	CodeBadContentType = "bad_content_type"
	CodeRateLimited    = "rate_limited"
	CodeIO             = "io_error"
	CodeReadOnly       = "read_only"
)

// KobraError is the wire-facing error type.
type KobraError struct {
	Code       string         `json:"error"`
	HTTP       int            `json:"-"`
	Msg        string         `json:"message"`
	Detail     map[string]any `json:"detail,omitempty"`
	Cause      error          `json:"-"`
	RetryAfter int            `json:"-"` // seconds; >0 adds Retry-After

	// Wire holds top-level fields that a specific endpoint's schema requires
	// next to error/message rather than inside detail. The §409 conflict
	// response is the one such case: data-api.schema.json's #/$defs/conflict
	// requires current_revision at the top level (and permits modified/slot
	// there too), so a generic envelope with everything under detail does not
	// satisfy it.
	Wire map[string]any `json:"-"`
}

func (e *KobraError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Msg, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Msg)
}

func (e *KobraError) Unwrap() error { return e.Cause }

// Envelope renders the JSON body for the client: code, message, optional
// detail and optional retry_after_seconds. Cause is never included.
func (e *KobraError) Envelope() []byte {
	out := map[string]any{
		"error":   e.Code,
		"message": e.Msg,
	}
	if len(e.Detail) > 0 {
		out["detail"] = e.Detail
	}
	for k, v := range e.Wire {
		// Reserved keys are never overwritten by a caller.
		switch k {
		case "error", "message", "detail", "retry_after_seconds":
			continue
		}
		out[k] = v
	}
	if e.RetryAfter > 0 {
		out["retry_after_seconds"] = e.RetryAfter
	}
	b, err := json.Marshal(out)
	if err != nil {
		b, _ = json.Marshal(map[string]any{
			"error":   CodeIO,
			"message": "The launcher could not describe the problem.",
		})
	}
	return b
}

// From normalises any error into a *KobraError. A nil error returns nil; an
// unrecognised error becomes a generic io_error with no leaked detail.
func From(err error) *KobraError {
	if err == nil {
		return nil
	}
	var ke *KobraError
	if errors.As(err, &ke) {
		return ke
	}
	return &KobraError{
		Code:  CodeIO,
		HTTP:  http.StatusInternalServerError,
		Msg:   "The launcher could not complete the request.",
		Cause: err,
	}
}

// WriteEnvelope renders err as the §22 JSON error envelope and writes it with
// the error's HTTP status. It is the single envelope writer for every HTTP
// surface: the data API, static serving and the server's middleware all call
// it so that the header set and the body shape cannot drift apart again.
//
// The status defaults to 500 when the KobraError does not set one. A RetryAfter
// of n seconds adds a Retry-After header, matching the retry_after_seconds the
// envelope already carries. A nil err is written as a generic 500 rather than
// panicking.
func WriteEnvelope(w http.ResponseWriter, err error) {
	ke := From(err)
	if ke == nil {
		ke = IO("The launcher could not complete the request.", nil, nil)
	}
	if ke.HTTP == 0 {
		ke.HTTP = http.StatusInternalServerError
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if ke.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(ke.RetryAfter))
	}
	w.WriteHeader(ke.HTTP)
	_, _ = w.Write(ke.Envelope())
}

// --- Constructors (§22.2 mapping table) -----------------------------------

// BadIdentifier is 400 bad_identifier: the identifier failed validation
// (§16.1, §16.2). detail["field"] names the offending parameter.
func BadIdentifier(field string, cause error) *KobraError {
	return &KobraError{
		Code:   CodeBadIdentifier,
		HTTP:   http.StatusBadRequest,
		Msg:    "That name is not allowed. Use lowercase letters, digits, dots, dashes and underscores, starting with a letter or digit.",
		Detail: map[string]any{"field": field},
		Cause:  cause,
	}
}

// MalformedBody is 400 malformed_body.
func MalformedBody(cause error) *KobraError {
	return &KobraError{
		Code:  CodeMalformedBody,
		HTTP:  http.StatusBadRequest,
		Msg:   "The request body was not valid JSON for this endpoint.",
		Cause: cause,
	}
}

// MalformedField is 400 malformed_body for a schema-level violation with a
// named field.
func MalformedField(field, msg string, cause error) *KobraError {
	return &KobraError{
		Code:   CodeMalformedBody,
		HTTP:   http.StatusBadRequest,
		Msg:    msg,
		Detail: map[string]any{"field": field},
		Cause:  cause,
	}
}

// NoSession is 401 no_session. Per FR-API-13 the message must not distinguish
// "unknown" from "expired" from "already used".
func NoSession(cause error) *KobraError {
	return &KobraError{
		Code:  CodeNoSession,
		HTTP:  http.StatusUnauthorized,
		Msg:   "This page has no active session. Reload the game from the launcher.",
		Cause: cause,
	}
}

// BadOrigin is 403 bad_origin (logged at warn).
func BadOrigin(origin string, cause error) *KobraError {
	return &KobraError{
		Code:  CodeBadOrigin,
		HTTP:  http.StatusForbidden,
		Msg:   "The request came from a different origin.",
		Cause: cause,
	}
}

// BadCSRF is 403 bad_csrf (logged at warn).
func BadCSRF(cause error) *KobraError {
	return &KobraError{
		Code:  CodeBadCSRF,
		HTTP:  http.StatusForbidden,
		Msg:   "The request was missing a valid CSRF token.",
		Cause: cause,
	}
}

// NotFound is 404 not_found.
func NotFound(what string) *KobraError {
	return &KobraError{
		Code:  CodeNotFound,
		HTTP:  http.StatusNotFound,
		Msg:   "That item does not exist.",
		Cause: fmt.Errorf("%s not found", what),
	}
}

// Conflict is 409 conflict. For a stale if_revision the extra fields are
// current_revision/modified/slot (data-api.schema.json #/$defs/conflict).
func Conflict(msg string, detail map[string]any) *KobraError {
	return &KobraError{
		Code:   CodeConflict,
		HTTP:   http.StatusConflict,
		Msg:    msg,
		Detail: detail,
	}
}

// WriterHeld is the cross-tab writer-election conflict (§11.5).
func WriterHeld(currentWriter string) *KobraError {
	return Conflict("Another tab is currently saving.", map[string]any{
		"current_writer": currentWriter,
	})
}

// TooLarge is 413 too_large.
func TooLarge(limit int64) *KobraError {
	return &KobraError{
		Code:   CodeTooLarge,
		HTTP:   http.StatusRequestEntityTooLarge,
		Msg:    "That request was larger than the launcher accepts.",
		Detail: map[string]any{"max_request_bytes": limit},
	}
}

// LengthRequired is 411: a write without Content-Length (§12.5).
func LengthRequired() *KobraError {
	return &KobraError{
		Code: CodeTooLarge,
		HTTP: http.StatusLengthRequired,
		Msg:  "The request must declare its Content-Length.",
	}
}

// BadContentType is 415 bad_content_type.
func BadContentType(got string) *KobraError {
	return &KobraError{
		Code:   CodeBadContentType,
		HTTP:   http.StatusUnsupportedMediaType,
		Msg:    "That content type is not accepted here.",
		Detail: map[string]any{"expected": "application/json"},
		Cause:  fmt.Errorf("got content type %q", got),
	}
}

// MethodNotAllowed is 405 with the Allow header.
func MethodNotAllowed(allow string) *KobraError {
	return &KobraError{
		Code:   CodeBadIdentifier,
		HTTP:   http.StatusMethodNotAllowed,
		Msg:    "That method is not allowed for this endpoint.",
		Detail: map[string]any{"allow": allow},
	}
}

// RateLimited is 429 rate_limited with a Retry-After (§17.3).
func RateLimited(kind string, retryAfter int) *KobraError {
	return &KobraError{
		Code:       CodeRateLimited,
		HTTP:       http.StatusTooManyRequests,
		Msg:        "Too many requests in a short time. The launcher is throttling this tab.",
		Detail:     map[string]any{"kind": kind},
		RetryAfter: retryAfter,
	}
}

// IO is 500 io_error. detail carries a short machine-readable reason.
func IO(msg string, detail map[string]any, cause error) *KobraError {
	return &KobraError{
		Code:   CodeIO,
		HTTP:   http.StatusInternalServerError,
		Msg:    msg,
		Detail: detail,
		Cause:  cause,
	}
}

// InsufficientSpace is the pre-flight disk-space rejection (§17.5).
func InsufficientSpace() *KobraError {
	return IO("The disk is full. Free some space and try again.",
		map[string]any{"reason": "insufficient_space"}, nil)
}

// OutOfRoot is the defence-in-depth confinement failure (§16.4). It is a 500
// because it means the server built a path it should not have.
func OutOfRoot(id string) *KobraError {
	return IO("The launcher refused to read or write that location.",
		map[string]any{"reason": "out_of_root"}, fmt.Errorf("path escaped data root for %q", id))
}

// ReadOnly is 503 read_only: the data directory failed the write probe.
func ReadOnly(reason string) *KobraError {
	return &KobraError{
		Code:   CodeReadOnly,
		HTTP:   http.StatusServiceUnavailable,
		Msg:    "The game cannot save because its data folder is read-only.",
		Detail: map[string]any{"reason": reason},
	}
}

// Draining is the 503 returned once shutdown has begun (§10.6).
func Draining() *KobraError {
	return &KobraError{
		Code: CodeReadOnly,
		HTTP: http.StatusServiceUnavailable,
		Msg:  "The launcher is shutting down.",
	}
}
