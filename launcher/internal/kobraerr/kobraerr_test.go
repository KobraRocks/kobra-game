package kobraerr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"strings"
	"testing"
)

// everyError enumerates one representative of every constructor in the
// taxonomy. The §26.5 test walks this list; a new constructor must be added
// here, which is the point.
func everyError(t *testing.T) map[string]*KobraError {
	t.Helper()
	return map[string]*KobraError{
		"BadIdentifier":     BadIdentifier("slot", errors.New("cause with /etc/passwd in it")),
		"MalformedBody":     MalformedBody(errors.New("cause with /home/someone/secret")),
		"MalformedField":    MalformedField("merge", "The config write contained no keys.", nil),
		"NoSession":         NoSession(errors.New("token deadbeef")),
		"BadOrigin":         BadOrigin("http://evil.example", nil),
		"BadCSRF":           BadCSRF(errors.New("csrf mismatch")),
		"NotFound":          NotFound("save slot"),
		"Conflict":          Conflict("This slot was changed by another tab.", nil),
		"WriterHeld":        WriterHeld("sid-1234"),
		"TooLarge":          TooLarge(64 << 20),
		"LengthRequired":    LengthRequired(),
		"BadContentType":    BadContentType("text/plain"),
		"MethodNotAllowed":  MethodNotAllowed("GET"),
		"RateLimited":       RateLimited("writes", 12),
		"IO":                IO("The launcher could not complete the request.", nil, errors.New("/var/log/thing")),
		"InsufficientSpace": InsufficientSpace(),
		"OutOfRoot":         OutOfRoot("../../etc/passwd"),
		"ReadOnly":          ReadOnly("permission_denied"),
		"Draining":          Draining(),
	}
}

// TestNoErrorLeaksPathsOrUsername is the §26.5 negative property: no message or
// detail may contain an absolute filesystem path, the OS username, a stack
// trace, or a token.
func TestNoErrorLeaksPathsOrUsername(t *testing.T) {
	username := os.Getenv("USER")
	if username == "" {
		if u, err := user.Current(); err == nil {
			username = u.Username
		}
	}
	for name, ke := range everyError(t) {
		t.Run(name, func(t *testing.T) {
			env := ke.Envelope()
			assertClean(t, name, string(env))
			assertClean(t, name+" message", ke.Msg)
			for k, v := range ke.Detail {
				assertClean(t, fmt.Sprintf("%s detail[%s]", name, k), fmt.Sprintf("%v", v))
			}
			for k, v := range ke.Wire {
				assertClean(t, fmt.Sprintf("%s wire[%s]", name, k), fmt.Sprintf("%v", v))
			}
			if username != "" {
				if strings.Contains(string(env), username) {
					t.Errorf("%s: envelope contains the OS username %q: %s", name, username, env)
				}
			}
			// The cause may contain anything; it must never be serialised.
			if ke.Cause != nil && strings.Contains(string(env), ke.Cause.Error()) {
				t.Errorf("%s: envelope contains the internal cause", name)
			}
			// Every message must be short enough for a dialog.
			if len(ke.Msg) > 300 {
				t.Errorf("%s: message is %d chars, over the schema's 300 cap", name, len(ke.Msg))
			}
		})
	}
}

func assertClean(t *testing.T, what, s string) {
	t.Helper()
	if strings.Contains(s, "/home/") || strings.Contains(s, "/var/") ||
		strings.Contains(s, "/tmp/") || strings.Contains(s, "/etc/") ||
		strings.Contains(s, "C:\\") || strings.Contains(s, "\\\\") {
		t.Errorf("%s leaks a path: %q", what, s)
	}
	if strings.Contains(s, "goroutine ") || strings.Contains(s, ".go:") {
		t.Errorf("%s leaks a stack trace: %q", what, s)
	}
}

// TestEnvelopeUsesTheSchemaErrorEnum checks every code is one of the values
// data-api.schema.json permits.
func TestEnvelopeUsesTheSchemaErrorEnum(t *testing.T) {
	allowed := map[string]bool{
		CodeBadIdentifier: true, CodeMalformedBody: true, CodeNoSession: true,
		CodeBadOrigin: true, CodeBadCSRF: true, CodeNotFound: true,
		CodeConflict: true, CodeTooLarge: true, CodeBadContentType: true,
		CodeRateLimited: true, CodeIO: true, CodeReadOnly: true,
	}
	for name, ke := range everyError(t) {
		if !allowed[ke.Code] {
			t.Errorf("%s: code %q is not in the API error enum", name, ke.Code)
		}
		if ke.HTTP < 400 || ke.HTTP > 599 {
			t.Errorf("%s: HTTP status %d is not an error status", name, ke.HTTP)
		}
	}
}

func TestRateLimitedCarriesRetryAfter(t *testing.T) {
	ke := RateLimited("writes", 30)
	if ke.RetryAfter != 30 {
		t.Errorf("RetryAfter = %d", ke.RetryAfter)
	}
	if !strings.Contains(string(ke.Envelope()), `"retry_after_seconds":30`) {
		t.Errorf("envelope lacks retry_after_seconds: %s", ke.Envelope())
	}
}

func TestFromNormalisesUnknownErrors(t *testing.T) {
	raw := errors.New("open /home/user/secret.json: permission denied")
	ke := From(raw)
	if ke.Code != CodeIO || ke.HTTP != 500 {
		t.Errorf("From(unknown) = %s/%d, want io_error/500", ke.Code, ke.HTTP)
	}
	if strings.Contains(string(ke.Envelope()), "/home/user") {
		t.Errorf("From() leaked the original error text: %s", ke.Envelope())
	}
	if !errors.Is(ke, raw) {
		t.Errorf("From() dropped the cause")
	}
	nested := fmt.Errorf("wrap: %w", BadIdentifier("slot", nil))
	if got := From(nested); got.Code != CodeBadIdentifier {
		t.Errorf("From(wrapped) = %s, want bad_identifier", got.Code)
	}
	if From(nil) != nil {
		t.Errorf("From(nil) should be nil")
	}
}

// TestWireFieldsCannotOverrideReservedKeys guards the envelope shape: a caller
// cannot smuggle a different code or message through Wire.
func TestWireFieldsCannotOverrideReservedKeys(t *testing.T) {
	ke := IO("The launcher could not complete the request.", nil, nil)
	ke.Wire = map[string]any{
		"error":   "not_found",
		"message": "overridden",
		"detail":  map[string]any{"x": 1},
	}
	env := string(ke.Envelope())
	if !strings.Contains(env, `"error":"io_error"`) {
		t.Errorf("Wire overrode the code: %s", env)
	}
	if strings.Contains(env, "overridden") {
		t.Errorf("Wire overrode the message: %s", env)
	}
}

// --- WriteEnvelope ---------------------------------------------------------

// TestWriteEnvelopeSetsRetryAfter is the divergence the single writer exists to
// remove: the data-api copy set Retry-After, the static copy silently did not.
func TestWriteEnvelopeSetsRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteEnvelope(rec, RateLimited("writes", 12))

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if got := rec.Header().Get("Retry-After"); got != "12" {
		t.Errorf("Retry-After = %q, want %q", got, "12")
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if body["error"] != CodeRateLimited {
		t.Errorf("body error = %v, want %q", body["error"], CodeRateLimited)
	}
	if body["retry_after_seconds"] != float64(12) {
		t.Errorf("body retry_after_seconds = %v, want 12", body["retry_after_seconds"])
	}
}

// TestWriteEnvelopePlainIOErrorHasNoRetryAfter covers the other direction: an
// ordinary IO failure must not grow a Retry-After header.
func TestWriteEnvelopePlainIOErrorHasNoRetryAfter(t *testing.T) {
	raw := errors.New("open /home/user/secret.json: permission denied")
	rec := httptest.NewRecorder()
	WriteEnvelope(rec, raw)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q, want empty", got)
	}
	if got, want := rec.Body.String(), string(From(raw).Envelope()); got != want {
		t.Errorf("body = %q, want the envelope %q", got, want)
	}
}

// TestWriteEnvelopeBodyAndStatusMatchTheEnvelope pins the wire shape the three
// old copies produced.
func TestWriteEnvelopeBodyAndStatusMatchTheEnvelope(t *testing.T) {
	ke := NotFound("file")
	rec := httptest.NewRecorder()
	WriteEnvelope(rec, ke)

	if rec.Code != ke.HTTP {
		t.Errorf("status = %d, want %d", rec.Code, ke.HTTP)
	}
	if got, want := rec.Body.String(), string(ke.Envelope()); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestWriteEnvelopeDefaultsMissingStatusTo500 covers a KobraError constructed
// without an HTTP status.
func TestWriteEnvelopeDefaultsMissingStatusTo500(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteEnvelope(rec, &KobraError{Code: CodeIO, Msg: "The launcher could not complete the request."})

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error":"io_error"`) {
		t.Errorf("body = %q, want an io_error envelope", rec.Body.String())
	}
}
