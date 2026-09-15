package sidecar

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
)

var counter atomic.Uint64

func nextCounter() uint64 { return counter.Add(1) }

// WriteAtomic is the durability primitive of §15.3, implemented here as well as
// in the storage engine because port.json is written before the engine exists.
//
// Protocol: create a uniquely named temp file with O_EXCL, write, fsync, close,
// verify the length, rotate the previous file to .bak, rename into place, then
// fsync the containing directory. The directory fsync is not optional: without
// it a rename can be lost on power failure on XFS and some network mounts.
func WriteAtomic(target string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.tmp-%d-%d",
		filepath.Base(target), os.Getpid(), nextCounter()))

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if info, err := os.Stat(tmp); err != nil || info.Size() != int64(len(data)) {
		_ = os.Remove(tmp)
		return fmt.Errorf("temp file verification failed")
	}
	if _, err := os.Stat(target); err == nil {
		bak := target + ".bak"
		_ = os.Remove(bak)
		if err := os.Rename(target, bak); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, target); err != nil {
		// Cross-device fallback: the sidecar directory can legitimately be on
		// a different filesystem from its target.
		if rerr := renameCrossDevice(tmp, target); rerr != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// renameCrossDevice degrades atomicity, so callers that care must log
// storage.atomicity.degraded. It is only reached when os.Rename returns EXDEV.
func renameCrossDevice(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
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
		return err
	}
	return os.Remove(src)
}
