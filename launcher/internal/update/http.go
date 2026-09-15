package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"kobragames.local/launcher/internal/kobraerr"
)

// Network limits. These are deliberately small: the update check fetches two
// short JSON documents, and the launcher's outbound client exists only for the
// user-initiated patch path (§3.3, FR-UPD-2).
const (
	// latestTimeout bounds GET <base>/latest.json (§19.2 step 2).
	latestTimeout = 10 * time.Second
	// manifestTimeout bounds GET <manifest_url>.
	manifestTimeout = 20 * time.Second
	// maxRedirects is the redirect cap. Beyond it the fetch fails rather than
	// following an unbounded chain.
	maxRedirects = 3
	// maxDocumentBytes is the response-body ceiling for a JSON document. A
	// release manifest with a full file index is well under this.
	maxDocumentBytes = 8 << 20
)

// httpClient is the package's only HTTP client. Timeouts are explicit and the
// redirect policy is bounded; there is no cookie jar and no proxy
// configuration beyond net/http's environment defaults.
var httpClient = &http.Client{
	Timeout: manifestTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("update: stopped after %d redirects", maxRedirects)
		}
		return nil
	},
}

// fetchDocument performs a bounded GET and returns the response body. It
// enforces the scheme, the status code, and the body ceiling. Errors are
// kobraerr values with fixed, path-free messages; the URL and underlying error
// travel in Cause.
func fetchDocument(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, kobraerr.IO("The update address could not be understood.",
			map[string]any{"reason": "bad_url"}, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, kobraerr.IO("The update address could not be understood.",
			map[string]any{"reason": "bad_scheme"}, fmt.Errorf("update: unsupported scheme %q", u.Scheme))
	}
	if u.Host == "" {
		return nil, kobraerr.IO("The update address could not be understood.",
			map[string]any{"reason": "bad_url"}, errors.New("update: empty host"))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, kobraerr.IO("The update could not be checked.",
			map[string]any{"reason": "request"}, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, kobraerr.IO("The update could not be checked. The launcher may be offline.",
			map[string]any{"reason": "network"}, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, kobraerr.IO("The update could not be checked.",
			map[string]any{"reason": "status", "status": resp.StatusCode},
			fmt.Errorf("update: unexpected HTTP status %d", resp.StatusCode))
	}

	// io.LimitReader bounds the body; reading one byte past the limit detects
	// an oversized document instead of silently truncating it.
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, kobraerr.IO("The update could not be checked.",
			map[string]any{"reason": "read"}, err)
	}
	if int64(len(body)) > limit {
		return nil, kobraerr.IO("The update could not be checked.",
			map[string]any{"reason": "too_large"}, errors.New("update: response body exceeded the document limit"))
	}
	return body, nil
}

// resolveURL resolves a possibly-relative reference (latest.json may publish a
// relative manifest_url) against the base it was fetched from.
func resolveURL(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}
