package sidecar

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// HealthResponse is the body served by GET /__kobra/health (§9.3 rule 3).
type HealthResponse struct {
	App      string `json:"app"`
	GameID   string `json:"game_id"`
	Instance string `json:"instance"`
	Release  string `json:"release,omitempty"`
	Port     uint16 `json:"port,omitempty"`
}

// probeClient is a loopback-only HTTP client. Proxy is explicitly nil so that
// an ambient proxy setting can never intercept a 127.0.0.1 request, and
// redirects are not followed because a redirect is not an answer from us.
func probeClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy:             nil,
			DisableKeepAlives: true,
			DialContext: (&net.Dialer{
				Timeout:   timeout,
				KeepAlive: -1,
			}).DialContext,
		},
	}
}

// fetchHealth performs the §9.3 rule-3 health request.
func fetchHealth(port uint16, timeout time.Duration) (*HealthResponse, error) {
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/__kobra/health", port)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// The Host header must match exactly what the server validates (§12.1).
	req.Host = fmt.Sprintf("127.0.0.1:%d", port)
	resp, err := probeClient(timeout).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("health returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return nil, err
	}
	var h HealthResponse
	if err := json.Unmarshal(body, &h); err != nil {
		return nil, err
	}
	return &h, nil
}
