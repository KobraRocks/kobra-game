package storage

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"

	"kobragames.local/launcher/internal/paths"
)

// extractTarZstd unpacks a .tar.zst archive into dest.
//
// Every entry is validated before anything is written: the name must be a
// relative path that stays under dest, the entry must be a regular file or a
// directory, and the total expanded size is bounded by the request ceiling so a
// compression bomb cannot fill the disk (§19.3 step 4, FR-UPD-5).
func extractTarZstd(archive []byte, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	zr, err := zstd.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	var total int64
	const limit = int64(512 << 20) // absolute cap; the caller also bounds the request
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(hdr.Name, "./")))
		if name == "." || name == "" {
			continue
		}
		if filepath.IsAbs(name) || strings.HasPrefix(name, "..") {
			return fmt.Errorf("archive entry escapes the destination")
		}
		// paths.ReservedName is the same predicate the engine and the static
		// server use, so an archive cannot smuggle an entry the rest of the
		// launcher would reject.
		if paths.ReservedName(filepath.Base(name)) {
			return fmt.Errorf("archive entry uses a reserved device name")
		}
		target := filepath.Join(dest, name)
		if err := confineLocal(dest, target); err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += hdr.Size
			if total > limit {
				return fmt.Errorf("archive expands beyond the size limit")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.CopyN(f, tr, hdr.Size); err != nil {
				_ = f.Close()
				_ = os.Remove(target)
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			// Symlinks, hardlinks, devices and FIFOs are rejected outright
			// (§19.3 step 2).
			return fmt.Errorf("archive entry has an unsupported type")
		}
	}
	return nil
}

func confineLocal(root, full string) error {
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("archive entry escapes the destination")
	}
	return nil
}
