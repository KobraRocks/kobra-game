package pack

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// storeOnly are formats that are already compressed; deflating them costs build
// time and saves nothing (§3.5).
var storeOnly = map[string]bool{
	".wasm": true, ".ogg": true, ".mp3": true, ".png": true, ".webp": true,
	".woff2": true, ".zst": true, ".zip": true, ".gz": true,
}

// storeExact are files whose content is derived from the archive's own size.
// README.txt states the ZIP's download size, so compressing it would make the
// size depend on the digits that state it and the fixpoint would oscillate.
// Storing it makes its contribution exactly its length.
var storeExact = map[string]bool{"README.txt": true}

// ArchiveTime derives the single timestamp every archive entry carries (§5.2).
// It comes from the release id, never from the build clock, so two builds of one
// release are byte-identical.
func ArchiveTime(published string) time.Time {
	if ts, err := time.Parse(time.RFC3339, published); err == nil {
		return ts.UTC()
	}
	return time.Unix(0, 0).UTC()
}

// HashFile returns "sha256:<hex>" and the byte length of a file.
func HashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), n, nil
}

// BuildZip writes the user-facing package (§3.1): game/, launcher/, the empty
// data/ skeleton, README.txt and LICENSES/.
//
// Directory entries are emitted explicitly because ZIP does not represent an
// empty directory reliably across extractors, and data/saves/ is empty by
// design (§2.5).
func BuildZip(stage, gameName string, outPath string, published time.Time, log *Logger) error {
	root := filepath.Join(stage, gameName)
	mtime := ArchiveTime(published.Format(time.RFC3339))

	type member struct {
		name string
		path string
	}
	var members []member
	dirs := map[string]bool{}

	top, err := os.ReadDir(root)
	if err != nil {
		return Fail(EvArchiveBuilt, "the staged tree could not be read", map[string]any{"kind": "zip"})
	}
	for _, entry := range top {
		full := filepath.Join(root, entry.Name())
		if entry.IsDir() {
			files, err := RelFiles(full)
			if err != nil {
				return Fail(EvArchiveBuilt, "a staged directory could not be listed", map[string]any{"kind": "zip"})
			}
			for _, rel := range files {
				members = append(members, member{gameName + "/" + entry.Name() + "/" + rel,
					filepath.Join(full, filepath.FromSlash(rel))})
			}
			subdirs, err := RelDirs(full)
			if err != nil {
				return Fail(EvArchiveBuilt, "a staged directory could not be listed", map[string]any{"kind": "zip"})
			}
			for _, rel := range subdirs {
				dirs[gameName+"/"+entry.Name()+"/"+rel] = true
			}
		} else {
			members = append(members, member{gameName + "/" + entry.Name(), full})
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].name < members[j].name })
	for _, m := range members {
		if dir := path.Dir(m.name); dir != "." {
			dirs[dir] = true
		}
	}

	var dirNames []string
	for dir := range dirs {
		dirNames = append(dirNames, dir)
	}
	sort.Strings(dirNames)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, dir := range dirNames {
		hdr := &zip.FileHeader{Name: dir + "/", Method: zip.Store, Modified: mtime}
		hdr.SetMode(os.ModeDir | 0o755)
		hdr.CreatorVersion = 3 << 8 // Unix
		if _, err := zw.CreateHeader(hdr); err != nil {
			return Fail(EvArchiveBuilt, "a directory entry could not be written", map[string]any{"kind": "zip"})
		}
	}
	entryCount := len(dirNames)
	var largestName string
	var largestSize int64
	for _, m := range members {
		mode := os.FileMode(0o644)
		if IsExecutable(m.path) {
			mode = 0o755
		}
		method := zip.Deflate
		base := path.Base(m.name)
		if storeOnly[strings.ToLower(path.Ext(m.name))] || storeExact[base] {
			method = zip.Store
		}
		hdr := &zip.FileHeader{Name: m.name, Method: method, Modified: mtime}
		hdr.SetMode(mode)
		hdr.CreatorVersion = 3 << 8
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return Fail(EvArchiveBuilt, "an archive entry could not be written", map[string]any{"kind": "zip"})
		}
		raw, err := os.ReadFile(m.path)
		if err != nil {
			return Fail(EvArchiveBuilt, "a staged file could not be read", map[string]any{"kind": "zip"})
		}
		if _, err := w.Write(raw); err != nil {
			return Fail(EvArchiveBuilt, "an archive entry could not be written", map[string]any{"kind": "zip"})
		}
		entryCount++
		if int64(len(raw)) > largestSize {
			largestSize = int64(len(raw))
			largestName = m.name
		}
	}
	if err := zw.Close(); err != nil {
		return Fail(EvArchiveBuilt, "the ZIP could not be finalised", map[string]any{"kind": "zip"})
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(outPath, buf.Bytes(), 0o644); err != nil {
		return err
	}
	hash, size, _ := HashFile(outPath)
	log.Info(EvArchiveBuilt, archiveBuiltFields("zip", size, hash,
		entryCount, len(members), largestName, largestSize))
	return nil
}

// BuildUpdateArchive writes the launcher-facing .tar.zst (§9.1): game/ and
// launcher/ only, never data/, README.txt, LICENSES/, or the release manifest.
//
// The manifest is excluded because it cannot index its own hash; see the package
// doc comment and Packaging spec §1.2.
func BuildUpdateArchive(stage, gameName, manifestRel, outPath string,
	published time.Time, log *Logger) error {

	mtime := ArchiveTime(published.Format(time.RFC3339))
	root := filepath.Join(stage, gameName)

	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	entryCount := 0
	var largestName string
	var largestSize int64
	for _, namespace := range []string{"game", "launcher"} {
		base := filepath.Join(root, namespace)
		if st, err := os.Stat(base); err != nil || !st.IsDir() {
			continue
		}
		files, err := RelFiles(base)
		if err != nil {
			return Fail(EvArchiveBuilt, "a namespace could not be listed", map[string]any{"kind": "tar.zst"})
		}
		for _, rel := range files {
			key := namespace + "/" + rel
			if key == manifestRel {
				continue
			}
			full := filepath.Join(base, filepath.FromSlash(rel))
			st, err := os.Stat(full)
			if err != nil {
				return Fail(EvArchiveBuilt, "a staged file could not be read", map[string]any{"kind": "tar.zst"})
			}
			mode := int64(0o644)
			if st.Mode().Perm()&0o111 != 0 {
				mode = 0o755
			}
			// A manual header, not tar.FileInfoHeader: that helper copies
			// ownership and timestamps from the filesystem, which would make the
			// archive depend on the build machine (§5.2).
			hdr := &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     key,
				Size:     st.Size(),
				Mode:     mode,
				ModTime:  mtime,
				Format:   tar.FormatPAX,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return Fail(EvArchiveBuilt, "a tar header could not be written", map[string]any{"kind": "tar.zst"})
			}
			f, err := os.Open(full)
			if err != nil {
				return Fail(EvArchiveBuilt, "a staged file could not be read", map[string]any{"kind": "tar.zst"})
			}
			if _, err := io.Copy(tw, f); err != nil {
				f.Close()
				return Fail(EvArchiveBuilt, "a tar entry could not be written", map[string]any{"kind": "tar.zst"})
			}
			f.Close()
			entryCount++
			if st.Size() > largestSize {
				largestSize = st.Size()
				largestName = key
			}
		}
	}
	if err := tw.Close(); err != nil {
		return Fail(EvArchiveBuilt, "the tar could not be finalised", map[string]any{"kind": "tar.zst"})
	}

	encoder, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedBestCompression),
		// One goroutine keeps the output independent of the build machine's CPU
		// count, which matters for §5.2's byte-identical rebuild.
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		return Fail(EvArchiveBuilt, "the zstd encoder could not be created", map[string]any{"kind": "tar.zst"})
	}
	defer encoder.Close()
	compressed := encoder.EncodeAll(raw.Bytes(), nil)

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(outPath, compressed, 0o644); err != nil {
		return err
	}
	hash, size, _ := HashFile(outPath)
	log.Info(EvArchiveBuilt, archiveBuiltFields("tar.zst", size, hash,
		entryCount, entryCount, largestName, largestSize))
	return nil
}

// archiveBuiltFields assembles the pack.archive.built event. §11.2 requires the
// package size, the largest single file and the total entry count; §5.2 requires
// the toolchain versions, so a hash can be reproduced or its irreproducibility
// explained.
func archiveBuiltFields(kind string, size int64, hash string,
	entries, files int, largest string, largestBytes int64) map[string]any {

	fields := map[string]any{
		"kind": kind, "bytes": size, "sha256": hash,
		"entries": entries, "files": files,
		"largest": largest, "largest_bytes": largestBytes,
		// The tar, ZIP and zstd readers/writers used here come from the Go
		// standard library and the vendored module, so these two versions name
		// the whole compression stack.
		"go_version": runtime.Version(),
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/klauspost/compress" && dep.Version != "" {
				fields["zstd_version"] = dep.Version
				break
			}
		}
	}
	return fields
}

// WriteSha256Sums writes SHA256SUMS in the conventional format, so
// `sha256sum -c SHA256SUMS` works on Linux and `shasum -a 256 -c` on macOS
// (§7.1).
func WriteSha256Sums(outDir string, names []string, log *Logger) error {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	var b strings.Builder
	for _, name := range sorted {
		hash, _, err := HashFile(filepath.Join(outDir, name))
		if err != nil {
			return Fail(EvHash, "a published artefact could not be hashed", map[string]any{"file": name})
		}
		b.WriteString(strings.TrimPrefix(hash, "sha256:"))
		b.WriteString("  ")
		b.WriteString(name)
		b.WriteString("\n")
		log.Info(EvHash, map[string]any{"file": name, "sha256": hash})
	}
	return os.WriteFile(filepath.Join(outDir, "SHA256SUMS"), []byte(b.String()), 0o644)
}

// Latest is the discovery pointer (§8.4). It is the last thing published and the
// rollback lever: reverting it withdraws an update without deleting artefacts.
type Latest struct {
	Release     string `json:"release"`
	ManifestURL string `json:"manifest_url"`
	NotesURL    string `json:"notes_url,omitempty"`
}

// WriteLatest writes latest.json for the newest built release.
func WriteLatest(outDir string, latest Latest) error {
	return WriteJSON(filepath.Join(outDir, "latest.json"), latest)
}

// VerifyOutput re-reads every published artefact and re-checks it (§13.2), then
// checks that each manifest names its archive correctly (§8.2.3.1) and that the
// archive really carries what the index claims (§6.4, §11.2).
//
// This is the last gate before a release is publishable, and it is the one that
// catches "the file is right and the manifest is wrong" — the failure mode that
// makes a release unappliable without being corrupt.
func VerifyOutput(outDir string, names []string, log *Logger) error {
	raw, err := os.ReadFile(filepath.Join(outDir, "SHA256SUMS"))
	if err != nil {
		return Fail(EvVerifyFail, "SHA256SUMS is missing", map[string]any{"reason": "no_sums"})
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		expected, name, ok := strings.Cut(line, "  ")
		if !ok {
			return Fail(EvVerifyFail, "SHA256SUMS is malformed", map[string]any{"reason": "malformed"})
		}
		hash, _, err := HashFile(filepath.Join(outDir, name))
		if err != nil {
			return Fail(EvVerifyFail, "a published artefact is missing", map[string]any{"file": name})
		}
		actual := strings.TrimPrefix(hash, "sha256:")
		if actual != expected {
			log.Error(EvVerifyFail, map[string]any{"file": name, "expected": expected, "actual": actual})
			return Fail(EvVerifyFail, "a published artefact does not match SHA256SUMS",
				map[string]any{"file": name})
		}
	}

	// §6.1/§8.4: an incomplete dist must not pass. Every published archive needs
	// its release manifest, and latest.json must resolve to one of them.
	if err := verifyCompleteness(outDir, names, log); err != nil {
		return err
	}

	for _, name := range names {
		if !strings.HasSuffix(name, ".release.manifest.json") {
			continue
		}
		var manifest ReleaseManifest
		if err := ReadJSON(filepath.Join(outDir, name), &manifest); err != nil {
			return Fail(EvVerifyFail, "a published manifest could not be read", map[string]any{"file": name})
		}
		if manifest.Archive == nil || manifest.Archive.Hash == "" || manifest.Archive.Size == 0 {
			log.Error(EvVerifyFail, map[string]any{"file": name, "reason": "archive_hash_missing"})
			return Fail(EvVerifyFail, "a manifest does not declare the archive it describes (§8.2.2)",
				map[string]any{"file": name})
		}
		target := filepath.Join(outDir, manifest.Archive.Name)
		hash, size, err := HashFile(target)
		if err != nil {
			return Fail(EvVerifyFail, "a manifest names an archive that was not published",
				map[string]any{"file": name, "archive": manifest.Archive.Name})
		}
		if hash != manifest.Archive.Hash {
			log.Error(EvVerifyFail, map[string]any{
				"file": name, "expected": manifest.Archive.Hash, "actual": hash,
			})
			return Fail(EvVerifyFail, "a manifest's archive.hash does not match the published archive",
				map[string]any{"file": name})
		}
		if size != manifest.Archive.Size {
			log.Error(EvVerifyFail, map[string]any{
				"file": name, "expected": manifest.Archive.Size, "actual": size,
			})
			return Fail(EvVerifyFail, "a manifest's archive.size does not match the published archive",
				map[string]any{"file": name})
		}

		// The index is only a claim until it is checked against the bytes that
		// ship. Stream the update archive and the distribution ZIP and compare
		// every indexed path.
		if err := verifyManifestIndex(outDir, &manifest, log); err != nil {
			return err
		}
	}
	log.Info(EvVerifyOK, map[string]any{"files": len(names)})
	return nil
}

// publishedArchiveRe matches the archive names the build publishes:
// <slug>-<release>-<platform>.tar.zst and .zip. The slug and the platform may
// both contain hyphens, so the calver release id is the anchor.
var publishedArchiveRe = regexp.MustCompile(
	`^(.*)-([0-9]{4}\.[0-9]{2}\.[0-9]+)-([^-].*)\.(tar\.zst|zip)$`)

// manifestNameForArchive derives the published release manifest name for an
// archive name. ok is false when the name does not follow the convention.
func manifestNameForArchive(name string) (string, bool) {
	m := publishedArchiveRe.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return m[1] + "-" + m[2] + ".release.manifest.json", true
}

// verifyCompleteness rejects a dist that is missing an artefact the launcher
// or a publisher needs: an archive without its manifest, or a latest.json that
// points at a manifest or archive that is absent or does not match it (§8.4).
func verifyCompleteness(outDir string, names []string, log *Logger) error {
	present := make(map[string]bool, len(names))
	for _, name := range names {
		present[name] = true
	}
	for _, name := range names {
		if !strings.HasSuffix(name, ".zip") && !strings.HasSuffix(name, ".tar.zst") {
			continue
		}
		manifestName, ok := manifestNameForArchive(name)
		if !ok {
			log.Error(EvVerifyFail, map[string]any{"file": name, "reason": "archive_name"})
			return Fail(EvVerifyFail,
				"a published archive does not follow the <slug>-<release>-<platform> naming rule",
				map[string]any{"file": name})
		}
		if !present[manifestName] {
			log.Error(EvVerifyFail, map[string]any{"file": name, "manifest": manifestName, "reason": "no_manifest"})
			return Fail(EvVerifyFail, "a published archive has no release manifest (§6.1)",
				map[string]any{"file": name, "manifest": manifestName})
		}
		if _, err := os.Stat(filepath.Join(outDir, manifestName)); err != nil {
			return Fail(EvVerifyFail, "a published archive's release manifest is missing",
				map[string]any{"file": name, "manifest": manifestName})
		}
	}

	latestPath := filepath.Join(outDir, "latest.json")
	if _, err := os.Stat(latestPath); err != nil {
		return Fail(EvVerifyFail, "latest.json is missing; the dist is incomplete",
			map[string]any{"reason": "no_latest"})
	}
	var latest Latest
	if err := ReadJSON(latestPath, &latest); err != nil {
		return Fail(EvVerifyFail, "latest.json is malformed", map[string]any{"reason": "malformed"})
	}
	if latest.Release == "" || latest.ManifestURL == "" {
		return Fail(EvVerifyFail, "latest.json names no release manifest (§8.4)",
			map[string]any{"reason": "no_manifest_url"})
	}
	manifestPath := filepath.Join(outDir, filepath.FromSlash(latest.ManifestURL))
	var manifest ReleaseManifest
	if err := ReadJSON(manifestPath, &manifest); err != nil {
		log.Error(EvVerifyFail, map[string]any{
			"file": "latest.json", "manifest": latest.ManifestURL, "reason": "dangling",
		})
		return Fail(EvVerifyFail, "latest.json names a release manifest that was not published",
			map[string]any{"manifest": latest.ManifestURL})
	}
	if manifest.Release != latest.Release {
		log.Error(EvVerifyFail, map[string]any{
			"file": "latest.json", "expected": latest.Release, "actual": manifest.Release,
		})
		return Fail(EvVerifyFail, "latest.json and the manifest it names declare different releases",
			map[string]any{"manifest": latest.ManifestURL})
	}
	if manifest.Archive == nil || manifest.Archive.Hash == "" {
		return Fail(EvVerifyFail, "latest.json dereferences a manifest with no archive (§8.2.2)",
			map[string]any{"manifest": latest.ManifestURL})
	}
	hash, size, err := HashFile(filepath.Join(outDir, manifest.Archive.Name))
	if err != nil {
		log.Error(EvVerifyFail, map[string]any{
			"file": "latest.json", "archive": manifest.Archive.Name, "reason": "dangling",
		})
		return Fail(EvVerifyFail, "latest.json dereferences a missing archive",
			map[string]any{"archive": manifest.Archive.Name})
	}
	if hash != manifest.Archive.Hash || size != manifest.Archive.Size {
		log.Error(EvVerifyFail, map[string]any{
			"file": "latest.json", "archive": manifest.Archive.Name, "reason": "archive_mismatch",
		})
		return Fail(EvVerifyFail, "latest.json dereferences a hash-mismatched archive",
			map[string]any{"archive": manifest.Archive.Name})
	}
	return nil
}

// verifyManifestIndex checks a release manifest's files[] index against the
// archived bytes: the update archive it names, and the distribution ZIP that
// ships beside it when one is present.
func verifyManifestIndex(outDir string, manifest *ReleaseManifest, log *Logger) error {
	archiveName := manifest.Archive.Name
	if !strings.HasSuffix(archiveName, ".tar.zst") {
		return Fail(EvVerifyFail, "the manifest names an archive format that cannot be verified",
			map[string]any{"archive": archiveName})
	}
	if err := verifyTarIndex(filepath.Join(outDir, archiveName), manifest, log); err != nil {
		return err
	}
	zipName := strings.TrimSuffix(archiveName, ".tar.zst") + ".zip"
	if _, err := os.Stat(filepath.Join(outDir, zipName)); err == nil {
		if err := verifyZipIndex(filepath.Join(outDir, zipName), manifest, log); err != nil {
			return err
		}
	}
	return nil
}

// indexOf turns a manifest's file index into a lookup table.
func indexOf(manifest *ReleaseManifest) map[string]FileEntry {
	index := make(map[string]FileEntry, len(manifest.Files))
	for _, entry := range manifest.Files {
		index[entry.Path] = entry
	}
	return index
}

// verifyTarIndex streams the update archive and compares every member with the
// manifest index, in both directions: an unindexed member and a missing indexed
// path are both release-blocking (Launcher spec §19.3, §6.4).
func verifyTarIndex(archivePath string, manifest *ReleaseManifest, log *Logger) error {
	archive := filepath.Base(archivePath)
	index := indexOf(manifest)

	f, err := os.Open(archivePath)
	if err != nil {
		return Fail(EvVerifyFail, "the update archive could not be opened", map[string]any{"archive": archive})
	}
	defer f.Close()
	decoder, err := zstd.NewReader(f)
	if err != nil {
		return Fail(EvVerifyFail, "the update archive could not be decompressed", map[string]any{"archive": archive})
	}
	defer decoder.Close()

	seen := make(map[string]bool, len(index))
	reader := tar.NewReader(decoder)
	for {
		hdr, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Fail(EvVerifyFail, "the update archive could not be read", map[string]any{"archive": archive})
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		entry, ok := index[hdr.Name]
		if !ok {
			log.Error(EvVerifyFail, map[string]any{
				"archive": archive, "path": hdr.Name, "reason": "unindexed_member",
			})
			return Fail(EvVerifyFail, "an update archive member is not in the release manifest index",
				map[string]any{"path": hdr.Name})
		}
		if hdr.Size != entry.Size {
			log.Error(EvVerifyFail, map[string]any{
				"archive": archive, "path": hdr.Name, "expected": entry.Size, "actual": hdr.Size,
			})
			return Fail(EvVerifyFail, "an update archive member's size does not match the release manifest index",
				map[string]any{"path": hdr.Name})
		}
		hash := sha256.New()
		if _, err := io.Copy(hash, reader); err != nil {
			return Fail(EvVerifyFail, "an update archive member could not be read",
				map[string]any{"archive": archive, "path": hdr.Name})
		}
		actual := "sha256:" + hex.EncodeToString(hash.Sum(nil))
		if actual != entry.Hash {
			log.Error(EvVerifyFail, map[string]any{
				"archive": archive, "path": hdr.Name, "expected": entry.Hash, "actual": actual,
			})
			return Fail(EvVerifyFail, "an update archive member does not match the release manifest index",
				map[string]any{"path": hdr.Name})
		}
		seen[hdr.Name] = true
	}
	for _, entry := range manifest.Files {
		if !seen[entry.Path] {
			log.Error(EvVerifyFail, map[string]any{
				"archive": archive, "path": entry.Path, "reason": "missing_member",
			})
			return Fail(EvVerifyFail, "the release manifest indexes a file that is not in the update archive",
				map[string]any{"path": entry.Path})
		}
	}
	return nil
}

// verifyZipIndex checks every indexed path against the bytes in the distribution
// ZIP. Members the index deliberately does not cover — README.txt, LICENSES/,
// data/.keep and the manifest itself (§9.1) — are ignored, but an indexed path
// that is absent from the ZIP, or whose bytes differ, is a failure.
func verifyZipIndex(zipPath string, manifest *ReleaseManifest, log *Logger) error {
	file := filepath.Base(zipPath)
	index := indexOf(manifest)

	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return Fail(EvVerifyFail, "the distribution ZIP could not be opened", map[string]any{"file": file})
	}
	defer reader.Close()

	seen := make(map[string]bool, len(index))
	for _, f := range reader.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rel, ok := stripTopDir(f.Name)
		if !ok {
			continue
		}
		entry, ok := index[rel]
		if !ok {
			continue
		}
		if f.UncompressedSize64 != uint64(entry.Size) {
			log.Error(EvVerifyFail, map[string]any{
				"file": file, "path": rel, "expected": entry.Size, "actual": f.UncompressedSize64,
			})
			return Fail(EvVerifyFail, "a distribution ZIP member's size does not match the release manifest index",
				map[string]any{"path": rel})
		}
		rc, err := f.Open()
		if err != nil {
			return Fail(EvVerifyFail, "a distribution ZIP member could not be read",
				map[string]any{"file": file, "path": rel})
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, rc)
		rc.Close()
		if copyErr != nil {
			return Fail(EvVerifyFail, "a distribution ZIP member could not be read",
				map[string]any{"file": file, "path": rel})
		}
		actual := "sha256:" + hex.EncodeToString(hash.Sum(nil))
		if actual != entry.Hash {
			log.Error(EvVerifyFail, map[string]any{
				"file": file, "path": rel, "expected": entry.Hash, "actual": actual,
			})
			return Fail(EvVerifyFail, "a distribution ZIP member does not match the release manifest index",
				map[string]any{"path": rel})
		}
		seen[rel] = true
	}
	for _, entry := range manifest.Files {
		if !seen[entry.Path] {
			log.Error(EvVerifyFail, map[string]any{
				"file": file, "path": entry.Path, "reason": "missing_member",
			})
			return Fail(EvVerifyFail, "the distribution ZIP is missing a file the release manifest indexes",
				map[string]any{"path": entry.Path})
		}
	}
	return nil
}

// stripTopDir removes the single top-level game folder from a ZIP member name.
// It returns ok=false for a name with no second segment.
func stripTopDir(name string) (string, bool) {
	clean := path.Clean(strings.TrimPrefix(filepath.ToSlash(name), "/"))
	parts := strings.SplitN(clean, "/", 2)
	if len(parts) != 2 || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

// ExtractZip unpacks a distribution ZIP into a clean directory and returns the
// single top-level game folder it must contain.
func ExtractZip(zipPath, dest string) (string, error) {
	if err := os.RemoveAll(dest); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", err
	}
	defer r.Close()
	for _, f := range r.File {
		target := filepath.Join(dest, filepath.FromSlash(f.Name))
		if err := CheckZipEntry(dest, target, f.Name); err != nil {
			return "", err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		mode := f.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
		if err != nil {
			rc.Close()
			return "", err
		}
		if _, err := io.Copy(out, rc); err != nil {
			out.Close()
			rc.Close()
			return "", err
		}
		out.Close()
		rc.Close()
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		return "", err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) != 1 {
		return "", fmt.Errorf("the ZIP must hold exactly one top-level directory, found %d", len(dirs))
	}
	return filepath.Join(dest, dirs[0]), nil
}

// CheckZipEntry refuses a zip-slip entry before anything is written (FR-UPD-5).
func CheckZipEntry(destRoot, target, name string) error {
	clean := path.Clean(strings.TrimPrefix(filepath.ToSlash(name), "/"))
	if clean == "." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return fmt.Errorf("archive entry escapes the destination: %q", name)
	}
	rel, err := filepath.Rel(destRoot, target)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("archive entry escapes the destination: %q", name)
	}
	return nil
}

// zipDataStrays returns every file entry under the package's top-level data/
// directory other than data/.keep.
//
// The check is scoped to <GameName>/data/, because game/assets/data/ is ordinary
// shipped content and must not be confused with the user's data directory.
func zipDataStrays(zipPath string) ([]string, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	var out []string
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := path.Clean(strings.TrimPrefix(filepath.ToSlash(f.Name), "/"))
		parts := strings.Split(name, "/")
		if len(parts) < 3 || parts[1] != "data" {
			continue
		}
		if len(parts) == 3 && parts[2] == ".keep" {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// ReadEngineManifest loads game/engine/engine.manifest.json from a staged tree.
func ReadEngineManifest(stage, gameName string) (*EngineManifest, error) {
	var doc EngineManifest
	err := ReadJSON(filepath.Join(stage, gameName, "game", "engine", "engine.manifest.json"), &doc)
	return &doc, err
}
