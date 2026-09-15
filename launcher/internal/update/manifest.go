package update

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"kobragames.local/launcher/internal/kobraerr"
)

// Manifest mirrors release.manifest.schema.json. Unknown top-level fields are
// tolerated, so a newer release process can add fields without breaking an
// older launcher.
//
// The archive identity is declared either as the structured "archive" object
// (archive.hash and archive.size, both required by the schema) or through the
// flat "archive_sha256" / "archive_hash" aliases this struct also accepts. A
// manifest that declares neither is REFUSED by Verify rather than downgraded to
// per-file verification (§8.2.3.3): the per-file index is an additional check,
// not a fallback.
type Manifest struct {
	Schema           string            `json:"schema"`
	Release          string            `json:"release"`
	PreviousRelease  *string           `json:"previous_release,omitempty"`
	Published        string            `json:"published,omitempty"`
	LauncherMin      string            `json:"launcher_min"`
	EngineVersion    string            `json:"engine_version"`
	SaveVersion      int               `json:"save_version"`
	Channel          string            `json:"channel,omitempty"`
	Scope            string            `json:"scope,omitempty"`
	TotalSize        int64             `json:"total_size"`
	Files            []FileEntry       `json:"files"`
	Migrations       []Migration       `json:"migrations,omitempty"`
	SchemaMigrations []SchemaMigration `json:"schema_migrations,omitempty"`
	MinBrowser       *MinBrowser       `json:"min_browser,omitempty"`
	Signatures       *Signatures       `json:"signatures,omitempty"`
	Notes            string            `json:"notes,omitempty"`
	ReleaseNotesURL  string            `json:"release_notes_url,omitempty"`

	// Archive optionally describes the payload archive. Not in the published
	// schema; see the struct comment.
	Archive *ArchiveRef `json:"archive,omitempty"`
	// ArchiveSHA256 is a flat alias for Archive.Hash. Not in the published
	// schema; see the struct comment.
	ArchiveSHA256 string `json:"archive_sha256,omitempty"`

	// raw is the manifest document exactly as fetched. It is what a detached
	// signature would cover, so it is handed to an installed SignatureVerifier
	// and never serialised.
	raw []byte
}

// FileEntry is one element of the manifest's mandatory file index. Hashes are
// mandatory for update verification (FR-UPD-1), unlike asset-manifest hashes.
type FileEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Hash string `json:"hash"`
	Mode string `json:"mode,omitempty"`
}

// Migration is one step of the ordered save-migration chain (FR-SAVE-12).
type Migration struct {
	From       int    `json:"from"`
	To         int    `json:"to"`
	Fn         string `json:"fn"`
	Reversible bool   `json:"reversible,omitempty"`
}

// SchemaMigration is a one-way manifest schema upgrade (FR-SCH-4).
type SchemaMigration struct {
	Manifest string `json:"manifest"`
	From     int    `json:"from"`
	To       int    `json:"to"`
}

// MinBrowser is the release's minimum browser build per engine.
type MinBrowser struct {
	Chrome int `json:"chrome,omitempty"`
	Edge   int `json:"edge,omitempty"`
	Opera  int `json:"opera,omitempty"`
}

// Signatures are optional detached signatures over the manifest itself
// (FR-LNCH-3). See the package comment for the deferred verification path.
type Signatures struct {
	GPG               string `json:"gpg,omitempty"`
	GPGKeyFingerprint string `json:"gpg_key_fingerprint,omitempty"`
	SigstoreBundle    string `json:"sigstore_bundle,omitempty"`
}

// ArchiveRef describes the release payload archive.
type ArchiveRef struct {
	Name   string `json:"name,omitempty"`
	URL    string `json:"url,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Hash   string `json:"hash,omitempty"`
	Format string `json:"format,omitempty"`
}

// declaredArchiveHash returns the archive hash the manifest declares, if any.
func (m *Manifest) declaredArchiveHash() string {
	if m == nil {
		return ""
	}
	if m.ArchiveSHA256 != "" {
		return m.ArchiveSHA256
	}
	if m.Archive != nil {
		return m.Archive.Hash
	}
	return ""
}

// declaredArchiveSize returns the declared compressed archive size, or 0 when
// the manifest does not declare one.
func (m *Manifest) declaredArchiveSize() int64 {
	if m == nil || m.Archive == nil {
		return 0
	}
	return m.Archive.Size
}

// Latest is the small /latest.json document naming the newest release. It is
// fetched from <patch_base_url>/latest.json (§19.2 step 2) and deliberately
// carries nothing else: the update check does not download the patch.
type Latest struct {
	Release     string `json:"release"`
	ManifestURL string `json:"manifest_url"`
	NotesURL    string `json:"notes_url,omitempty"`
}

// FetchLatest downloads <baseURL>/latest.json with a 10 s timeout.
func FetchLatest(ctx context.Context, baseURL string) (*Latest, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, kobraerr.MalformedField("patch_base_url", "The update address is not configured.", nil)
	}
	docURL, err := resolveURL(baseURL, "latest.json")
	if err != nil {
		return nil, kobraerr.IO("The update address could not be understood.",
			map[string]any{"reason": "bad_url"}, err)
	}

	ctx, cancel := context.WithTimeout(ctx, latestTimeout)
	defer cancel()

	body, err := fetchDocument(ctx, docURL, maxDocumentBytes)
	if err != nil {
		return nil, err
	}
	var latest Latest
	if err := json.Unmarshal(body, &latest); err != nil {
		return nil, kobraerr.IO("The update information was not readable.",
			map[string]any{"reason": "malformed_latest"}, err)
	}
	if !validRelease(latest.Release) {
		return nil, kobraerr.MalformedField("release", "The update information was not readable.", nil)
	}
	if strings.TrimSpace(latest.ManifestURL) == "" {
		return nil, kobraerr.MalformedField("manifest_url", "The update information was not readable.", nil)
	}
	// latest.json may publish a relative manifest_url; resolve it against the
	// document's own URL.
	if resolved, err := resolveURL(docURL, latest.ManifestURL); err == nil {
		latest.ManifestURL = resolved
	}
	return &latest, nil
}

// FetchManifest downloads and decodes a release manifest document. Unknown
// fields are tolerated (the schema allows them); the fields §19 depends on are
// validated here so that Verify and Extract can trust them structurally.
func FetchManifest(ctx context.Context, manifestURL string) (*Manifest, error) {
	if strings.TrimSpace(manifestURL) == "" {
		return nil, kobraerr.MalformedField("manifest_url", "The update manifest address is missing.", nil)
	}
	ctx, cancel := context.WithTimeout(ctx, manifestTimeout)
	defer cancel()

	body, err := fetchDocument(ctx, manifestURL, maxDocumentBytes)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, kobraerr.IO("The update manifest was not readable.",
			map[string]any{"reason": "malformed_manifest"}, err)
	}
	if !validRelease(m.Release) {
		return nil, kobraerr.MalformedField("release", "The update manifest was not readable.", nil)
	}
	// §19.5/FR-UPD-6: launcher_min is a floor, so it has to be readable. A present
	// but unparseable value is refused here rather than left to MeetsLauncherMin,
	// because compareVersions orders an unparseable version as older than every
	// numeric one — which for a floor reads as "always satisfied" and disables the
	// gate outright. An absent launcher_min stays "no requirement", the leniency
	// MeetsLauncherMin documents. VERSIONING.md: an unparseable version fails its
	// gate closed.
	if t := strings.TrimSpace(m.LauncherMin); t != "" {
		if _, ok := parseVersion(t); !ok {
			return nil, kobraerr.MalformedField("launcher_min", "The update manifest was not readable.", nil)
		}
	}
	if m.TotalSize < 0 {
		return nil, kobraerr.MalformedField("total_size", "The update manifest was not readable.", nil)
	}
	if m.Schema != "" && m.Schema != ReleaseManifestSchema {
		return nil, kobraerr.MalformedField("schema", "The update manifest was not readable.", nil)
	}
	m.raw = body
	return &m, nil
}

// LauncherTooOldMessage is the exact E29 wording (§19.5, FS §16 error table):
// the release refuses to run under an older launcher, and the message names
// the one thing the user can do about it.
const LauncherTooOldMessage = "This update needs a newer launcher."

// ManifestUnreadableMessage is the exact E37 wording (Updater spec §16.2).
//
// An unreadable launcher_min is not the launcher being old. Reporting E29 there
// would send the player looking for a launcher that does not exist and offer a
// remedy that cannot work, when the fault is the publisher's document — so
// R16.5 requires the two refusals to stay distinct. FetchManifest refuses such a
// manifest before this point; this wording exists for the defence-in-depth
// branch in MeetsLauncherMin.
const ManifestUnreadableMessage = "The update manifest was not readable."

// MeetsLauncherMin reports whether launcherVersion satisfies the manifest's
// declared launcher_min (§19.5, FR-UPD-6). A nil return means the floor is met.
// Otherwise the error carries the wording the caller MUST surface, and the
// caller MUST refuse to apply the update while leaving the current release
// runnable.
//
// The two refusals are deliberately distinct (Updater spec R16.5):
//
//   - E29, LauncherTooOldMessage: the launcher is older than a floor that was
//     read. The remedy is a newer launcher, and the text names it.
//   - E37, ManifestUnreadableMessage: the floor itself cannot be read, so there
//     is nothing the player can do and the publisher owns the fix.
//
// Check takes no launcher version — §19.2 defines the check as comparing
// release identifiers only — so this function exists to be called on the apply
// path, immediately after FetchManifest and before Verify, where the launcher
// version is known. It is also safe to call on the check path once a manifest
// has been fetched. A missing manifest or an empty launcher_min means "no
// requirement" and satisfies the check. An unparseable launcher version fails
// the check closed: the launcher cannot prove it is new enough. An unparseable
// launcher_min fails it closed too, for the opposite reason: a floor that cannot
// be read cannot be proven satisfied (VERSIONING.md).
func MeetsLauncherMin(m *Manifest, launcherVersion string) error {
	err, _ := launcherMinRefusal(m, launcherVersion)
	return err
}

// launcherMinRefusal returns the floor refusal together with the stable
// diagnostics reason for it, or (nil, "") when the floor is met. Keeping the
// error and the reason in one place is what stops GuardApply from having to
// infer which refusal it received from the message text.
func launcherMinRefusal(m *Manifest, launcherVersion string) (error, string) {
	if m == nil || strings.TrimSpace(m.LauncherMin) == "" {
		return nil, ""
	}
	// Fail closed on an unreadable floor. compareVersions orders an unparseable
	// version as older than every numeric one, so without this check a malformed
	// launcher_min would read as older than the launcher and satisfy the gate
	// unconditionally — the opposite of what a floor means. FetchManifest refuses
	// such a manifest first; this keeps the gate safe for every other caller.
	if _, ok := parseVersion(m.LauncherMin); !ok {
		return kobraerr.MalformedField("launcher_min", ManifestUnreadableMessage, nil),
			"launcher_min_unreadable"
	}
	if compareVersions(launcherVersion, m.LauncherMin) >= 0 {
		return nil, ""
	}
	return kobraerr.IO(LauncherTooOldMessage, map[string]any{"reason": "launcher_min"}, nil),
		"launcher_min"
}

// --- version helpers -------------------------------------------------------

// compareVersions compares two dotted numeric versions component by component,
// treating a missing component as zero. It returns -1, 0 or +1. A version with
// a non-numeric component compares as older than any numeric version.
//
// That last rule makes an unparseable value "older than everything", which is
// the safe direction for an upper bound (the launcher's own version: it cannot
// prove itself new enough) and the unsafe direction for a floor (launcher_min:
// it would be satisfied unconditionally). This function is therefore only
// defined for parseable input; a caller that can receive an unreadable floor
// MUST gate on parseVersion first, as MeetsLauncherMin and FetchManifest do.
// See VERSIONING.md, "an unparseable version fails its gate closed".
func compareVersions(a, b string) int {
	as, aok := parseVersion(a)
	bs, bok := parseVersion(b)
	switch {
	case !aok && !bok:
		return 0
	case !aok:
		return -1
	case !bok:
		return 1
	}
	for i := 0; i < len(as) || i < len(bs); i++ {
		var av, bv int
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

// ReleaseNewer reports whether release a is newer than release b under the
// date-version ordering of release.manifest.schema.json ("^[0-9]{4}\.[0-9]{2}\.[0-9]+$",
// "ordering is lexicographic on the zero-padded components"). The components
// are compared numerically, which agrees with the schema's zero-padded
// lexicographic order and additionally tolerates an unpadded component.
func ReleaseNewer(a, b string) bool {
	if !validRelease(a) {
		return false
	}
	if !validRelease(b) {
		return true
	}
	return compareVersions(a, b) > 0
}

// validRelease reports whether s matches the schema's release pattern.
func validRelease(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	if len(parts[0]) != 4 || len(parts[1]) != 2 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// parseVersion splits a dotted numeric version. It returns ok=false when any
// component is empty or non-numeric.
func parseVersion(s string) ([]int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" {
		return nil, false
	}
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			return nil, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}
