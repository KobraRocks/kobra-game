package port

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- helpers ---------------------------------------------------------------

const denyListPath = "../../port-deny-list.json"

// testGameID is deliberately not com.kobra.stardrifter so probe tests can tell
// "ours" from "another game" without depending on the golden value.
const testGameID = "com.kobra.testgame"

// emptyDenyList returns a deny list that denies nothing.
func emptyDenyList(t *testing.T) *DenyList {
	t.Helper()
	d, err := ParseDenyList([]byte(`{
		"schema": "kobra.port-deny-list/1",
		"ranges": [],
		"ports": [],
		"reserved_ranges": [],
		"defaults": {"base": 8765, "span": 100}
	}`))
	if err != nil {
		t.Fatalf("ParseDenyList(empty): %v", err)
	}
	return d
}

// freePort finds a port in the high user range that can be bound, closes the
// listener, and returns the port. The caller re-confirms it via Probe/Bind.
func freePort(t *testing.T) uint16 {
	t.Helper()
	for p := uint16(29100); p < 29400; p++ {
		l, err := Bind(p)
		if err != nil {
			continue
		}
		_ = l.Close()
		return p
	}
	t.Fatal("no free port found in test range 29100-29399")
	return 0
}

func portOf(t *testing.T, l net.Listener) uint16 {
	t.Helper()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T, want *net.TCPAddr", l.Addr())
	}
	return uint16(addr.Port)
}

func portFromURL(t *testing.T, raw string) uint16 {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	n, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port in %q: %v", raw, err)
	}
	return uint16(n)
}

// --- §7.3 / §26.3 golden hash ----------------------------------------------

func TestFNV1a32Golden(t *testing.T) {
	// Pinned per §26.3: if this constant ever changes, every existing
	// install moves to a new origin, which is exactly what the golden test
	// exists to prevent. Computed once and fixed as a literal.
	const golden = uint32(1579246171) // 0x5e21625b
	if got := FNV1a32("com.kobra.stardrifter"); got != golden {
		t.Fatalf("FNV1a32(%q) = %d (0x%08x), want %d (0x%08x)",
			"com.kobra.stardrifter", got, got, golden, golden)
	}
	// §26.3 also pins the modulo 100 result.
	if got := FNV1a32("com.kobra.stardrifter") % 100; got != 71 {
		t.Fatalf("fnv1a32(com.kobra.stardrifter) %% 100 = %d, want 71", got)
	}

	cases := []struct {
		in   string
		want uint32
	}{
		{"", 2166136261}, // FNV offset basis, by definition
		{"a", 3826002220},
		{"foobar", 3214735720},
	}
	for _, tc := range cases {
		if got := FNV1a32(tc.in); got != tc.want {
			t.Errorf("FNV1a32(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestDefaultBounds(t *testing.T) {
	const base, span = uint16(8765), uint16(100)
	ids := []string{
		"com.kobra.stardrifter",
		"com.kobra.test",
		"",
		"a",
		"com.example.some.very.long.game.identifier-1234",
		"üñïçødé.ゲーム",
	}
	for _, id := range ids {
		got := Default(id, base, span)
		if got < base || got > base+span-1 {
			t.Errorf("Default(%q, %d, %d) = %d, outside [%d, %d]", id, base, span, got, base, base+span-1)
		}
		want := uint16(uint32(base) + FNV1a32(id)%uint32(span))
		if got != want {
			t.Errorf("Default(%q) = %d, want %d", id, got, want)
		}
		// Deterministic: same inputs, same answer.
		if again := Default(id, base, span); again != got {
			t.Errorf("Default(%q) not deterministic: %d then %d", id, got, again)
		}
	}
	// Golden default: the §26.3 game id lands 71 ports above the base.
	if got := Default("com.kobra.stardrifter", base, span); got != 8836 {
		t.Errorf("Default(com.kobra.stardrifter, 8765, 100) = %d, want 8836", got)
	}
	// span 1 (and the degenerate span 0) must not panic or leave the base.
	if got := Default("anything", base, 1); got != base {
		t.Errorf("Default with span 1 = %d, want %d", got, base)
	}
	if got := Default("anything", base, 0); got != base {
		t.Errorf("Default with span 0 = %d, want %d", got, base)
	}
}

// --- §8.4 validation -------------------------------------------------------

func TestValidate(t *testing.T) {
	d, err := LoadDenyList(denyListPath)
	if err != nil {
		t.Fatalf("LoadDenyList: %v", err)
	}
	tests := []struct {
		name       string
		port       uint16
		wantErr    error  // sentinel matched with errors.Is
		wantDeny   bool   // *DenyError expected
		wantReason string // exact reason when wantDeny
	}{
		{name: "privileged", port: 80, wantErr: ErrPrivilegedPort},
		{name: "privileged boundary", port: 1023, wantErr: ErrPrivilegedPort},
		{name: "denied port", port: 4045, wantDeny: true},
		{name: "denied port with message", port: 6000, wantDeny: true, wantReason: "Port 6000 is blocked by browsers. Pick another port."},
		{name: "denied by privileged range but privileged wins", port: 500, wantErr: ErrPrivilegedPort},
		{name: "ephemeral boundary", port: 49152, wantErr: ErrEphemeralPort},
		{name: "ephemeral", port: 50000, wantErr: ErrEphemeralPort},
		{name: "ok low boundary", port: 1024},
		{name: "ok window", port: 8771},
		{name: "ok high boundary", port: 49151},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.port, d)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Validate(%d) = %v, want errors.Is(_, %v)", tc.port, err, tc.wantErr)
				}
				return
			}
			if tc.wantDeny {
				var de *DenyError
				if !errors.As(err, &de) {
					t.Fatalf("Validate(%d) = %v, want *DenyError", tc.port, err)
				}
				if de.Port != tc.port {
					t.Errorf("DenyError.Port = %d, want %d", de.Port, tc.port)
				}
				if de.Reason == "" {
					t.Errorf("DenyError.Reason is empty for port %d", tc.port)
				}
				if tc.wantReason != "" && de.Reason != tc.wantReason {
					t.Errorf("DenyError.Reason = %q, want %q", de.Reason, tc.wantReason)
				}
				// The reason the CLI prints must be the deny list's own.
				if got := d.Reason(tc.port); got != de.Reason {
					t.Errorf("d.Reason(%d) = %q, DenyError.Reason = %q", tc.port, got, de.Reason)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate(%d) = %v, want nil", tc.port, err)
			}
		})
	}
}

func TestValidateNilDenyList(t *testing.T) {
	// A nil deny list denies nothing; only the range checks apply.
	if err := Validate(8771, nil); err != nil {
		t.Errorf("Validate(8771, nil) = %v, want nil", err)
	}
	if err := Validate(80, nil); !errors.Is(err, ErrPrivilegedPort) {
		t.Errorf("Validate(80, nil) = %v, want ErrPrivilegedPort", err)
	}
	if err := Validate(50000, nil); !errors.Is(err, ErrEphemeralPort) {
		t.Errorf("Validate(50000, nil) = %v, want ErrEphemeralPort", err)
	}
}

// --- deny list parsing of the real shipped file ----------------------------

func TestLoadDenyListRealFile(t *testing.T) {
	d, err := LoadDenyList(denyListPath)
	if err != nil {
		t.Fatalf("LoadDenyList(%s): %v", denyListPath, err)
	}
	if d.Len() == 0 {
		t.Fatal("deny list has no individual ports; the shipped file is not being parsed")
	}

	// A known denied port from the shipped file.
	if !d.Contains(4045) {
		t.Error("Contains(4045) = false, want true (4045 is in the shipped file)")
	}
	if got := d.Reason(4045); got == "" {
		t.Error("Reason(4045) is empty, want the generated unsafe-port message")
	}

	// A known good port from the 8765-8864 defaults window.
	if d.Contains(8771) {
		t.Error("Contains(8771) = true, want false (8771 is not denied)")
	}
	if got := d.Reason(8771); got != "" {
		t.Errorf("Reason(8771) = %q, want empty", got)
	}
	if err := Validate(8771, d); err != nil {
		t.Errorf("Validate(8771) = %v, want nil", err)
	}

	// The privileged range and the reserved ephemeral range came through.
	if !d.Contains(500) {
		t.Error("Contains(500) = false, want true (privileged range 1-1023)")
	}
	if d.Contains(50000) {
		t.Error("Contains(50000) = true, want false: reserved_ranges must not join the hard deny set, or Validate would never reach ErrEphemeralPort")
	}
	if !d.Reserved(50000) {
		t.Error("Reserved(50000) = false, want true (reserved_ranges 49152-65535)")
	}

	// The defaults block drives Available's fallback window.
	if base, span := d.DefaultWindow(); base != 8765 || span != 100 {
		t.Errorf("DefaultWindow() = (%d, %d), want (8765, 100)", base, span)
	}
}

func TestDenyReasonsContainNoPath(t *testing.T) {
	d, err := LoadDenyList(denyListPath)
	if err != nil {
		t.Fatalf("LoadDenyList: %v", err)
	}
	// §22.4: a user-facing reason must never leak a filesystem path. Walk
	// every port the list can reject and check its message.
	for p := uint32(1); p <= 65535; p++ {
		port := uint16(p)
		if !d.Contains(port) && !d.Reserved(port) {
			if got := d.Reason(port); got != "" {
				t.Fatalf("Reason(%d) = %q for a port that is neither denied nor reserved", port, got)
			}
			continue
		}
		msg := d.Reason(port)
		if msg == "" {
			t.Fatalf("Reason(%d) is empty for a denied/reserved port", port)
		}
		if strings.ContainsAny(msg, `/\`) || strings.Contains(msg, "launcher/") || strings.Contains(msg, denyListPath) {
			t.Fatalf("Reason(%d) = %q; it appears to contain a filesystem path", port, msg)
		}
	}
}

func TestParseDenyListRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"not json", "{"},
		{"wrong schema", `{"schema":"kobra.port-deny-list/2","ranges":[],"ports":[],"reserved_ranges":[],"defaults":{"base":8765,"span":100}}`},
		{"missing schema", `{"ranges":[],"ports":[],"reserved_ranges":[]}`},
		{"inverted range", `{"schema":"kobra.port-deny-list/1","ranges":[{"from":900,"to":800,"reason":"unsafe"}],"ports":[],"reserved_ranges":[]}`},
		{"zero port", `{"schema":"kobra.port-deny-list/1","ranges":[],"ports":[{"port":0,"reason":"unsafe"}],"reserved_ranges":[]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseDenyList([]byte(tc.data)); err == nil {
				t.Fatalf("ParseDenyList(%s) = nil error, want failure", tc.name)
			}
		})
	}
}

// --- §8.2 probing ----------------------------------------------------------

func TestProbeFreeOnClosedPort(t *testing.T) {
	p := freePort(t)
	res, err := Probe(p, 0, testGameID)
	if err != nil {
		t.Fatalf("Probe(%d) error: %v", p, err)
	}
	if res != ProbeFree {
		t.Fatalf("Probe(%d) = %v, want ProbeFree", p, res)
	}
}

func TestProbeResponses(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		want    ProbeResult
		headers map[string]string
	}{
		{
			name:   "ours",
			status: http.StatusOK,
			body:   `{"app":"kobra-launcher","game_id":"` + testGameID + `"}`,
			want:   ProbeOurs,
		},
		{
			name:   "another kobra game",
			status: http.StatusOK,
			body:   `{"app":"kobra-launcher","game_id":"com.kobra.other"}`,
			want:   ProbeOtherGame,
		},
		{
			name:   "another app claiming the path",
			status: http.StatusOK,
			body:   `{"app":"nginx","game_id":"` + testGameID + `"}`,
			want:   ProbeOtherApp,
		},
		{
			name:   "captive portal html",
			status: http.StatusOK,
			body:   `<html>hi</html>`,
			want:   ProbeOtherApp,
		},
		{
			name:   "server error",
			status: http.StatusInternalServerError,
			body:   `{"app":"kobra-launcher","game_id":"` + testGameID + `"}`,
			want:   ProbeOtherApp,
		},
		{
			name:   "redirect is not followed",
			status: http.StatusFound,
			body:   "",
			want:   ProbeOtherApp,
			headers: map[string]string{
				"Location": "http://127.0.0.1:1/__kobra/probe",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != ProbePath {
					t.Errorf("probe hit %q, want %q", r.URL.Path, ProbePath)
				}
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			p := portFromURL(t, srv.URL)
			res, err := Probe(p, time.Second, testGameID)
			if err != nil {
				t.Fatalf("Probe(%d) error: %v", p, err)
			}
			if res != tc.want {
				t.Fatalf("Probe(%d) = %v, want %v", p, res, tc.want)
			}
		})
	}
}

func TestProbeDoesNotFollowRedirectToOurBody(t *testing.T) {
	// The redirect target would answer "ours"; §8.2/§10.1 say a 3xx is
	// another application, and a redirect must never take the probe to a
	// second host.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"app":"kobra-launcher","game_id":"` + testGameID + `"}`))
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+ProbePath, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	res, err := Probe(portFromURL(t, redirector.URL), time.Second, testGameID)
	if err != nil {
		t.Fatalf("Probe error: %v", err)
	}
	if res != ProbeOtherApp {
		t.Fatalf("Probe on redirecting server = %v, want ProbeOtherApp", res)
	}
}

// --- §8.3 scan and bind ----------------------------------------------------

func TestBindIsExclusive(t *testing.T) {
	l1, err := Bind(0)
	if err != nil {
		t.Fatalf("Bind(0): %v", err)
	}
	defer l1.Close()
	p := portOf(t, l1)

	// A second listener on the same port must fail: SO_REUSEADDR must not
	// become SO_REUSEPORT (§10.1).
	l2, err := Bind(p)
	if err == nil {
		_ = l2.Close()
		t.Fatalf("second Bind(%d) succeeded; the port is being shared", p)
	}
	if !isAddrInUse(err) {
		t.Fatalf("second Bind(%d) = %v, want EADDRINUSE", p, err)
	}
}

func TestBindAndRebindAfterClose(t *testing.T) {
	p := freePort(t)
	l1, err := Bind(p)
	if err != nil {
		t.Fatalf("Bind(%d): %v", p, err)
	}
	_ = l1.Close()
	l2, err := Bind(p)
	if err != nil {
		t.Fatalf("rebind %d after close: %v", p, err)
	}
	_ = l2.Close()
}

func TestScanFindsConfirmedFreePort(t *testing.T) {
	p := freePort(t)
	empty := emptyDenyList(t)

	got, err := Scan(p, 1, empty, nil, 0, testGameID)
	if err != nil {
		t.Fatalf("Scan(%d, 1) = %v", p, err)
	}
	if got != p {
		t.Fatalf("Scan(%d, 1) = %d, want %d", p, got, p)
	}
}

func TestScanSkipsExcludedAndDenied(t *testing.T) {
	p := freePort(t)
	empty := emptyDenyList(t)

	if _, err := Scan(p, 1, empty, map[uint16]bool{p: true}, 0, testGameID); !errors.Is(err, ErrNoPort) {
		t.Fatalf("Scan with excluded port = %v, want ErrNoPort", err)
	}

	// Deny the whole window; Scan must exhaust it without probing anyone.
	d, err := ParseDenyList([]byte(`{
		"schema": "kobra.port-deny-list/1",
		"ranges": [{"from": 20000, "to": 20010, "reason": "unsafe"}],
		"ports": [],
		"reserved_ranges": [],
		"defaults": {"base": 20000, "span": 11}
	}`))
	if err != nil {
		t.Fatalf("ParseDenyList: %v", err)
	}
	if _, err := Scan(20000, 11, d, nil, 50*time.Millisecond, testGameID); !errors.Is(err, ErrNoPort) {
		t.Fatalf("Scan over a fully denied window = %v, want ErrNoPort", err)
	}
}

func TestScanSkipsPrivilegedAndEphemeral(t *testing.T) {
	empty := emptyDenyList(t)
	if _, err := Scan(90, 5, empty, nil, 50*time.Millisecond, testGameID); !errors.Is(err, ErrNoPort) {
		t.Fatalf("Scan in the privileged range = %v, want ErrNoPort", err)
	}
	if _, err := Scan(0, 0, empty, nil, 50*time.Millisecond, testGameID); !errors.Is(err, ErrNoPort) {
		t.Fatalf("Scan with span 0 = %v, want ErrNoPort", err)
	}
}

func TestWideScanSkipsDeniedAndEphemeralWithoutProbing(t *testing.T) {
	// A deny list covering the entire non-privileged, non-ephemeral range
	// means WideScan can only return ErrNoPort, and it does so without
	// touching the network.
	d, err := ParseDenyList([]byte(`{
		"schema": "kobra.port-deny-list/1",
		"ranges": [{"from": 1024, "to": 49151, "reason": "unsafe"}],
		"ports": [],
		"reserved_ranges": [{"from": 49152, "to": 65535, "reason": "ephemeral"}],
		"defaults": {"base": 8765, "span": 100}
	}`))
	if err != nil {
		t.Fatalf("ParseDenyList: %v", err)
	}
	done := make(chan struct{})
	var gotErr error
	go func() {
		defer close(done)
		_, gotErr = WideScan(d, nil, 50*time.Millisecond, testGameID)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WideScan over a fully denied range did not finish promptly; it is probing instead of skipping")
	}
	if !errors.Is(gotErr, ErrNoPort) {
		t.Fatalf("WideScan = %v, want ErrNoPort", gotErr)
	}
}

// --- §8.3 Available --------------------------------------------------------

func TestAvailableTakesAFreeCandidate(t *testing.T) {
	p := freePort(t)
	_, l, err := Available(p, emptyDenyList(t), nil, 100*time.Millisecond, testGameID)
	if err != nil {
		t.Fatalf("Available(%d): %v", p, err)
	}
	defer l.Close()
	if got := portOf(t, l); got != p {
		t.Fatalf("Available bound %d, want %d", got, p)
	}
}

func TestAvailableReallocatesOnceOnEADDRINUSE(t *testing.T) {
	d, err := LoadDenyList(denyListPath)
	if err != nil {
		t.Fatalf("LoadDenyList: %v", err)
	}
	base, span := d.DefaultWindow()

	// Occupy a port inside the defaults window, then ask for it.
	var candidate uint16
	var held net.Listener
	for p := base; p < base+span; p++ {
		l, berr := Bind(p)
		if berr == nil {
			candidate, held = p, l
			break
		}
	}
	if held == nil {
		t.Fatal("could not occupy any port in the deny list's defaults window")
	}
	defer held.Close()

	got, l, err := Available(candidate, d, nil, 100*time.Millisecond, testGameID)
	if err != nil {
		t.Fatalf("Available(%d) = %v, want a reallocated port", candidate, err)
	}
	defer l.Close()
	if got == candidate {
		t.Fatalf("Available returned the occupied candidate %d", candidate)
	}
	if got < base || got >= base+span {
		t.Fatalf("Available returned %d, outside the window [%d, %d)", got, base, base+span)
	}
	if bound := portOf(t, l); bound != got {
		t.Fatalf("Available returned port %d but bound %d", got, bound)
	}
}
