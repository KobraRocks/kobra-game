package port

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"
)

// ProbePath is the single well-known unauthenticated path the probe uses
// (§8.2, FR-SRV-9). It is deliberately the same endpoint the shell and the
// diagnostic tooling use.
const ProbePath = "/__kobra/probe"

// DefaultProbeTimeout is the §8.2 probe timeout, applied whenever a caller
// passes a non-positive timeout.
const DefaultProbeTimeout = 500 * time.Millisecond

// probeAppName is the only app value the probe trusts. A body that says
// anything else — including a different launcher — is "another application".
const probeAppName = "kobra-launcher"

// maxProbeBody bounds how much of the probe response is read, so a hostile or
// broken local server cannot make the launcher allocate without limit.
const maxProbeBody = 4096

// ProbeResult is the interpretation of one probe (§8.2).
type ProbeResult int

const (
	// ProbeFree: the connection was refused. The port is free.
	ProbeFree ProbeResult = iota
	// ProbeOurs: a Kobra launcher answered for our own game_id.
	ProbeOurs
	// ProbeOtherGame: a Kobra launcher answered for a different game_id.
	ProbeOtherGame
	// ProbeOtherApp: the port is held by something that is not us.
	ProbeOtherApp
)

// String returns the short token used by the port.probe log event. Free and
// ours match the Appendix C.2 vocabulary; the two "other" cases are kept
// distinct because they mean different things to the user (a second Kobra game
// vs. an unrelated program).
func (r ProbeResult) String() string {
	switch r {
	case ProbeFree:
		return "free"
	case ProbeOurs:
		return "ours"
	case ProbeOtherGame:
		return "other_game"
	case ProbeOtherApp:
		return "other_app"
	default:
		return "other_app"
	}
}

// Usable reports whether a probe result means the port may be taken: free, or
// already ours (the second-instance case, §8.2).
func (r ProbeResult) Usable() bool { return r == ProbeFree || r == ProbeOurs }

// newProbeClient builds the probe HTTP client. §8.2 and the loopback-only rule
// require:
//
//   - an explicit Transport with Proxy: nil, so an ambient HTTP_PROXY can
//     never intercept a 127.0.0.1 probe;
//   - DisableKeepAlives, because each probe is independent and a pooled
//     connection to a port that later changes hands would be a lie;
//   - no redirect following: a 3xx is "another application", never a reason to
//     talk to a second host.
func newProbeClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:             nil,
			DisableKeepAlives: true,
			DialContext: (&net.Dialer{
				Timeout:   timeout,
				KeepAlive: -1,
			}).DialContext,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// Returning ErrUseLastResponse makes Do return the 3xx
			// response itself instead of following it.
			return http.ErrUseLastResponse
		},
	}
}

// Probe asks the port who it is: GET http://127.0.0.1:<p>/__kobra/probe and
// interpret the answer per §8.2.
//
//	connection refused                        -> ProbeFree
//	200 {"app":"kobra-launcher","game_id":ours} -> ProbeOurs
//	200 {"app":"kobra-launcher","game_id":other}-> ProbeOtherGame
//	anything else (3xx, other status, other body)-> ProbeOtherApp
//
// A non-positive timeout means DefaultProbeTimeout. The returned error is
// non-nil only for a transport failure that is not a refusal (timeout, reset,
// malformed request); the result is then ProbeOtherApp and callers must treat
// the port as unusable. Probing is advisory — Bind is authoritative (§8.3).
func Probe(p uint16, timeout time.Duration, oursGameID string) (ProbeResult, error) {
	client := newProbeClient(timeout)
	url := fmt.Sprintf("http://127.0.0.1:%d%s", p, ProbePath)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return ProbeOtherApp, err
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return ProbeFree, nil
		}
		return ProbeOtherApp, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxProbeBody))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return ProbeOtherApp, nil
	}
	var body struct {
		App    string `json:"app"`
		GameID string `json:"game_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxProbeBody)).Decode(&body); err != nil {
		return ProbeOtherApp, nil
	}
	// Only a body that names this exact application is trusted (§8.2).
	if body.App != probeAppName {
		return ProbeOtherApp, nil
	}
	if body.GameID == oursGameID {
		return ProbeOurs, nil
	}
	return ProbeOtherGame, nil
}
