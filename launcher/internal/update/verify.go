package update

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"

	"kobragames.local/launcher/internal/kobraerr"
)

// Fixed, path-free messages (§22.4). Every user-visible error in this package
// uses one of these; identifiers, entry names and paths go into Cause only.
const (
	msgDamaged   = "The update file is damaged. Re-download it and try again."
	msgUnsafe    = "The update was rejected because its contents are not safe to install."
	msgData      = "The update was rejected because it would modify your saves."
	msgSignature = "The update could not be verified: its signature could not be verified."
	// msgIncomplete is a defect in the release, not in the user's download, so
	// it deliberately does not tell the user to re-download (§8.2.3.3).
	msgIncomplete = "The update was refused because its release manifest does not describe the update file."
	// msgEmbeddedManifest is the §6.4 rule stated to a human: the manifest
	// cannot be inside the archive it indexes.
	msgEmbeddedManifest = "The update was refused because the release manifest was packaged inside the update archive."
)

// ReleaseManifestName is the package-relative path of the release manifest
// inside a game folder. It is the one path that MUST NOT appear in an update
// archive: §6.4 excludes the manifest from its own file index, and an archive
// member no index lists cannot be verified or extracted (FR-UPD-1).
//
// The manifest is a *publication* document. The copy shipped in the
// distribution ZIP exists for support inspection; the update archive omits it,
// exactly as it omits data/, README.txt and LICENSES/ (§1.2, §9.1). The
// launcher does not need it in either case: /api/update/check falls back to
// launcher.config.json's release (§19.2).
const ReleaseManifestName = "game/release.manifest.json"

// SignatureVerifier verifies a detached signature over a release manifest.
// manifestJSON is the manifest document exactly as fetched and m is its decoded
// form.
//
// No verifier ships with the launcher. §19.3 step 2 and FR-LNCH-5 require the
// manifest's signature to be verified if signatures.gpg is present, but §3.4
// deliberately keeps the dependency set tiny and the spec defers the choice of
// verifier, so there is nothing here that could honestly check a GPG
// signature. Rather than hand-roll a parser that would give false assurance,
// Verify fails closed unless a build installs a real verifier with
// SetSignatureVerifier. Deferred per §19.3 step 2 / FR-LNCH-5.
type SignatureVerifier func(manifestJSON []byte, m *Manifest) error

var (
	verifierMu        sync.RWMutex
	signatureVerifier SignatureVerifier
)

// SetSignatureVerifier installs the verifier used for manifests that declare a
// signature. It is intended for a future build or for tests; the default is
// nil, which makes Verify refuse any signed manifest. Passing nil removes the
// verifier again.
func SetSignatureVerifier(v SignatureVerifier) {
	verifierMu.Lock()
	signatureVerifier = v
	verifierMu.Unlock()
}

func currentVerifier() SignatureVerifier {
	verifierMu.RLock()
	defer verifierMu.RUnlock()
	return signatureVerifier
}

// declaresSignature reports whether the manifest carries a detached signature
// that §19.3 step 2 requires the launcher to check.
func declaresSignature(m *Manifest) bool {
	return m != nil && m.Signatures != nil &&
		(m.Signatures.GPG != "" || m.Signatures.SigstoreBundle != "")
}

// Verify checks a downloaded archive against the manifest before extraction
// (§19.3):
//
//  1. the manifest's own signature, if declared — fails closed (see
//     SignatureVerifier);
//  2. the declared compressed archive size, when the manifest declares one;
//  3. that the manifest declares an archive hash at all, then that the archive
//     matches it — a manifest without one is refused rather than downgraded to
//     per-file verification (§8.2.3.3);
//  4. every archive member against the manifest's mandatory per-file index
//     (path, size and hash), rejecting any member the index does not list —
//     including ReleaseManifestName, which must never be inside the archive it
//     indexes (§6.4);
//  5. the byte count actually decompressed against the manifest's declared
//     total_size, so a zip bomb or a truncated archive is caught before any
//     file is written.
//
// The check is streaming: the archive is never fully buffered.
func Verify(archivePath string, m *Manifest) error {
	if m == nil {
		return kobraerr.IO(msgDamaged, map[string]any{"reason": "no_manifest"}, nil)
	}

	if declaresSignature(m) {
		v := currentVerifier()
		if v == nil {
			return verifyFail(m, "signature_unavailable",
				kobraerr.IO(msgSignature, map[string]any{"reason": "signature_unavailable"},
					errors.New("update: manifest declares a signature but no verifier is installed")))
		}
		if err := v(m.raw, m); err != nil {
			return verifyFail(m, "signature_invalid",
				kobraerr.IO(msgSignature, map[string]any{"reason": "signature_invalid"}, err))
		}
	}

	st, err := os.Stat(archivePath)
	if err != nil {
		return verifyFail(m, "archive_unreadable",
			kobraerr.IO(msgDamaged, map[string]any{"reason": "archive_unreadable"}, err))
	}
	if want := m.declaredArchiveSize(); want > 0 && st.Size() != want {
		return verifyFail(m, "archive_size",
			kobraerr.IO(msgDamaged, map[string]any{"reason": "archive_size"},
				fmt.Errorf("update: archive is %d bytes, manifest declares %d", st.Size(), want)))
	}

	// §8.2.3.3: a manifest that does not name the archive it describes leaves
	// only the per-file index as an integrity claim, which is the silent
	// downgrade §8.2.1 rejects. Fail closed. The check runs before the archive
	// is hashed so a malformed release is refused without reading it.
	want := m.declaredArchiveHash()
	if want == "" {
		return verifyFail(m, "archive_hash_missing",
			kobraerr.IO(msgIncomplete, map[string]any{"reason": "archive_hash_missing"},
				errors.New("update: manifest declares neither archive.hash nor archive_sha256")))
	}
	sum, _, err := hashFile(archivePath)
	if err != nil {
		return verifyFail(m, "archive_unreadable",
			kobraerr.IO(msgDamaged, map[string]any{"reason": "archive_unreadable"}, err))
	}
	ok, err := hashMatches(want, sum)
	if err != nil {
		return verifyFail(m, "archive_hash",
			kobraerr.IO(msgDamaged, map[string]any{"reason": "archive_hash"}, err))
	}
	if !ok {
		return verifyFail(m, "archive_hash",
			kobraerr.IO(msgDamaged, map[string]any{"reason": "archive_hash"},
				fmt.Errorf("update: archive hash mismatch (manifest %s, archive %s)", redactHash(want), redactHash(sum))))
	}

	index := indexFiles(m.Files)
	var total int64
	err = walkArchive(archivePath, func(ent archiveEntry, body io.Reader) error {
		if ent.Kind == kindDir {
			// A directory entry carries no content and no size budget.
			if _, err := io.Copy(io.Discard, body); err != nil {
				return err
			}
			return nil
		}
		if len(index) > 0 {
			want, ok := index[ent.Rel]
			if !ok {
				return &entryViolation{Reason: "unlisted_entry", Entry: ent.Rel,
					Detail: "archive member is not in the manifest file index"}
			}
			if want.Size != ent.Header.Size {
				return fmt.Errorf("update: entry size mismatch for %q: manifest %d, archive %d",
					ent.Rel, want.Size, ent.Header.Size)
			}
			h := sha256.New()
			n, err := io.Copy(h, body)
			if err != nil {
				return err
			}
			total += n
			actual := "sha256:" + hex.EncodeToString(h.Sum(nil))
			matched, err := hashMatches(want.Hash, actual)
			if err != nil {
				return err
			}
			if !matched {
				return fmt.Errorf("update: entry hash mismatch for %q", ent.Rel)
			}
			return nil
		}
		n, err := io.Copy(io.Discard, body)
		total += n
		return err
	})
	if err != nil {
		var v *entryViolation
		if errors.As(err, &v) {
			return verifyEntryViolation(m, v)
		}
		return verifyFail(m, "entry_content",
			kobraerr.IO(msgDamaged, map[string]any{"reason": "entry_content"}, err))
	}

	if total != m.TotalSize {
		return verifyFail(m, "total_size",
			kobraerr.IO(msgDamaged, map[string]any{"reason": "total_size"},
				fmt.Errorf("update: extracted total is %d bytes, manifest declares %d", total, m.TotalSize)))
	}

	logInfo("update.verify.ok", map[string]any{"release": m.Release})
	return nil
}

// verifyEntryViolation maps a rejected archive member to the right public
// error and event. A data/ entry is E30 (FR-UPD-7); an embedded release
// manifest is the §6.4 rule; everything else means the archive is unsafe to
// install.
func verifyEntryViolation(m *Manifest, v *entryViolation) error {
	if v.Reason == "data" {
		logError("update.data.rejected", map[string]any{"release": m.Release})
		return kobraerr.IO(msgData, map[string]any{"reason": "data_dir"}, v)
	}
	if v.Reason == "unlisted_entry" && isReleaseManifestPath(v.Entry) {
		logError("update.manifest.rejected", map[string]any{"release": m.Release})
		return kobraerr.IO(msgEmbeddedManifest,
			map[string]any{"reason": "release_manifest_in_archive"}, v)
	}
	return verifyFail(m, "unsafe_entry",
		kobraerr.IO(msgUnsafe, map[string]any{"reason": v.Reason}, v))
}

// isReleaseManifestPath reports whether an archive member is the release
// manifest, tolerating a "./" prefix and a cleaned path so that a crafted
// archive cannot slip past by spelling the same entry differently.
func isReleaseManifestPath(entry string) bool {
	return path.Clean(strings.TrimPrefix(entry, "./")) == ReleaseManifestName
}

// verifyFail logs the update.verify.fail event (Appendix C.5) and returns err
// unchanged.
func verifyFail(m *Manifest, reason string, err error) error {
	logError("update.verify.fail", map[string]any{"release": m.Release, "reason": reason})
	return err
}

// indexFiles turns the manifest's file index into a lookup keyed by the
// cleaned slash path.
func indexFiles(files []FileEntry) map[string]FileEntry {
	if len(files) == 0 {
		return nil
	}
	out := make(map[string]FileEntry, len(files))
	for _, f := range files {
		out[path.Clean(f.Path)] = f
	}
	return out
}

// hashFile returns the file's SHA-256 as "sha256:<hex>" and its size.
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), n, nil
}

// hashMatches compares a declared "sha256:<hex>" hash with an actual one,
// tolerating case in the hex digits. A declared hash in any other form is an
// error: the schema requires sha256.
func hashMatches(declared, actual string) (bool, error) {
	d, err := parseHash(declared)
	if err != nil {
		return false, err
	}
	a, err := parseHash(actual)
	if err != nil {
		return false, err
	}
	return d == a, nil
}

func parseHash(s string) (string, error) {
	lower := strings.ToLower(strings.TrimSpace(s))
	algo, hexPart, ok := strings.Cut(lower, ":")
	if !ok || algo != "sha256" {
		return "", fmt.Errorf("update: manifest hash %q is not a sha256 hash", redactHash(s))
	}
	if len(hexPart) != 64 {
		return "", fmt.Errorf("update: manifest hash %q is not a sha256 hash", redactHash(s))
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return "", fmt.Errorf("update: manifest hash %q is not a sha256 hash", redactHash(s))
	}
	return hexPart, nil
}

// redactHash shortens a hash for an internal cause message so that even a
// malformed value cannot smuggle a long path-like string into a log line.
func redactHash(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:16] + "…"
}
