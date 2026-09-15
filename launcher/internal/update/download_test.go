package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- helpers ---------------------------------------------------------------

// bodyOf builds a deterministic payload of n bytes.
func bodyOf(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + (i % 26))
	}
	return b
}

func shaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// downloadDir returns a fresh update directory shaped like the sidecar's.
func downloadDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), UpdateDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return dir
}

// serveBytes serves payload at /archive with the given range support. The
// Content-Length is set explicitly so the over-delivery guard of R8.3 is
// exercised rather than left to the transport's buffering heuristics.
func serveBytes(t *testing.T, payload []byte, supportRange bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		if supportRange {
			if rng := r.Header.Get("Range"); rng != "" {
				var start int64
				if _, err := fmt.Sscanf(rng, "bytes=%d-", &start); err == nil && start < int64(len(payload)) {
					w.Header().Set("Content-Range",
						fmt.Sprintf("bytes %d-%d/%d", start, len(payload)-1, len(payload)))
					w.WriteHeader(http.StatusPartialContent)
					_, _ = w.Write(payload[start:])
					return
				}
			}
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- §8.4 exact-size success ----------------------------------------------

func TestDownloadExactSizeSuccess(t *testing.T) {
	payload := bodyOf(64 * 1024)
	srv := serveBytes(t, payload, false)
	dir := downloadDir(t)

	var progress []int64
	res, err := DownloadArchive(context.Background(), srv.URL+"/archive",
		filepath.Join(dir, DownloadFileName), int64(len(payload)), DownloadOptions{
			Progress: func(written, total int64) { progress = append(progress, written) },
		})
	if err != nil {
		t.Fatalf("DownloadArchive: %v", err)
	}
	if res.Bytes != int64(len(payload)) {
		t.Fatalf("bytes = %d, want %d", res.Bytes, len(payload))
	}
	if res.SHA256 != shaOf(payload) {
		t.Fatalf("sha = %s, want %s", res.SHA256, shaOf(payload))
	}
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatal("downloaded bytes differ from the served payload")
	}
	// R8.13: the callback always fires once on completion.
	if len(progress) == 0 || progress[len(progress)-1] != int64(len(payload)) {
		t.Fatalf("final progress = %v, want a completion tick at %d", progress, len(payload))
	}
}

// --- §8.1 one byte over is refused ----------------------------------------

func TestDownloadOneByteOverIsRefused(t *testing.T) {
	payload := bodyOf(4096)
	srv := serveBytes(t, payload, false)
	dir := downloadDir(t)

	// Declare one byte less than the server will send. The downloader must
	// stop at the declared bound rather than absorbing the extra byte.
	res, err := DownloadArchive(context.Background(), srv.URL+"/archive",
		filepath.Join(dir, DownloadFileName), int64(len(payload)-1), DownloadOptions{})
	if err == nil {
		t.Fatal("expected a refusal when the server sends more than declared")
	}
	if res.Bytes > int64(len(payload)-1) {
		t.Fatalf("wrote %d bytes, declared %d: the bound was not enforced", res.Bytes, len(payload)-1)
	}
}

// --- §8.1 one byte short is a failure -------------------------------------

func TestDownloadOneByteShortIsAFailure(t *testing.T) {
	payload := bodyOf(4096)
	srv := serveBytes(t, payload, false)
	dir := downloadDir(t)

	res, err := DownloadArchive(context.Background(), srv.URL+"/archive",
		filepath.Join(dir, DownloadFileName), int64(len(payload)+1), DownloadOptions{})
	if err == nil {
		t.Fatal("expected a truncated-transfer failure")
	}
	if !errors.Is(err, errDownloadFailed) {
		t.Fatalf("err = %v, want errDownloadFailed", err)
	}
	if res.Bytes != int64(len(payload)) {
		t.Fatalf("bytes = %d, want %d", res.Bytes, len(payload))
	}
}

// --- §8.3 resume ----------------------------------------------------------

func TestDownloadResumesFromPartial(t *testing.T) {
	payload := bodyOf(32 * 1024)
	srv := serveBytes(t, payload, true)
	dir := downloadDir(t)
	dest := filepath.Join(dir, DownloadFileName)
	half := len(payload) / 2

	// Shape a partial plus the metadata that claims it.
	if err := os.WriteFile(dest, payload[:half], 0o600); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	if err := writeJSONAtomic(filepath.Join(dir, MetaFileName), downloadMeta{
		Schema:       MetaSchema,
		URL:          srv.URL + "/archive",
		Size:         int64(len(payload)),
		BytesWritten: int64(half),
	}); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	res, err := DownloadArchive(context.Background(), srv.URL+"/archive", dest,
		int64(len(payload)), DownloadOptions{})
	if err != nil {
		t.Fatalf("DownloadArchive: %v", err)
	}
	if !res.Resumed {
		t.Fatal("expected the download to report a resume")
	}
	// R8.8 step 5: the digest covers the whole file, not just the new half.
	if res.SHA256 != shaOf(payload) {
		t.Fatalf("resumed sha = %s, want %s", res.SHA256, shaOf(payload))
	}
}

func TestDownloadRestartsWhenServerIgnoresRange(t *testing.T) {
	payload := bodyOf(8192)
	srv := serveBytes(t, payload, false) // no range support: always answers 200
	dir := downloadDir(t)
	dest := filepath.Join(dir, DownloadFileName)
	half := len(payload) / 2

	if err := os.WriteFile(dest, payload[:half], 0o600); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	if err := writeJSONAtomic(filepath.Join(dir, MetaFileName), downloadMeta{
		Schema:       MetaSchema,
		URL:          srv.URL + "/archive",
		Size:         int64(len(payload)),
		BytesWritten: int64(half),
	}); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	// A 200 answer to a Range request means the server sent the whole file, so
	// the partial must be discarded rather than appended to (R8.8 step 3).
	res, err := DownloadArchive(context.Background(), srv.URL+"/archive", dest,
		int64(len(payload)), DownloadOptions{})
	if err != nil {
		t.Fatalf("DownloadArchive: %v", err)
	}
	if res.Resumed {
		t.Fatal("a server that ignored the Range must not count as a resume")
	}
	if res.SHA256 != shaOf(payload) {
		t.Fatalf("sha = %s, want %s", res.SHA256, shaOf(payload))
	}
}

func TestDownloadDiscardsPartialWithDifferentHash(t *testing.T) {
	payload := bodyOf(8192)
	srv := serveBytes(t, payload, true)
	dir := downloadDir(t)
	dest := filepath.Join(dir, DownloadFileName)

	if err := os.WriteFile(dest, payload[:1024], 0o600); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	// R8.9: a partial recorded against a different archive must be discarded.
	if err := writeJSONAtomic(filepath.Join(dir, MetaFileName), downloadMeta{
		Schema:       MetaSchema,
		URL:          srv.URL + "/archive",
		ArchiveHash:  "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		Size:         int64(len(payload)),
		BytesWritten: 1024,
	}); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	res, err := DownloadArchive(context.Background(), srv.URL+"/archive", dest,
		int64(len(payload)), DownloadOptions{ExpectedHash: shaOf(payload)})
	if err != nil {
		t.Fatalf("DownloadArchive: %v", err)
	}
	if res.Resumed {
		t.Fatal("a partial with a different recorded hash must not be resumed")
	}
	if res.SHA256 != shaOf(payload) {
		t.Fatalf("sha = %s, want %s", res.SHA256, shaOf(payload))
	}
}

// --- §8.5 cancellation ----------------------------------------------------

func TestDownloadCancelIsSafeAndResumable(t *testing.T) {
	payload := bodyOf(64 * 1024)
	var sent atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The first chunk is delayed past the client's first read, which is
		// what makes the mid-transfer cancel deterministic: the client is
		// inside Read when the flag flips, so the next loop iteration sees it.
		time.Sleep(50 * time.Millisecond)
		// 1 KiB chunks with an explicit flush: without the flush the response
		// is buffered whole and the client would never observe a partial
		// transfer, which is the situation the cancel is meant to interrupt.
		for i := 0; i < len(payload); i += 1024 {
			end := i + 1024
			if end > len(payload) {
				end = len(payload)
			}
			if _, err := w.Write(payload[i:end]); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			sent.Add(int64(end - i))
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer srv.Close()

	dir := downloadDir(t)
	dest := filepath.Join(dir, DownloadFileName)
	// The cancel arrives mid-transfer, which is the case §12 is about: a user
	// cancelling work that is genuinely in progress.
	const cancelAfter = 32 * 1024
	var cancelled atomic.Bool

	res, err := DownloadArchive(context.Background(), srv.URL+"/archive", dest,
		int64(len(payload)), DownloadOptions{
			ShouldCancel: func() bool {
				if sent.Load() >= cancelAfter {
					cancelled.Store(true)
				}
				return cancelled.Load()
			},
		})
	if !IsCancelled(err) {
		t.Fatalf("err = %v, want a cancellation", err)
	}
	if res.Bytes == 0 || res.Bytes >= int64(len(payload)) {
		t.Fatalf("bytes = %d, want a partial transfer", res.Bytes)
	}
	// R12.6: the partial and its metadata survive a cancel so a re-request
	// resumes instead of starting over.
	if st, serr := os.Stat(dest); serr != nil || st.Size() == 0 {
		t.Fatalf("partial was removed by the cancel: %v", serr)
	}
	var meta downloadMeta
	if ok, merr := readJSON(filepath.Join(dir, MetaFileName), &meta); !ok || merr != nil {
		t.Fatalf("metadata was not kept: ok=%v err=%v", ok, merr)
	}
	// The install must be untouched: nothing but .part and the metadata exist.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		switch e.Name() {
		case DownloadFileName, MetaFileName:
		default:
			t.Fatalf("cancel left unexpected artefact %q", e.Name())
		}
	}
}

// TestDownloadCancelBeforeFirstByte covers the other half of §12: a request
// already recorded as cancelled must not transfer anything at all, and must
// report the cancellation rather than a network failure.
func TestDownloadCancelBeforeFirstByte(t *testing.T) {
	payload := bodyOf(4096)
	srv := serveBytes(t, payload, false)
	dir := downloadDir(t)

	res, err := DownloadArchive(context.Background(), srv.URL+"/archive",
		filepath.Join(dir, DownloadFileName), int64(len(payload)), DownloadOptions{
			ShouldCancel: func() bool { return true },
		})
	if !IsCancelled(err) {
		t.Fatalf("err = %v, want a cancellation", err)
	}
	if res.Bytes != 0 {
		t.Fatalf("bytes = %d, want 0", res.Bytes)
	}
}

// --- §8.5 stall watchdog --------------------------------------------------

func TestDownloadStallTimeoutAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Send nothing, then hold the connection open well past the timeout.
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	dir := downloadDir(t)
	start := time.Now()
	_, err := DownloadArchive(context.Background(), srv.URL+"/archive",
		filepath.Join(dir, DownloadFileName), 1024, DownloadOptions{
			StallTimeout: 200 * time.Millisecond,
		})
	if err == nil {
		t.Fatal("expected a stalled download to fail")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("stall watchdog took %v; the idle deadline was not applied", elapsed)
	}
}

// --- §8.2 the 20-second trap ----------------------------------------------

func TestDownloadClientHasNoTotalTimeout(t *testing.T) {
	if downloadClient.Timeout != 0 {
		t.Fatalf("downloadClient.Timeout = %v: a total-request timeout kills long downloads (R8.4)",
			downloadClient.Timeout)
	}
	if httpClient.Timeout != manifestTimeout {
		t.Fatalf("the manifest client's timeout changed; the downloader must not share it")
	}
}

// --- §8.6 bounds ----------------------------------------------------------

func TestDownloadRefusesDeclaredSizeOverCeiling(t *testing.T) {
	srv := serveBytes(t, bodyOf(1024), false)
	dir := downloadDir(t)

	_, err := DownloadArchive(context.Background(), srv.URL+"/archive",
		filepath.Join(dir, DownloadFileName), int64(len(bodyOf(1024))), DownloadOptions{
			MaxArchiveBytes: 16,
		})
	if !errors.Is(err, errArchiveTooLarge) {
		t.Fatalf("err = %v, want errArchiveTooLarge", err)
	}
}

func TestDownloadRefusesZeroSize(t *testing.T) {
	srv := serveBytes(t, bodyOf(1024), false)
	dir := downloadDir(t)

	_, err := DownloadArchive(context.Background(), srv.URL+"/archive",
		filepath.Join(dir, DownloadFileName), 0, DownloadOptions{})
	if !errors.Is(err, errNoArchiveSize) {
		t.Fatalf("err = %v, want errNoArchiveSize", err)
	}
}

func TestDownloadRefusesNonLoopbackPlainHTTP(t *testing.T) {
	dir := downloadDir(t)
	_, err := DownloadArchive(context.Background(), "http://example.invalid/archive",
		filepath.Join(dir, DownloadFileName), 1024, DownloadOptions{})
	if !errors.Is(err, errInsecureScheme) {
		t.Fatalf("err = %v, want errInsecureScheme", err)
	}
}

func TestDownloadPermitsLoopbackHTTP(t *testing.T) {
	if err := checkArchiveURL("http://127.0.0.1:8080/a", false); err != nil {
		t.Fatalf("loopback http must be permitted: %v", err)
	}
	if err := checkArchiveURL("http://localhost:8080/a", false); err != nil {
		t.Fatalf("localhost http must be permitted: %v", err)
	}
	if err := checkArchiveURL("http://10.0.0.5/a", false); err == nil {
		t.Fatal("a remote cleartext base must be refused")
	}
	if err := checkArchiveURL("https://example.com/a", false); err != nil {
		t.Fatalf("https must be permitted: %v", err)
	}
}

// --- redirects (§8.2) -----------------------------------------------------

func TestDownloadRedirectCap(t *testing.T) {
	var hops atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops.Add(1)
		http.Redirect(w, r, "/archive", http.StatusFound)
	}))
	defer srv.Close()

	dir := downloadDir(t)
	_, err := DownloadArchive(context.Background(), srv.URL+"/archive",
		filepath.Join(dir, DownloadFileName), 1024, DownloadOptions{})
	if err == nil {
		t.Fatal("expected a redirect chain to be refused")
	}
	if hops.Load() > int64(maxRedirects)+1 {
		t.Fatalf("followed %d redirects, cap is %d", hops.Load(), maxRedirects)
	}
}

func TestDownloadRefusesHTTPSDowngradeRedirect(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/a", nil)
	via := []*http.Request{mustRequest(t, "https://example.invalid/a")}
	err := checkDownloadRedirect(req, via)
	if err == nil {
		t.Fatal("a redirect from https to http must be refused (R8.6)")
	}
}

func mustRequest(t *testing.T, raw string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return r
}

// --- HTTP status handling -------------------------------------------------

func TestDownloadRejectsErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	dir := downloadDir(t)
	_, err := DownloadArchive(context.Background(), srv.URL+"/archive",
		filepath.Join(dir, DownloadFileName), 1024, DownloadOptions{})
	if !errors.Is(err, errDownloadFailed) {
		t.Fatalf("err = %v, want errDownloadFailed", err)
	}
	// A 404 leaves nothing behind that a later launch could mistake for work.
	if _, serr := os.Stat(filepath.Join(dir, DownloadFileName)); serr == nil {
		t.Fatal("a failed download left a .part behind for a non-resumable failure")
	}
}

func TestDownloadEmptyBodyIsTruncation(t *testing.T) {
	srv := serveBytes(t, nil, false)
	dir := downloadDir(t)

	_, err := DownloadArchive(context.Background(), srv.URL+"/archive",
		filepath.Join(dir, DownloadFileName), 1024, DownloadOptions{})
	if !errors.Is(err, errDownloadFailed) {
		t.Fatalf("err = %v, want errDownloadFailed", err)
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("err = %v, want a truncation diagnosis", err)
	}
}

// TestContentRangeParsing covers the 206 validation that keeps a resumed file
// from being spliced out of two unrelated ranges.
func TestContentRangeParsing(t *testing.T) {
	cases := []struct {
		header   string
		existing int64
		wantErr  bool
	}{
		{"bytes 100-499/500", 100, false},
		{"bytes 0-99/100", 0, false},
		{"bytes 50-99/100", 100, true}, // starts where we did not ask
		{"", 100, true},
		{"bytes 100/500", 100, true},
		{"garbage", 100, true},
	}
	for _, tc := range cases {
		resp := &http.Response{Header: http.Header{}}
		if tc.header != "" {
			resp.Header.Set("Content-Range", tc.header)
		}
		err := validateContentRange(resp, tc.existing)
		if (err != nil) != tc.wantErr {
			t.Fatalf("Content-Range %q vs %d: err = %v, wantErr = %v",
				tc.header, tc.existing, err, tc.wantErr)
		}
	}
}

// TestDownloadTimeoutScalesWithSize pins R8.18: the deadline is a backstop, and
// it grows with the declared size rather than being a fixed short value.
func TestDownloadTimeoutScalesWithSize(t *testing.T) {
	small := downloadTimeout(0)
	if small != MinDownloadTimeout {
		t.Fatalf("timeout(0) = %v, want %v", small, MinDownloadTimeout)
	}
	big := downloadTimeout(1 << 30) // 1 GiB at 64 KiB/s is ~4.5 hours
	if big <= MinDownloadTimeout {
		t.Fatalf("timeout(1GiB) = %v, want more than %v", big, MinDownloadTimeout)
	}
}

// TestDownloadSizeBoundIsExactThroughManyChunks exercises the boundary logic on
// a payload larger than the copy buffer, where the buffer must be shrunk to the
// remaining allowance.
func TestDownloadSizeBoundIsExactThroughManyChunks(t *testing.T) {
	payload := bodyOf((256 << 10) + 1234)
	srv := serveBytes(t, payload, false)
	dir := downloadDir(t)

	res, err := DownloadArchive(context.Background(), srv.URL+"/archive",
		filepath.Join(dir, DownloadFileName), int64(len(payload)), DownloadOptions{})
	if err != nil {
		t.Fatalf("DownloadArchive: %v", err)
	}
	if res.Bytes != int64(len(payload)) {
		t.Fatalf("bytes = %d, want %d", res.Bytes, len(payload))
	}
}

// TestDownloadMetaRoundTrip keeps the on-disk shape of the resume record pinned.
func TestDownloadMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MetaFileName)
	want := downloadMeta{
		Schema:       MetaSchema,
		URL:          "https://example.invalid/a.tar.zst",
		ArchiveHash:  "sha256:abc",
		Size:         100,
		BytesWritten: 50,
		ETag:         `"v1"`,
		LastModified: "Mon, 01 Jan 2026 00:00:00 GMT",
		Updated:      nowRFC3339(),
	}
	if err := writeJSONAtomic(path, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	var got downloadMeta
	if ok, err := readJSON(path, &got); !ok || err != nil {
		t.Fatalf("read: ok=%v err=%v", ok, err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	if !got.matches(want.URL, want.ArchiveHash, want.Size) {
		t.Fatal("a freshly written partial must be resumable")
	}
	if got.matches(want.URL, "sha256:different", want.Size) {
		t.Fatal("a partial with a different archive hash must not match")
	}
}

// TestDownloadFileModeIsPrivate pins R5.5: the partial archive is private.
func TestDownloadFileModeIsPrivate(t *testing.T) {
	payload := bodyOf(2048)
	srv := serveBytes(t, payload, false)
	dir := downloadDir(t)

	res, err := DownloadArchive(context.Background(), srv.URL+"/archive",
		filepath.Join(dir, DownloadFileName), int64(len(payload)), DownloadOptions{})
	if err != nil {
		t.Fatalf("DownloadArchive: %v", err)
	}
	st, err := os.Stat(res.Path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("partial mode = %o, want 600", perm)
	}
}

// TestDownloadRefusesOversizedContentLength pins R8.2/R8.3: a response that
// announces more bytes than the manifest declares is refused before anything is
// written, rather than absorbed up to the declared bound.
func TestDownloadRefusesOversizedContentLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Announce far more than will actually be sent.
		w.Header().Set("Content-Length", strconv.Itoa(1024*100))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bodyOf(1024))
	}))
	defer srv.Close()

	dir := downloadDir(t)
	_, err := DownloadArchive(context.Background(), srv.URL+"/archive",
		filepath.Join(dir, DownloadFileName), 1024, DownloadOptions{})
	if !errors.Is(err, errDownloadFailed) {
		t.Fatalf("err = %v, want a refusal for an oversized announcement", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, DownloadFileName)); serr == nil {
		t.Fatal("a refused download left a .part behind")
	}
}

// TestDownloadCapsWritesAtDeclaredSize pins the other direction: the declared
// size governs the write, so a stream longer than the manifest cannot grow the
// file past the confirmed size.
func TestDownloadCapsWritesAtDeclaredSize(t *testing.T) {
	payload := bodyOf(8192)
	// No Content-Length, so the server's length cannot be used as a refusal;
	// the bound must come from the manifest.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dir := downloadDir(t)
	declared := int64(4096)
	res, err := DownloadArchive(context.Background(), srv.URL+"/archive",
		filepath.Join(dir, DownloadFileName), declared, DownloadOptions{})
	if err != nil {
		t.Fatalf("DownloadArchive: %v", err)
	}
	if res.Bytes != declared {
		t.Fatalf("bytes = %d, want the declared %d", res.Bytes, declared)
	}
	st, serr := os.Stat(res.Path)
	if serr != nil {
		t.Fatalf("stat: %v", serr)
	}
	if st.Size() != declared {
		t.Fatalf("file size = %d, want %d", st.Size(), declared)
	}
}
