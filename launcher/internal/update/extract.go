package update

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"kobragames.local/launcher/internal/kobraerr"
	"kobragames.local/launcher/internal/paths"
)

// Extract unpacks a .tar.zst archive into a staging root (Updater spec §9.2
// step 10, Launcher spec §19.3 "Before swapping").
//
// # Member routing
//
// A published archive mirrors the game folder itself, because that is what
// Packaging spec §9.1 requires of it:
//
//	game/…        the whole game tree, except game/release.manifest.json
//	launcher/…    only when the launcher changed
//
// The swap then replaces game/ wholesale and merges launcher/ in place, so the
// two prefixes must land in different places. Extract routes them:
//
//	game/x      →  <stagingRoot>/game.new/x    (the "game/" prefix stripped)
//	launcher/x  →  <stagingRoot>/launcher.new/x
//
// Anything else — including a "data/" prefix — is a violation. The staging root
// is the sidecar update directory, not the game folder, so nothing under game/
// or launcher/ is touched until the swap and the launcher merge run.
//
// It trusts nothing the archive says about itself:
//
//   - every entry must be a regular file or a directory; symlinks, hardlinks,
//     devices, FIFOs and unknown types are rejected (FR-UPD-5);
//   - every name must be relative and confined, with no ".." segment, no
//     absolute or drive-qualified path, no Windows device name and no reserved
//     character (FR-UPD-5);
//   - no entry may be, or resolve to, anything under data/ — such an archive is
//     rejected with the update.data.rejected event and the E30 wording
//     (FR-UPD-7);
//   - the actual bytes written are counted against the manifest's declared
//     total_size budget, so a compression bomb stops before it fills the disk
//     (FR-UPD-5);
//   - when the manifest carries a file index, every member must be listed and
//     must match the listed size and SHA-256.
//
// Extract never creates, moves or deletes anything under data/. On failure the
// partially written file is removed; the rest of the staging tree is left for
// §19.4 recovery, which disposes of a stray game.new.
//
// Extract does not re-check the archive's own hash: §19.3 puts Verify first,
// and the caller must run Verify (and the launcher_min gate, GuardApply) before
// extraction. Extract re-enforces every safety rule anyway so that a caller
// that skipped Verify still cannot write an unsafe path or exceed the declared
// budget.
func Extract(archivePath, stagingRoot string, m *Manifest) error {
	if m == nil {
		return kobraerr.IO(msgDamaged, map[string]any{"reason": "no_manifest"}, nil)
	}
	if strings.TrimSpace(stagingRoot) == "" {
		return kobraerr.IO(msgUnsafe, map[string]any{"reason": "bad_destination"}, nil)
	}
	base := filepath.Base(filepath.Clean(stagingRoot))
	if strings.EqualFold(base, DataDirName) {
		// Defence in depth: even a caller mistake must not aim extraction at
		// the user's data directory.
		logError("update.data.rejected", map[string]any{"release": m.Release})
		return kobraerr.IO(msgData, map[string]any{"reason": "data_dir"},
			errors.New("update: extraction destination is a data directory"))
	}
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		return kobraerr.IO(msgUnsafe, map[string]any{"reason": "destination"}, err)
	}

	index := indexFiles(m.Files)
	budget := m.TotalSize
	var written int64
	var count int

	err := walkArchive(archivePath, func(ent archiveEntry, body io.Reader) error {
		target, err := stagedTarget(stagingRoot, ent.Rel)
		if err != nil {
			return err
		}
		if err := paths.Confine(stagingRoot, target); err != nil {
			return &entryViolation{Reason: "path", Entry: ent.Rel, Detail: "entry escapes the staging root"}
		}

		if ent.Kind == kindDir {
			if _, err := io.Copy(io.Discard, body); err != nil {
				return err
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			return nil
		}

		var want *FileEntry
		if len(index) > 0 {
			f, ok := index[ent.Rel]
			if !ok {
				return &entryViolation{Reason: "unlisted_entry", Entry: ent.Rel,
					Detail: "archive member is not in the manifest file index"}
			}
			if f.Size != ent.Header.Size {
				return &entryViolation{Reason: "entry_size", Entry: ent.Rel,
					Detail: fmt.Sprintf("manifest declares %d bytes, archive declares %d", f.Size, ent.Header.Size)}
			}
			want = &f
		}

		// Budget check before writing: nothing is created for an entry that
		// would exceed the declared extracted size.
		if written+ent.Header.Size > budget {
			return &entryViolation{Reason: "size_budget", Entry: ent.Rel,
				Detail: "entry would exceed the manifest's declared total_size"}
		}

		mode := os.FileMode(0o644)
		if want != nil && want.Mode != "" {
			parsed, err := parseMode(want.Mode)
			if err != nil {
				return &entryViolation{Reason: "entry_mode", Entry: ent.Rel, Detail: err.Error()}
			}
			mode = parsed
		} else if ent.Header.FileInfo().Mode().Perm() != 0 {
			mode = ent.Header.FileInfo().Mode().Perm()
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, copyErr := io.CopyN(io.MultiWriter(f, h), body, ent.Header.Size)
		// The staged tree is later promoted with a rename, so the contents have
		// to be durable before that rename: without this fsync a power loss can
		// install zero-length files and still record the update as complete
		// (§15.3 step 6 makes the same point for the single-file writer).
		var syncErr error
		if copyErr == nil {
			syncErr = f.Sync()
		}
		closeErr := f.Close()
		if copyErr != nil || syncErr != nil || closeErr != nil {
			_ = os.Remove(target)
			switch {
			case copyErr != nil:
				return copyErr
			case syncErr != nil:
				return syncErr
			default:
				return closeErr
			}
		}
		if n != ent.Header.Size {
			_ = os.Remove(target)
			return fmt.Errorf("update: truncated entry %q (%d of %d bytes)", ent.Rel, n, ent.Header.Size)
		}
		if want != nil {
			actual := "sha256:" + hex.EncodeToString(h.Sum(nil))
			ok, err := hashMatches(want.Hash, actual)
			if err != nil {
				_ = os.Remove(target)
				return err
			}
			if !ok {
				_ = os.Remove(target)
				return &entryViolation{Reason: "entry_hash", Entry: ent.Rel,
					Detail: "content does not match the manifest hash"}
			}
		}
		written += n
		count++
		return nil
	})
	if err != nil {
		var v *entryViolation
		if errors.As(err, &v) {
			return extractViolation(m, v)
		}
		return kobraerr.IO(msgUnsafe, map[string]any{"reason": "extract"}, err)
	}

	logInfo("update.extract", map[string]any{"release": m.Release, "files": count})
	return nil
}

// stagedTarget maps an archive member onto its staged location, or rejects it
// (spec §9.2 step 10's member routing).
//
// The routing is what makes the swap's two operations consistent: game.new/ is
// renamed over game/, so it must contain the game tree's contents directly,
// while launcher.new/ is a staging copy of the launcher tree that the caller
// merges into the live launcher/ directory. Extracting the archive flat — which
// is what the code did before this routing existed — produced
// game.new/game/index.html, which the swap then installed as
// game/game/index.html: a silently nested, unrunnable release.
func stagedTarget(stagingRoot, rel string) (string, error) {
	slash := strings.IndexByte(rel, '/')
	if slash <= 0 {
		return "", &entryViolation{Reason: "unlisted_entry", Entry: rel,
			Detail: "archive member is not under game/ or launcher/"}
	}
	prefix, rest := rel[:slash], rel[slash+1:]
	if rest == "" {
		return "", &entryViolation{Reason: "path", Entry: rel, Detail: "archive member names a bare prefix"}
	}
	switch prefix {
	case DataDirName:
		return "", &entryViolation{Reason: "data", Entry: rel, Detail: "archive member is under data/"}
	case gameDirName:
		return filepath.Join(stagingRoot, stagingDirName, filepath.FromSlash(rest)), nil
	case launcherDirName:
		return filepath.Join(stagingRoot, launcherStagingDirName, filepath.FromSlash(rest)), nil
	default:
		return "", &entryViolation{Reason: "unlisted_entry", Entry: rel,
			Detail: "archive member is not under game/ or launcher/"}
	}
}

// MergeLauncherInto promotes the staged launcher tree into the live launcher
// directory (Packaging spec §9.3: "launcher/ MUST be replaced in place, not
// swapped away: the running binary must survive its own release").
//
// It copies rather than renames, because the live launcher directory holds the
// running executable and a rename over it would replace the file the process is
// executing from. Per-file replacement is safe on every supported platform for
// that reason: the running image is already mapped.
//
// A staged tree that does not exist means the release shipped no launcher
// change, which is a normal case and not an error. A launcher tree that cannot
// be read is a damaged release.
func MergeLauncherInto(stagingRoot, gameFolder string) error {
	staged := filepath.Join(stagingRoot, launcherStagingDirName)
	if !dirExists(staged) {
		return nil
	}
	live := filepath.Join(gameFolder, launcherDirName)
	if err := os.MkdirAll(live, 0o755); err != nil {
		return kobraerr.IO(msgDamaged, map[string]any{"reason": "launcher_target"}, err)
	}
	err := filepath.WalkDir(staged, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, rerr := filepath.Rel(staged, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(live, rel)
		if err := paths.Confine(live, target); err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFilePreservingMode(path, target)
	})
	if err != nil {
		return kobraerr.IO(msgDamaged, map[string]any{"reason": "launcher_merge"}, err)
	}
	return nil
}

// copyFilePreservingMode copies one regular file, keeping its permission bits
// (the launcher binary and its shell wrappers must stay executable).
func copyFilePreservingMode(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	mode := st.Mode().Perm()
	if mode == 0 {
		mode = 0o644
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	// A plain rename over the destination is atomic on Unix, but some systems
	// refuse to replace an existing file. In that case the destination is moved
	// ASIDE — never removed — so a failed replacement can put the previous file
	// back. The earlier code removed the destination first, which meant a
	// failure here left the live launcher file missing.
	if err := os.Rename(tmpName, dst); err == nil {
		return nil
	} else if _, statErr := os.Stat(dst); statErr != nil {
		// Nothing to move aside: the rename failed for a reason that is not a
		// destination collision, so report it.
		_ = os.Remove(tmpName)
		return err
	}

	aside := dst + ".replaced"
	_ = os.Remove(aside)
	if err := os.Rename(dst, aside); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		if restoreErr := os.Rename(aside, dst); restoreErr != nil {
			// The destination is missing and the previous file is at .replaced:
			// record it, because a human may have to put it back.
			logWarn("update.launcher.restore_failed", map[string]any{"reason": "launcher_replace"})
		}
		_ = os.Remove(tmpName)
		return err
	}
	_ = os.Remove(aside)
	return nil
}

// extractViolation maps a rejected entry to the documented error, logging the
// event that goes with it.
func extractViolation(m *Manifest, v *entryViolation) error {
	switch v.Reason {
	case "data":
		logError("update.data.rejected", map[string]any{"release": m.Release})
		return kobraerr.IO(msgData, map[string]any{"reason": "data_dir"}, v)
	case "size_budget":
		logError("update.verify.fail", map[string]any{"release": m.Release, "reason": "size_budget"})
		return kobraerr.IO(msgUnsafe, map[string]any{"reason": "size_budget"}, v)
	case "unlisted_entry":
		if isReleaseManifestPath(v.Entry) {
			logError("update.manifest.rejected", map[string]any{"release": m.Release})
			return kobraerr.IO(msgEmbeddedManifest,
				map[string]any{"reason": "release_manifest_in_archive"}, v)
		}
		return kobraerr.IO(msgUnsafe, map[string]any{"reason": v.Reason}, v)
	default:
		return kobraerr.IO(msgUnsafe, map[string]any{"reason": v.Reason}, v)
	}
}

// parseMode parses the manifest's POSIX mode string ("644", "0755"). Only the
// permission bits survive; setuid/setgid/sticky are stripped because a release
// archive must never install them.
func parseMode(s string) (os.FileMode, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("update: mode %q is not an octal POSIX mode", s)
	}
	return os.FileMode(n & 0o777), nil
}
