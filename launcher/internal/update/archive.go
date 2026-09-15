package update

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// entryViolation is an internal error describing why one archive entry was
// rejected. Its Reason distinguishes the cases the public API must treat
// differently: a data/ entry is E30 and aborts the update with
// update.data.rejected, while every other violation means the archive itself
// is unsafe to install. The entry name lives in the error text, which is only
// ever used as a Cause (§22.4).
type entryViolation struct {
	Reason string // "path", "entry_type", "device", "data"
	Entry  string
	Detail string
}

func (e *entryViolation) Error() string {
	return fmt.Sprintf("update: rejected archive entry (%s): %q", e.Reason, e.Entry)
}

// entryKind classifies an archive entry after validation.
type entryKind int

const (
	kindRegular entryKind = iota
	kindDir
)

// archiveEntry is one validated tar member.
type archiveEntry struct {
	Header *tar.Header
	Rel    string // cleaned, slash-separated path relative to the destination
	Kind   entryKind
}

// reservedNames are the Windows device names that must never appear as a path
// component (FR-UPD-5). Matching ignores case and the stem's extension, so
// "CON" and "CON.txt" are both rejected, as Windows treats them identically.
var reservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// isReservedComponent reports whether one path component names a Windows
// device, with or without an extension.
func isReservedComponent(seg string) bool {
	seg = strings.TrimRight(seg, ". ")
	if seg == "" {
		return false
	}
	stem := seg
	if i := strings.IndexByte(seg, '.'); i >= 0 {
		stem = seg[:i]
	}
	return reservedNames[strings.ToUpper(stem)]
}

// checkEntry validates one tar header against the FR-UPD-5 rules that can be
// decided from the header alone: the entry must be a regular file or a
// directory, its name must be relative, must not contain "." or ".." segments,
// must not be absolute, must not use a Windows device name or reserved
// character, and must never begin with data/ (FR-UPD-7, case-insensitively,
// because Windows and default macOS filesystems fold case).
//
// A header that names the archive root itself (".", "./") returns an empty Rel
// and a nil error; callers skip it.
func checkEntry(hdr *tar.Header) (archiveEntry, error) {
	ent := archiveEntry{Header: hdr}

	switch hdr.Typeflag {
	case tar.TypeReg, tar.TypeRegA:
		ent.Kind = kindRegular
	case tar.TypeDir:
		ent.Kind = kindDir
	default:
		return ent, &entryViolation{
			Reason: "entry_type",
			Entry:  hdr.Name,
			Detail: fmt.Sprintf("type %q is not a regular file or directory", hdr.Typeflag),
		}
	}

	name := hdr.Name
	if name == "" {
		return ent, &entryViolation{Reason: "path", Entry: name, Detail: "empty name"}
	}
	if strings.ContainsRune(name, 0) {
		return ent, &entryViolation{Reason: "path", Entry: name, Detail: "name contains NUL"}
	}
	// A backslash is a separator on Windows but never legal in a release path
	// (relPath in the schemas forbids it), so it is rejected rather than
	// normalised: normalising would let "..\\..\\x" become an escape.
	if strings.Contains(name, `\`) {
		return ent, &entryViolation{Reason: "path", Entry: name, Detail: "name contains a backslash"}
	}
	if strings.HasPrefix(name, "/") || filepath.IsAbs(name) {
		return ent, &entryViolation{Reason: "path", Entry: name, Detail: "absolute name"}
	}
	if len(name) >= 2 && name[1] == ':' {
		return ent, &entryViolation{Reason: "path", Entry: name, Detail: "drive-qualified name"}
	}

	clean := path.Clean(name)
	if clean == "." || clean == "/" {
		ent.Rel = ""
		return ent, nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return ent, &entryViolation{Reason: "path", Entry: name, Detail: "name escapes the destination"}
	}

	segments := strings.Split(clean, "/")
	for _, seg := range segments {
		if seg == "" || seg == "." || seg == ".." {
			return ent, &entryViolation{Reason: "path", Entry: name, Detail: "empty or dot segment"}
		}
		if isReservedComponent(seg) {
			return ent, &entryViolation{Reason: "device", Entry: name, Detail: "reserved device name"}
		}
		if strings.ContainsAny(seg, `<>:"|?*`) {
			return ent, &entryViolation{Reason: "path", Entry: name, Detail: "reserved character in name"}
		}
		if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
			return ent, &entryViolation{Reason: "path", Entry: name, Detail: "name ends in a dot or space"}
		}
	}

	// FR-UPD-7: a release never contains and never writes data/.
	if strings.EqualFold(segments[0], DataDirName) {
		return ent, &entryViolation{Reason: "data", Entry: name, Detail: "entry is under the data directory"}
	}

	ent.Rel = clean
	return ent, nil
}

// walkArchive streams every member of a .tar.zst archive through fn without
// trusting any archive-declared path or size. The body reader is bounded to
// the header's declared size by archive/tar; fn must consume it (io.CopyN) so
// a lying header surfaces as an unexpected-EOF error rather than silent
// truncation.
func walkArchive(archivePath string, fn func(ent archiveEntry, body io.Reader) error) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	zr, err := zstd.NewReader(f)
	if err != nil {
		return err
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		ent, verr := checkEntry(hdr)
		if verr != nil {
			return verr
		}
		if ent.Rel == "" {
			continue
		}
		limited := io.LimitReader(tr, hdr.Size)
		if err := fn(ent, limited); err != nil {
			return err
		}
	}
}
