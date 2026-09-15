package update

// Cross-filesystem promotion.
//
// The staged release lives under the sidecar — machine-local state, i.e.
// XDG_STATE_HOME or %LOCALAPPDATA% — while the game folder is wherever the user
// installed it. On a split layout (game on D: and state on C:, or a game on a
// data disk and the state directory in a tmpfs) the promotion rename is
// cross-device and os.Rename fails with EXDEV. Without a fallback every update
// on such an install fails after a full download and extraction.
//
// The sidecar and storage packages already handle EXDEV for single files
// (sidecar.WriteAtomic, storage.renameCrossDevice). This is the directory
// equivalent, and unlike a plain copy it keeps the promotion atomic: the bytes
// are copied to a temporary sibling of the destination, which is on the
// destination's filesystem, and only then renamed into place.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"kobragames.local/launcher/internal/faultinject"
)

// promoteDir moves src to dst, which must not already exist.
//
// Same filesystem: a single os.Rename. Cross filesystem: src is copied to a
// temporary sibling of dst and that copy is renamed into place, so the promotion
// is still one atomic step; src is removed last. The returned flag reports that
// the degraded path was used, which the caller logs and records in diagnostics.
func promoteDir(src, dst string) (degraded bool, err error) {
	// §26.4: injected BEFORE the real rename, so the source is still there for
	// the copy path to read. A failure injected afterwards would leave nothing to
	// copy and the fallback would fail for the wrong reason.
	renameErr := faultinject.Err(faultinject.UpdatePromoteRename)
	if renameErr == nil {
		renameErr = os.Rename(src, dst)
	}
	if renameErr == nil {
		return false, nil
	} else if !isCrossDevice(renameErr) {
		return false, renameErr
	}

	parent := filepath.Dir(dst)
	tmp, err := os.MkdirTemp(parent, promoteTempPrefix(dst))
	if err != nil {
		return true, err
	}
	if err := copyTree(src, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return true, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.RemoveAll(tmp)
		return true, err
	}
	if err := os.RemoveAll(src); err != nil {
		// The release is installed; a leftover staging tree is debris that
		// Recover removes, so this must not be reported as a failed update.
		logWarn("update.swap.promote.debris", map[string]any{"reason": "staging_remove"})
	}
	return true, nil
}

// copyTree copies the tree at src into the existing directory dst, preserving
// permission bits and syncing every file. The staged tree contains only
// directories and regular files — the extractor refuses symlinks, devices and
// hardlinks — so any other entry means a damaged staging tree and fails closed.
func copyTree(src, dst string) error {
	rootInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() {
		return fmt.Errorf("update: staging root is not a directory")
	}
	err = filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			mode := os.FileMode(0o755)
			if info, ierr := d.Info(); ierr == nil {
				mode = info.Mode().Perm()
			}
			return os.MkdirAll(target, mode)
		case d.Type().IsRegular():
			info, ierr := d.Info()
			if ierr != nil {
				return ierr
			}
			return copyFileSynced(path, target, info.Mode().Perm())
		default:
			return fmt.Errorf("update: staged entry is not a regular file")
		}
	})
	if err != nil {
		return err
	}
	// MkdirTemp made dst 0700; the promoted directory must keep the mode the
	// staged root had, exactly as a rename would have preserved it.
	return os.Chmod(dst, rootInfo.Mode().Perm())
}

// copyFileSynced copies one regular file, preserving its mode, and fsyncs it
// before closing so the contents are durable before the tree is promoted.
func copyFileSynced(src, dst string, mode os.FileMode) error {
	if mode == 0 {
		mode = 0o644
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

// promoteTempPrefix is the name pattern promoteDir uses for its staging copy
// beside dst, so the leftover name and the cleanup glob can never disagree.
// Recover removes leftovers: a crash between the copy and the rename leaves one
// behind, and it is never a source of truth for recovery.
func promoteTempPrefix(dst string) string { return "." + filepath.Base(dst) + ".promote-" }

// removePromoteDebris deletes any interrupted cross-device promotion copy
// beside the game folder. Recovery always decides from game/, game.old/ and
// game.new/, never from a promote copy, so this is safe to do unconditionally.
func removePromoteDebris(gameFolder string) {
	pattern := promoteTempPrefix(filepath.Join(gameFolder, gameDirName)) + "*"
	matches, err := filepath.Glob(filepath.Join(gameFolder, pattern))
	if err != nil {
		return
	}
	for _, m := range matches {
		if err := os.RemoveAll(m); err != nil {
			logWarn("update.swap.promote.debris", map[string]any{"reason": "cleanup_failed"})
		}
	}
}
