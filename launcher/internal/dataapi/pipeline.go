package dataapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"kobragames.local/launcher/internal/kobraerr"
	"kobragames.local/launcher/internal/paths"
	"kobragames.local/launcher/internal/session"
	"kobragames.local/launcher/internal/storage"
)

// contentJSON is the only media type accepted for write bodies. Binary mod
// archives arrive base64-encoded inside the JSON document, so the media type is
// always JSON on the wire.
const contentJSON = "application/json"

// authMethod records how a request proved its session, because the CSRF rule
// depends on it (FR-SRV-6a).
type authMethod int

const (
	// authCookie is a session carried by the HttpOnly session cookie, which the
	// browser attaches to every same-origin request without the page asking.
	// This is the only case the double-submit check defends.
	authCookie authMethod = iota
	// authBearer is a session id attached explicitly in an Authorization
	// header. Nothing ambient is involved, so CSRF does not apply.
	authBearer
)

// validate implements steps 1-6 of §12.6 in the fixed, security-relevant order:
// Host, Origin, Method, Content-Type, Content-Length, Session, CSRF.
//
// body is true for requests that carry a JSON body, so the Content-Type and
// Content-Length checks only run where they apply.
func (s *service) validate(w http.ResponseWriter, r *http.Request, methods string, body bool) (*session.Session, bool) {
	if !s.checkHost(r) {
		s.log().Warn("host.reject", map[string]any{"host": r.Host})
		// 421 Misdirected Request, closed without a body (§12.1).
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusMisdirectedRequest)
		return nil, false
	}
	if !s.checkOrigin(r) {
		s.log().Warn("origin.reject", map[string]any{"origin": r.Header.Get("Origin")})
		kobraerr.WriteEnvelope(w, kobraerr.BadOrigin(r.Header.Get("Origin"), nil))
		return nil, false
	}
	if !methodAllowed(r.Method, methods) {
		w.Header().Set("Allow", strings.Join(strings.Fields(methods), ", "))
		kobraerr.WriteEnvelope(w, kobraerr.MethodNotAllowed(methods))
		return nil, false
	}
	if body {
		if !contentTypeOK(r.Header.Get("Content-Type")) {
			kobraerr.WriteEnvelope(w, kobraerr.BadContentType(r.Header.Get("Content-Type")))
			return nil, false
		}
		if r.ContentLength < 0 {
			kobraerr.WriteEnvelope(w, kobraerr.LengthRequired())
			return nil, false
		}
		if r.ContentLength > s.Config().MaxRequestBytes {
			kobraerr.WriteEnvelope(w, kobraerr.TooLarge(s.Config().MaxRequestBytes))
			return nil, false
		}
	}
	sess, auth, ok := s.checkSession(w, r)
	if !ok {
		return nil, false
	}
	// FR-SRV-6a's double-submit check exists to defend against a request the
	// browser authenticated with an *ambient* credential — the session cookie,
	// which it attaches automatically to any request to this origin. A bearer
	// session id is attached explicitly by the caller, is never sent
	// automatically, and cannot be read by a cross-origin page (the session
	// exchange response is not CORS-readable), so no CSRF vector exists and the
	// check is skipped. This is what makes the bearer path usable for writes,
	// which the previous unconditional check made impossible.
	if isWriteMethod(r.Method) && auth == authCookie {
		if err := s.checkCSRF(r, sess); err != nil {
			s.log().Warn("csrf.reject", map[string]any{"origin": r.Header.Get("Origin")})
			kobraerr.WriteEnvelope(w, err)
			return nil, false
		}
	}
	return sess, true
}

// checkHost implements §12.1. The comparison is exact: localhost, [::1] and any
// hostname are rejected, which is what defeats DNS rebinding.
func (s *service) checkHost(r *http.Request) bool {
	return r.Host == s.bindAuthority()
}

// checkOrigin implements §12.2. An absent Origin is accepted (same-origin GET,
// HEAD, or a non-browser client); a present one must match exactly.
func (s *service) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	return origin == "http://"+s.bindAuthority()
}

func methodAllowed(method, allowed string) bool {
	for _, m := range strings.Fields(allowed) {
		if m == method {
			return true
		}
	}
	return false
}

func isWriteMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// contentTypeOK parses the media type rather than comparing the raw header, so
// "application/json; charset=utf-8" is accepted and "application/json-something"
// is not (§12.4).
func contentTypeOK(header string) bool {
	if header == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(header)
	if err != nil {
		return false
	}
	return mt == contentJSON
}

// checkSession implements the session gate (§11.2, §11.3). It reports how the
// session was proven, because the CSRF rule applies only to a cookie-carried
// session (FR-SRV-6a).
func (s *service) checkSession(w http.ResponseWriter, r *http.Request) (*session.Session, authMethod, bool) {
	store := s.host.Sessions()
	if c, err := r.Cookie(session.CookieSession); err == nil {
		if sess, ok := store.Get(c.Value); ok {
			return sess, authCookie, true
		}
	}
	// A bearer session id is also accepted: this is what --print-url consumers
	// and the E2E harness use, and it avoids putting a cookie jar in a script.
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		if sess, ok := store.Get(strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))); ok {
			return sess, authBearer, true
		}
	}
	_ = w
	kobraerr.WriteEnvelope(w, kobraerr.NoSession(nil))
	return nil, authCookie, false
}

// checkCSRF implements the §11.4 double-submit check. The constants live in the
// session package so that the cookie and header names have one owner.
//
// Both carriers are required and both are compared with the session's own
// token, so a request that satisfies Host, Origin and the session cookie but
// lacks the header is rejected — which is exactly why FR-SRV-19 cannot use
// navigator.sendBeacon() for its goodbye: sendBeacon cannot set a request
// header. A shell must use fetch(..., {keepalive: true}) instead.
//
// This is only reached for cookie-authenticated requests; see validate.
func (s *service) checkCSRF(r *http.Request, sess *session.Session) error {
	var cookieVal string
	if c, err := r.Cookie(session.CookieCSRF); err == nil {
		cookieVal = c.Value
	}
	header := r.Header.Get(session.HeaderCSRF)
	return s.host.Sessions().CheckCSRF(cookieVal, header, sess, r.Header.Get("Origin"))
}

// decodeJSON reads a bounded body and unmarshals it (§12.5, §17.4).
//
// A Content-Length that lies is caught by the LimitReader: the read is capped at
// max+1 and anything that reaches the cap is rejected with 413.
func decodeJSON(r *http.Request, v any, max int64) error {
	if max <= 0 {
		max = 64 << 20
	}
	body := io.LimitReader(r.Body, max+1)
	raw, err := io.ReadAll(body)
	if err != nil {
		return kobraerr.MalformedBody(err)
	}
	if int64(len(raw)) > max {
		return kobraerr.TooLarge(max)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return kobraerr.MalformedBody(errors.New("empty body"))
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return kobraerr.MalformedBody(err)
	}
	if dec.More() {
		return kobraerr.MalformedBody(errors.New("trailing content"))
	}
	return nil
}

// routePath extracts a single path parameter below prefix and validates it as an
// identifier before it reaches any handler (§12.6 step 8, §16.1).
func routePath(path, prefix string) (string, bool) {
	rest := strings.TrimPrefix(path, prefix)
	rest = strings.Trim(rest, "/")
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

// validateIdentifierField validates one identifier and maps a failure to the
// named-field error envelope.
func validateIdentifierField(name, value string) error {
	if err := storage.ValidateIdentifier(value); err != nil {
		// Guarantee the error names the field even if a future change returns
		// a different shape.
		if ke := kobraerr.From(err); ke.Code == kobraerr.CodeBadIdentifier {
			ke.Detail = map[string]any{"field": name}
			return ke
		}
		return kobraerr.BadIdentifier(name, err)
	}
	return nil
}

// confineStatic is used by handlers that must resolve a client-supplied name
// that is not a bare identifier.
func confineStatic(root, full string) error {
	if err := paths.Confine(root, full); err != nil {
		return kobraerr.NotFound("path")
	}
	return nil
}
