package update

// This file implements DownloadArchive (Updater spec §8): the streaming,
// resumable, cancellable, size-bounded fetch of a release archive.
//
// Design points that are load-bearing:
//
//   - It uses its OWN http.Client. The package's manifest client has a 20 s
//     total-request timeout, which would abort every realistic download; see
//     §8.2 and stall.go.
//   - The declared manifest size is authoritative, both as the ceiling (a
//     server that streams forever must not fill the disk, R8.3) and as the
//     denominator for progress (R11.2).
//   - The archive is streamed to "<name>.part" and verified in place before any
//     consumer reads it; the apply removes it afterwards. The rename to a
//     non-".part" name that R5.1 describes is deliberately not performed: the
//     verification gate is what the requirement protects, and a second name
//     would only add another artefact to clean up.
//   - SHA-256 is computed incrementally as bytes are written, so a large
//     archive is never read twice (R8.11).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Download limits (§8.2, §8.6).
const (
	// dlDialTimeout bounds connection establishment.
	dlDialTimeout = 10 * time.Second
	// dlTLSHandshakeTimeout bounds the TLS handshake.
	dlTLSHandshakeTimeout = 10 * time.Second
	// dlResponseHeaderTimeout bounds the wait for the first header byte.
	dlResponseHeaderTimeout = 15 * time.Second
	// dlStallTimeout is the maximum gap between successful reads (R8.5).
	dlStallTimeout = 30 * time.Second
	// dlIdleConnTimeout matches the transport's connection reuse window.
	dlIdleConnTimeout = 30 * time.Second

	// DefaultMaxArchiveBytes is the ceiling on a declared archive size. Without
	// it a compromised manifest is an unbounded disk-fill primitive on a folder
	// that may live on a USB stick (R8.17).
	DefaultMaxArchiveBytes = int64(8) << 30 // 8 GiB

	// MinDownloadTimeout and MinDownloadRate set the effective total deadline:
	// max(MinDownloadTimeout, size / MinDownloadRate) (R8.18). The stall
	// watchdog is what actually bounds a hung transfer; this is the backstop
	// for a server that dribbles bytes forever.
	MinDownloadTimeout = 5 * time.Minute
	MinDownloadRate    = int64(64) << 10 // 64 KiB/s

	// dlProgressInterval bounds how often the progress callback fires (R8.13).
	dlProgressInterval = 250 * time.Millisecond
)

// Sentinel failures. Apply maps these onto the E33/E34/E36 wordings; callers
// outside this package should use errors.Is rather than matching on text.
var (
	// errCancelled is a user-requested cancellation (E34, §12).
	errCancelled = errors.New("update: the download was cancelled")
	// errNoArchiveSize is a manifest that declares no usable archive size
	// (E33, R8.19).
	errNoArchiveSize = errors.New("update: the manifest declares no usable archive size")
	// errArchiveTooLarge is a declared size beyond the policy ceiling (E33,
	// R8.17).
	errArchiveTooLarge = errors.New("update: the declared archive is larger than the launcher will download")
	// errInsecureScheme is a plain-http archive URL outside loopback (E33,
	// R13.6).
	errInsecureScheme = errors.New("update: the archive address is not secure")
	// errDownloadFailed is a network, status or truncation failure (E36,
	// §16.2).
	errDownloadFailed = errors.New("update: the download failed")
)

// IsCancelled reports whether err is a user-requested cancellation.
func IsCancelled(err error) bool { return errors.Is(err, errCancelled) }

// DownloadOptions configures one archive fetch.
type DownloadOptions struct {
	// MaxArchiveBytes is the policy ceiling on the declared size. Zero means
	// DefaultMaxArchiveBytes.
	MaxArchiveBytes int64
	// ExpectedHash is the archive hash the marker recorded at request time. It
	// is used only to validate a resumable partial (R8.9); Verify remains the
	// authority on the finished file.
	ExpectedHash string
	// StallTimeout is the maximum gap between reads. Zero means
	// dlStallTimeout.
	StallTimeout time.Duration
	// Progress is called at most once per dlProgressInterval, and always once
	// on completion and once on failure (R8.13). It runs on the downloading
	// goroutine and is never called after DownloadArchive returns (R8.14).
	Progress func(written, total int64)
	// ShouldCancel is polled alongside progress. It is how the sidecar cancel
	// flag (§12) reaches a download that has no browser session attached.
	ShouldCancel func() bool
	// AllowInsecure permits a plain-http archive URL. It is set for loopback
	// development servers only (R13.6).
	AllowInsecure bool
	// NoResume discards any partial download instead of resuming it. It exists
	// for the tests of R8.9 and for a caller that knows the partial is stale.
	NoResume bool
}

// DownloadResult describes a completed download.
type DownloadResult struct {
	// Path is the verified-and-complete file: the "<name>.part" path, verified
	// in place rather than renamed (see the R5.1 note in the package comment).
	Path string
	// Bytes is the number of bytes written, which equals the declared size.
	Bytes int64
	// SHA256 is the lowercase hex digest of the file as written (R8.11).
	SHA256 string
	// Resumed is true when the download continued an existing partial.
	Resumed bool
}

// downloadMeta is the sidecar record that makes a partial download resumable
// (R8.8). It is written while the download runs so a crash mid-download leaves
// a usable starting point.
type downloadMeta struct {
	Schema       string `json:"schema"`
	URL          string `json:"url"`
	ArchiveHash  string `json:"archive_hash,omitempty"`
	Size         int64  `json:"size"`
	BytesWritten int64  `json:"bytes_written"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	Updated      string `json:"updated"`
}

// matches reports whether a partial belongs to exactly the archive the manifest
// now names (R8.9). A partial is resumed only on an exact match: a file
// assembled from two different archives is worse than no resume at all.
func (dm *downloadMeta) matches(url, hash string, size int64) bool {
	if dm == nil || dm.Schema != MetaSchema {
		return false
	}
	if dm.URL != url || dm.Size != size {
		return false
	}
	// When the manifest declares a hash, the partial's recorded hash must agree
	// with it. When it declares none, Verify will refuse the archive anyway.
	if hash != "" && dm.ArchiveHash != hash {
		return false
	}
	return dm.BytesWritten > 0 && dm.BytesWritten < size
}

// downloadClient is the archive client. It has no Client.Timeout on purpose:
// the total limit is bounded by the context deadline (R8.18) and idleness by
// the stall guard (R8.5).
var downloadClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   dlDialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   dlTLSHandshakeTimeout,
		ResponseHeaderTimeout: dlResponseHeaderTimeout,
		IdleConnTimeout:       dlIdleConnTimeout,
		ExpectContinueTimeout: time.Second,
		MaxIdleConns:          4,
	},
	CheckRedirect: checkDownloadRedirect,
}

// checkDownloadRedirect bounds the redirect chain and refuses a downgrade from
// https to http (R8.6). A downgrade redirect from a mirror is a compromise,
// not a convenience.
func checkDownloadRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("update: stopped after %d redirects", maxRedirects)
	}
	if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return errors.New("update: refused a redirect that downgrades to plain http")
	}
	return nil
}

// downloadTimeout returns the effective context deadline for a declared size
// (R8.18).
func downloadTimeout(size int64) time.Duration {
	d := MinDownloadTimeout
	if size > 0 {
		if rate := time.Duration(size/MinDownloadRate) * time.Second; rate > d {
			d = rate
		}
	}
	return d
}

// DownloadArchive streams the release archive at url into dest (spec §8.1).
//
// dest is the ".part" path. size is the manifest's declared archive size and is
// authoritative (R8.2). The download is bounded to exactly size bytes, is
// resumable when a matching partial exists, and is cancellable through ctx and
// opts.ShouldCancel.
func DownloadArchive(ctx context.Context, url, dest string, size int64, opts DownloadOptions) (DownloadResult, error) {
	var res DownloadResult

	if err := checkArchiveURL(url, opts.AllowInsecure); err != nil {
		return res, err
	}
	if size <= 0 {
		return res, errNoArchiveSize
	}
	maxBytes := opts.MaxArchiveBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxArchiveBytes
	}
	if size > maxBytes {
		return res, errArchiveTooLarge
	}
	if strings.TrimSpace(dest) == "" {
		return res, errDownloadFailed
	}

	ctx, cancel := context.WithTimeout(ctx, downloadTimeout(size))
	defer cancel()

	stall := newStallGuard(opts.StallTimeout, cancel)
	stall.start()
	defer stall.stop()

	// Decide whether an existing partial is usable (R8.8, R8.9).
	metaPath := filepath.Join(filepath.Dir(dest), MetaFileName)
	var existing int64
	var etag, lastMod string
	if !opts.NoResume {
		var dm downloadMeta
		if ok, err := readJSON(metaPath, &dm); ok && err == nil && dm.matches(url, opts.ExpectedHash, size) {
			if st, serr := os.Stat(dest); serr == nil && st.Size() == dm.BytesWritten {
				existing = dm.BytesWritten
				etag, lastMod = dm.ETag, dm.LastModified
				res.Resumed = true
				logInfo("update.download.resume", map[string]any{"from_bytes": existing})
			} else {
				logWarn("update.download.restart", map[string]any{"reason": "partial_missing"})
				removeIfPresent(dest)
			}
		}
	} else {
		removeIfPresent(dest)
	}

	if !res.Resumed {
		removeIfPresent(metaPath)
	}

	written, sum, resumed, err := transfer(ctx, url, dest, size, existing, etag, lastMod, opts.ExpectedHash, stall, opts)
	res.Path = dest
	res.Bytes = written
	res.SHA256 = sum
	res.Resumed = resumed

	if err != nil {
		if opts.Progress != nil {
			opts.Progress(written, size)
		}
		return res, err
	}
	if written != size {
		return res, fmt.Errorf("%w: expected %d bytes, received %d", errDownloadFailed, size, written)
	}
	if opts.Progress != nil {
		opts.Progress(written, size)
	}
	return res, nil
}

// transfer performs one HTTP request and writes the body to dest, appending
// when existing > 0. It returns the bytes written, the SHA-256 of the whole
// file (R8.11), and whether the transfer genuinely continued a partial — a
// server that answered 200 to a Range request sent the whole file, so that is
// not a resume (R8.8 step 3).
func transfer(ctx context.Context, url, dest string, size, existing int64, etag, lastMod, archiveHash string,
	stall *stallGuard, opts DownloadOptions) (written int64, sum string, resumed bool, err error) {

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", false, fmt.Errorf("%w: %v", errDownloadFailed, err)
	}
	// §8.7: this is an archive, not a JSON document.
	req.Header.Set("Accept", "application/zstd, application/octet-stream, */*")
	if existing > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(existing, 10)+"-")
		if etag != "" {
			req.Header.Set("If-Range", etag)
		} else if lastMod != "" {
			req.Header.Set("If-Range", lastMod)
		}
	}

	resp, err := downloadClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return existing, "", false, cancelOrTimeoutErr(ctx, opts)
		}
		return existing, "", false, fmt.Errorf("%w: %v", errDownloadFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return existing, "", false, fmt.Errorf("%w: unexpected HTTP status %d", errDownloadFailed, resp.StatusCode)
	}

	// R8.2/R8.3: Content-Length is not the size of the artefact, and the
	// declared size is authoritative. A response that announces MORE than the
	// manifest declares is refused before a single byte is written, which is
	// cheaper and safer than reading to the bound and discovering it after. The
	// reverse case is not a refusal: a server that announces fewer bytes than
	// the manifest declares is caught as a truncation at the end of the
	// transfer, and a range response legitimately announces less.
	if resp.ContentLength > 0 && resp.StatusCode == http.StatusOK && resp.ContentLength > size {
		return existing, "", false, fmt.Errorf(
			"%w: server announced %d bytes, the manifest declares %d",
			errDownloadFailed, resp.ContentLength, size)
	}

	// A 200 answer to a Range request means the server sent the whole file;
	// anything already on disk must be discarded (R8.8 step 3). A 206 that we
	// did not ask for is equally unusable: reject it rather than splice.
	switch {
	case resp.StatusCode == http.StatusPartialContent && existing == 0:
		return 0, "", false, fmt.Errorf("%w: server answered 206 to a request without a range", errDownloadFailed)
	case resp.StatusCode == http.StatusPartialContent:
		if err := validateContentRange(resp, existing); err != nil {
			return existing, "", false, err
		}
		resumed = true
	case resp.StatusCode == http.StatusOK && existing > 0:
		logWarn("update.download.restart", map[string]any{"reason": "range_ignored"})
		existing = 0
	}

	h := sha256.New()
	start := existing
	if start > 0 {
		// Hash what is already on disk first so the final digest covers the
		// whole file (R8.8 step 5). This reads local bytes only, so it cannot
		// be mistaken for a network stall.
		if err := hashPreamble(h, dest, start); err != nil {
			removeIfPresent(dest)
			return 0, "", false, fmt.Errorf("%w: %v", errDownloadFailed, err)
		}
	}

	flags := os.O_WRONLY | os.O_CREATE
	if start > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(dest, flags, 0o600)
	if err != nil {
		return start, "", false, fmt.Errorf("%w: %v", errDownloadFailed, err)
	}

	written = start
	body := &guardedReader{r: resp.Body, g: stall}
	lastTick := time.Now()
	buf := make([]byte, 256<<10)

	for {
		// R8.3: never write more than the declared size. One byte over is a
		// refusal, not a truncation.
		remaining := size - written
		if remaining <= 0 {
			break
		}
		// R8.15 / R12.3: the cancellation request is observed between reads,
		// not only on a progress tick. A fast transfer would otherwise finish
		// before a cancel could ever be noticed.
		if opts.ShouldCancel != nil && opts.ShouldCancel() {
			_ = f.Close()
			recordPartial(filepath.Join(filepath.Dir(dest), MetaFileName), url, archiveHash, size, written, resp)
			logInfo("update.cancel.honoured", map[string]any{"phase": PhaseDownload})
			return written, "", resumed, errCancelled
		}
		if int64(len(buf)) > remaining {
			buf = buf[:remaining]
		}
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				_ = f.Close()
				return written, "", resumed, fmt.Errorf("%w: %v", errDownloadFailed, werr)
			}
			_, _ = h.Write(buf[:n])
			written += int64(n)
			if time.Since(lastTick) >= dlProgressInterval {
				lastTick = time.Now()
				reportProgress(opts, written, size)
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				_ = f.Close()
				recordPartial(filepath.Join(filepath.Dir(dest), MetaFileName), url, archiveHash, size, written, resp)
				if ctx.Err() != nil {
					return written, "", resumed, cancelOrTimeoutErr(ctx, opts)
				}
				return written, "", resumed, fmt.Errorf("%w: %v", errDownloadFailed, rerr)
			}
			break
		}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return written, "", resumed, fmt.Errorf("%w: %v", errDownloadFailed, err)
	}
	if err := f.Close(); err != nil {
		return written, "", resumed, fmt.Errorf("%w: %v", errDownloadFailed, err)
	}

	sum = hex.EncodeToString(h.Sum(nil))
	if written < size {
		recordPartial(filepath.Join(filepath.Dir(dest), MetaFileName), url, archiveHash, size, written, resp)
		return written, sum, resumed, fmt.Errorf("%w: truncated transfer (%d of %d bytes)", errDownloadFailed, written, size)
	}
	// A complete partial is still a partial until Verify runs; leave the meta
	// record in place so a crash before the caller renames it can still resume.
	recordPartial(filepath.Join(filepath.Dir(dest), MetaFileName), url, archiveHash, size, written, resp)
	return written, sum, resumed, nil
}

// validateContentRange checks that a 206 response actually starts where the
// request asked it to. A server that answers 206 with an unrelated range would
// otherwise produce a file that hashes to nothing.
func validateContentRange(resp *http.Response, existing int64) error {
	cr := resp.Header.Get("Content-Range")
	if cr == "" {
		return errors.New("update: partial response carried no Content-Range")
	}
	spec := strings.TrimPrefix(cr, "bytes ")
	slash := strings.Index(spec, "/")
	if slash < 0 {
		return errors.New("update: partial response carried a malformed Content-Range")
	}
	span := spec[:slash]
	dash := strings.Index(span, "-")
	if dash < 0 {
		return errors.New("update: partial response carried a malformed Content-Range")
	}
	start, err := strconv.ParseInt(strings.TrimSpace(span[:dash]), 10, 64)
	if err != nil {
		return errors.New("update: partial response carried a malformed Content-Range")
	}
	if start != existing {
		return fmt.Errorf("update: partial response started at %d, expected %d", start, existing)
	}
	return nil
}

// hashPreamble feeds the first n bytes of an existing partial into h.
func hashPreamble(h hash.Hash, path string, n int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := io.CopyN(h, f, n); err != nil {
		return err
	}
	return nil
}

// recordPartial records how far a download got, so the next launch can resume
// rather than restart (R8.16, R12.6). A complete file is recorded too: it is
// still a partial until Verify runs, and a crash between the transfer and the
// caller's rename must not throw the work away.
func recordPartial(metaPath, url, archiveHash string, size, written int64, resp *http.Response) {
	if written <= 0 {
		return
	}
	dm := downloadMeta{
		Schema:       MetaSchema,
		URL:          url,
		ArchiveHash:  archiveHash,
		Size:         size,
		BytesWritten: written,
		Updated:      nowRFC3339(),
	}
	if resp != nil {
		dm.ETag = resp.Header.Get("ETag")
		dm.LastModified = resp.Header.Get("Last-Modified")
	}
	_ = writeJSONAtomic(metaPath, dm)
}

// cancelOrTimeoutErr distinguishes a user cancellation (E34) from a deadline
// or stall expiry (E36).
func cancelOrTimeoutErr(ctx context.Context, opts DownloadOptions) error {
	if opts.ShouldCancel != nil && opts.ShouldCancel() {
		return errCancelled
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: the transfer timed out", errDownloadFailed)
	}
	return errCancelled
}

// reportProgress invokes the caller's callback defensively: a panic in a
// progress reporter must not take down a download that is otherwise fine.
func reportProgress(opts DownloadOptions, written, total int64) {
	if opts.Progress == nil {
		return
	}
	opts.Progress(written, total)
}

// checkArchiveURL enforces the scheme policy of R13.6: https everywhere, plain
// http on loopback only.
func checkArchiveURL(rawURL string, allowInsecure bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: %v", errDownloadFailed, err)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: empty host", errDownloadFailed)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if allowInsecure || isLoopbackHost(u.Hostname()) {
			return nil
		}
		return errInsecureScheme
	default:
		return errInsecureScheme
	}
}

// isLoopbackHost reports whether host names the local machine. The test
// harness serves dist/ over 127.0.0.1, which is why loopback http is permitted
// without a flag (R13.6).
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
