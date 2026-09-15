// Package storage is the launcher's only writer of game data (Launcher spec
// §15-§18).
//
// It owns the atomic write protocol (§15.3), the per-data-directory write lock
// (§15.6, §17.1), the revision store (§18), the write probe (§15.7), the .bak
// rotation and trash (§15.5), and path confinement (§16.4).
//
// There is deliberately no WriteFile(path, data) method. Every write names a
// kind of data (save, config, mods manifest) and the engine maps that kind to a
// validated directory, so a caller cannot invent a path.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"kobragames.local/launcher/internal/diagnostics"
	"kobragames.local/launcher/internal/faultinject"
	"kobragames.local/launcher/internal/kobraerr"
	"kobragames.local/launcher/internal/paths"
)

// Save file schema identifier (save.schema.json).
const SchemaSave = "kobra.save/1"

// identifierRe is the §16.1 identifier rule, shared with data-api.schema.json's
// $defs/identifier. It lives here rather than in a per-OS file so there is
// exactly one definition on every platform.
var identifierRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Options configures a new Engine.
type Options struct {
	DataDir            string
	DataDirKind        string // "game" or "override"
	KeepRevisions      int    // §15.5 cap on <slot>.json.<rev> sidecars
	TrashRetentionDays int    // §15.5; 0 keeps trash indefinitely
	MaxRequestBytes    int64  // used for the §17.5 disk-space guard
	GameID             string
	Release            string
	// EngineVersion and SaveVersion are written into every save header. They
	// are launcher configuration, not storage policy.
	EngineVersion string
	SaveVersion   int
	Logger        *diagnostics.Logger
}

// Engine is the data-access layer.
type Engine struct {
	dataDir       string
	dataDirKind   string
	keepRevisions int
	trashDays     int
	maxRequest    int64
	gameID        string
	release       string
	engineVer     string
	saveVer       int
	log           *diagnostics.Logger

	// writeMu is the §15.6 global write lock: writes take it exclusively,
	// reads take it shared. It is a conservative superset of the per-slot
	// concurrency FR-SAVE-6 permits.
	writeMu sync.RWMutex

	revMu     sync.Mutex
	revisions map[string]int64

	writerMu sync.Mutex
	writer   string

	probeMu     sync.Mutex
	writable    bool
	probeReason string

	degradedMu sync.Mutex
	degraded   bool
}

// New creates an engine and initialises the directory scaffolding.
func New(opts Options) (*Engine, error) {
	if opts.DataDir == "" {
		return nil, errors.New("storage: data directory is required")
	}
	if opts.KeepRevisions < 0 {
		opts.KeepRevisions = 0
	}
	e := &Engine{
		dataDir:       opts.DataDir,
		dataDirKind:   opts.DataDirKind,
		keepRevisions: opts.KeepRevisions,
		trashDays:     opts.TrashRetentionDays,
		maxRequest:    opts.MaxRequestBytes,
		gameID:        opts.GameID,
		release:       opts.Release,
		engineVer:     opts.EngineVersion,
		saveVer:       opts.SaveVersion,
		log:           opts.Logger,
		revisions:     map[string]int64{},
	}
	if e.dataDirKind == "" {
		e.dataDirKind = "game"
	}
	if e.engineVer == "" {
		e.engineVer = "0.0.0"
	}
	if e.saveVer < 1 {
		e.saveVer = 1
	}
	return e, nil
}

// DataDirKind reports "game" or "override" for /api/state (§14.5).
func (e *Engine) DataDirKind() string { return e.dataDirKind }

// DataWritable reports the §15.7 probe result.
func (e *Engine) DataWritable() bool {
	e.probeMu.Lock()
	defer e.probeMu.Unlock()
	return e.writable
}

// DataWritableReason returns the probe's machine-readable failure reason, or ""
// when the probe passed.
func (e *Engine) DataWritableReason() string {
	e.probeMu.Lock()
	defer e.probeMu.Unlock()
	return e.probeReason
}

// AtomicityDegraded reports whether any write had to use the cross-device
// fallback of §15.4, which degrades atomicity.
func (e *Engine) AtomicityDegraded() bool {
	e.degradedMu.Lock()
	defer e.degradedMu.Unlock()
	return e.degraded
}

func (e *Engine) markDegraded(path string) {
	e.degradedMu.Lock()
	e.degraded = true
	e.degradedMu.Unlock()
	e.logf(diagnostics.LevelWarn, "storage.atomicity.degraded", map[string]any{
		"reason": "EXDEV",
	})
}

// logf logs when a logger is configured; a nil logger is a supported mode for
// tests and for the --diagnostics fast path.
func (e *Engine) logf(lv diagnostics.Level, evt string, fields map[string]any) {
	if e.log == nil {
		return
	}
	e.log.Event(lv, evt, fields)
}

// --- Write probe (§15.7) --------------------------------------------------

// ProbeWrite runs the five-step probe against DataDir and records the result.
// It returns the probe error, if any; DataWritable reflects the outcome either
// way, because a failed probe is a degraded mode rather than a startup failure
// (FR-SAVE-17).
func (e *Engine) ProbeWrite() error {
	e.probeMu.Lock()
	defer e.probeMu.Unlock()

	reason := "unknown"
	err := e.probeWriteLocked(&reason)
	e.probeReason = reason
	e.writable = err == nil && reason == ""
	if err != nil {
		e.logf(diagnostics.LevelInfo, "storage.write.probe", map[string]any{
			"writable": false,
			"reason":   reason,
		})
		return err
	}
	e.logf(diagnostics.LevelInfo, "storage.write.probe", map[string]any{
		"writable": true,
		"reason":   "",
	})
	return nil
}

// probeWriteLocked performs steps 1-5 and classifies the failure. The caller
// holds probeMu.
func (e *Engine) probeWriteLocked(reason *string) error {
	// 1. MkdirAll
	if err := os.MkdirAll(e.dataDir, 0o755); err != nil {
		*reason = classifyMkdirErr(err)
		return err
	}
	base := filepath.Join(e.dataDir, fmt.Sprintf(".write-probe-%d", os.Getpid()))
	// 2. OpenFile with O_EXCL
	f, err := os.OpenFile(base, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		*reason = classifyOpenErr(err)
		return err
	}
	// 3. Write "ok", Sync, Close
	if _, err := f.Write([]byte("ok")); err != nil {
		_ = f.Close()
		_ = os.Remove(base)
		*reason = "unknown"
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(base)
		*reason = "unknown"
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(base)
		*reason = "unknown"
		return err
	}
	// 4. Rename
	renamed := base + ".renamed"
	if err := os.Rename(base, renamed); err != nil {
		_ = os.Remove(base)
		if isEXDEV(err) {
			// §15.7: the probe passes, the engine will use the fallback.
			*reason = ""
			e.markDegraded(renamed)
			_ = os.Remove(base)
			return nil
		}
		*reason = "unknown"
		return err
	}
	// 5. Remove both
	_ = os.Remove(base)
	_ = os.Remove(renamed)
	*reason = ""
	return nil
}

func classifyMkdirErr(err error) string {
	if errors.Is(err, os.ErrPermission) {
		return "permission_denied"
	}
	if isEROFS(err) {
		return "read_only_filesystem"
	}
	return "unknown"
}

func classifyOpenErr(err error) string {
	if errors.Is(err, os.ErrPermission) {
		return "permission_denied"
	}
	if isEROFS(err) {
		return "read_only_filesystem"
	}
	if errors.Is(err, os.ErrNotExist) {
		return "not_a_directory"
	}
	return "unknown"
}

// --- Paths and confinement (§16) ------------------------------------------

// dirFor returns the absolute directory for a data area.
func (e *Engine) dirFor(area string) string { return filepath.Join(e.dataDir, area) }

// confine re-checks a constructed path against the data root even after
// identifier validation has run (§16.4). ErrOutOfRoot is a 500 because it means
// the server built a path it should not have.
func (e *Engine) confine(full string) error {
	if err := paths.Confine(e.dataDir, full); err != nil {
		return kobraerr.OutOfRoot(filepath.Base(full))
	}
	return nil
}

// savePath builds <DataDir>/saves/<slot>.json (§16.1).
func (e *Engine) savePath(slot string) (string, error) {
	if err := ValidateIdentifier(slot); err != nil {
		return "", err
	}
	full := filepath.Join(e.dirFor("saves"), slot+".json")
	if err := e.confine(full); err != nil {
		return "", err
	}
	return full, nil
}

func (e *Engine) configPath() string { return filepath.Join(e.dirFor("config"), "settings.json") }
func (e *Engine) modsPath() string   { return filepath.Join(e.dirFor("config"), "mods.json") }

// --- Atomic write (§15.3) --------------------------------------------------

var seqMu sync.Mutex
var seq uint64

func nextSeq() uint64 {
	seqMu.Lock()
	defer seqMu.Unlock()
	seq++
	return seq
}

// writeAtomic is §15.3, used by every writer in this package.
//
// The context is checked before the write starts. It is deliberately not
// consulted again until after the rename and the directory fsync, where success
// is reported regardless of cancellation: cancelling in the middle would leave
// a temp file with no clear owner (§23.3), and reporting failure after the data
// is durable is what made callers retry a completed write.
func (e *Engine) writeAtomic(ctx context.Context, target string, data []byte, mode os.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := e.checkSpace(int64(len(data))); err != nil {
		return err
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmp := filepath.Join(dir, fmt.Sprintf(".%s.tmp-%d-%d", filepath.Base(target), os.Getpid(), nextSeq()))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	// §26.4: with the kobra_faultinject tag this models a disk that fills
	// mid-write. It is a no-op in every release build.
	if ferr := faultinject.Err(faultinject.StorageWrite); ferr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return ferr
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	// Crash window 1: the payload is durable, the previous revision has not yet
	// been rotated aside. Recovery must find the previous revision intact.
	faultinject.Point(faultinject.StorageWriteAfterFsync)
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	info, err := os.Stat(tmp)
	if err != nil || info.Size() != int64(len(data)) {
		_ = os.Remove(tmp)
		return errors.New("temp file verification failed")
	}

	if _, err := os.Stat(target); err == nil {
		bak := target + ".bak"
		_ = os.Remove(bak)
		if err := os.Rename(target, bak); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	// §26.4 injects the rename failure BEFORE the real rename: an error injected
	// afterwards would leave the temp file already moved, and the fallback would
	// fail for want of a source rather than exercise the copy path.
	renameErr := faultinject.Err(faultinject.StorageRename)
	if renameErr == nil {
		renameErr = os.Rename(tmp, target)
	}
	if renameErr != nil {
		if !isEXDEV(renameErr) {
			_ = os.Remove(tmp)
			return renameErr
		}
		if rerr := e.renameCrossDevice(tmp, target); rerr != nil {
			_ = os.Remove(tmp)
			return renameErr
		}
	}
	// Crash window 2: the new revision is in place, its directory entry is not
	// yet fsynced. What survives a power loss is the filesystem's business; the
	// test asserts the state the next start sees.
	faultinject.Point(faultinject.StorageWriteAfterRename)
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	// The write is durable from here on, so a cancellation that arrived during
	// the rename is NOT a failure: reporting one made callers answer 500 and
	// retry a write that had already landed, and in WriteSave it skipped the
	// revision bump, leaving the in-memory revision behind the file's own.
	return nil
}

// renameCrossDevice is §15.4. Atomicity is degraded here, so the degradation is
// logged and surfaced in the diagnostics payload.
func (e *Engine) renameCrossDevice(src, dst string) error {
	e.markDegraded(dst)
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

// --- Revisions (§18) ------------------------------------------------------

// revisionSidecar is the path of the revision sidecar for a data file.
func revisionSidecar(target string, rev int64) string {
	return target + "." + strconv.FormatInt(rev, 10)
}

// InitRevisions rebuilds the in-memory revision store by scanning saves/ for
// <slot>.json.<rev> sidecars, taking the maximum per slot, and preferring the
// revision recorded inside the save header when no sidecar survives (§18.1).
func (e *Engine) InitRevisions(ctx context.Context) error {
	entries, err := os.ReadDir(e.dirFor("saves"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	e.revMu.Lock()
	defer e.revMu.Unlock()
	rebuilt := map[string]bool{}
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		slot := strings.TrimSuffix(name, ".json")
		if err := ValidateIdentifier(slot); err != nil {
			continue
		}
		key := filepath.ToSlash(filepath.Join("saves", slot+".json"))
		max := int64(0)
		for _, other := range entries {
			on := other.Name()
			if !strings.HasPrefix(on, name+".") {
				continue
			}
			if v, err := strconv.ParseInt(strings.TrimPrefix(on, name+"."), 10, 64); err == nil && v > max {
				max = v
			}
		}
		if max == 0 {
			// No sidecar: fall back to the header's revision, and record that
			// a conflict check may have been weakened for this slot.
			if h, err := e.readSaveHeader(filepath.Join(e.dirFor("saves"), name)); err == nil && h.Revision > 0 {
				max = h.Revision
			} else {
				max = 1
			}
			rebuilt[slot] = true
		}
		e.revisions[key] = max
	}
	for slot := range rebuilt {
		e.logf(diagnostics.LevelWarn, "storage.revision.rebuilt", map[string]any{"slot": slot})
	}
	return ctx.Err()
}

func (e *Engine) revisionOf(key string) int64 {
	e.revMu.Lock()
	defer e.revMu.Unlock()
	return e.revisions[key]
}

func (e *Engine) setRevision(key string, rev int64) {
	e.revMu.Lock()
	e.revisions[key] = rev
	e.revMu.Unlock()
}

// --- Save read/write (§14.2, §15) -----------------------------------------

// saveHeader is the on-disk save file (save.schema.json). Unknown top-level
// fields are ignored on read; the payload is preserved byte-for-byte so an
// older build can round-trip a newer save without data loss (FR-SAVE-4).
type saveHeader struct {
	Schema          string          `json:"schema"`
	SaveVersion     int             `json:"save_version"`
	GameVersion     string          `json:"game_version"`
	Release         string          `json:"release,omitempty"`
	Created         string          `json:"created"`
	Modified        string          `json:"modified"`
	Revision        int64           `json:"revision"`
	PlaytimeSeconds *int64          `json:"playtime_seconds,omitempty"`
	Checksum        string          `json:"checksum,omitempty"`
	Compression     string          `json:"compression,omitempty"`
	Payload         json.RawMessage `json:"payload"`
}

// SaveSummary is one entry of GET /api/data/saves.
type SaveSummary struct {
	Slot            string `json:"slot"`
	SaveVersion     int    `json:"save_version,omitempty"`
	GameVersion     string `json:"game_version,omitempty"`
	Modified        string `json:"modified,omitempty"`
	Revision        int64  `json:"revision"`
	PlaytimeSeconds *int64 `json:"playtime_seconds,omitempty"`
	Size            int64  `json:"size,omitempty"`
	Corrupt         bool   `json:"corrupt,omitempty"`
}

// SaveList is the GET /api/data/saves response.
type SaveList struct {
	Saves        []SaveSummary `json:"saves"`
	RebuiltIndex bool          `json:"rebuilt_index,omitempty"`
}

// SaveWrite is a POST /api/save request after validation.
type SaveWrite struct {
	Slot        string
	IfRevision  *int64
	Claim       bool
	Compression string
	Payload     json.RawMessage
	Meta        *SaveMeta
	SessionID   string
}

// SaveMeta is the optional meta object of a save write.
type SaveMeta struct {
	PlaytimeSeconds *int64 `json:"playtime_seconds,omitempty"`
	Title           string `json:"title,omitempty"`
	Thumbnail       string `json:"thumbnail,omitempty"`
}

// SaveWriteResult is the POST /api/save response.
type SaveWriteResult struct {
	Slot     string
	Revision int64
	Modified time.Time
	Bytes    int64
}

// WriteSave applies one save write: revision check, atomic write, revision
// sidecar, rollover (§14.2, §15.3, §18).
func (e *Engine) WriteSave(ctx context.Context, w SaveWrite) (SaveWriteResult, error) {
	var res SaveWriteResult
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if !e.DataWritable() {
		return res, kobraerr.ReadOnly(e.DataWritableReason())
	}
	target, err := e.savePath(w.Slot)
	if err != nil {
		return res, err
	}
	key := filepath.ToSlash(filepath.Join("saves", w.Slot+".json"))

	// §11.5 writer claim is part of the save path, so it happens here.
	if w.Claim {
		if err := e.ClaimWriter(w.SessionID); err != nil {
			return res, err
		}
	}

	// §17.1: the write lock is taken after validation and before the first
	// filesystem call.
	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	if err := e.checkRevision(key, w.IfRevision); err != nil {
		return res, err
	}

	// The payload is canonicalised so the checksum is stable across a
	// re-save of an unchanged body (save.schema.json).
	canonical, err := canonicaliseJSON(w.Payload)
	if err != nil {
		return res, kobraerr.MalformedBody(err)
	}
	sum := sha256.Sum256(canonical)

	now := time.Now().UTC()
	created := now.Format(time.RFC3339)
	if prev, err := e.readSaveHeader(target); err == nil && prev.Created != "" {
		created = prev.Created
	}
	rev := e.revisionOf(key) + 1

	hdr := map[string]any{
		"schema":       SchemaSave,
		"save_version": e.saveVer,
		"game_version": e.engineVer,
		"created":      created,
		"modified":     now.Format(time.RFC3339),
		"revision":     rev,
		"checksum":     "sha256:" + hex.EncodeToString(sum[:]),
		"payload":      json.RawMessage(canonical),
	}
	if e.release != "" {
		hdr["release"] = e.release
	}
	if w.Compression != "" {
		hdr["compression"] = w.Compression
	}
	if w.Meta != nil && w.Meta.PlaytimeSeconds != nil {
		hdr["playtime_seconds"] = *w.Meta.PlaytimeSeconds
	}
	body, err := json.Marshal(hdr)
	if err != nil {
		return res, kobraerr.IO("The launcher could not serialise the save.", nil, err)
	}
	body = append(body, '\n')

	if err := e.writeAtomic(ctx, target, body, 0o644); err != nil {
		e.logf(diagnostics.LevelError, "save.write.failed", map[string]any{
			"slot":  w.Slot,
			"errno": errnoName(err),
		})
		return res, mapIOError(err)
	}
	// §18.3: the revision is bumped after the write succeeds. A failed sidecar
	// write does not fail the save, because the data is already durable.
	//
	// Eviction runs before the new sidecar is written so that the live
	// revision's own sidecar is never the file that gets evicted.
	e.setRevision(key, rev)
	e.pruneRevisions(w.Slot)
	if err := e.writeRevisionSidecar(ctx, target, rev); err != nil {
		e.logf(diagnostics.LevelWarn, "save.revision.sidecar", map[string]any{"slot": w.Slot})
	}

	res = SaveWriteResult{
		Slot:     w.Slot,
		Revision: rev,
		Modified: now,
		Bytes:    int64(len(canonical)),
	}
	e.logf(diagnostics.LevelInfo, "save.write", map[string]any{
		"slot":  w.Slot,
		"rev":   rev,
		"bytes": res.Bytes,
	})
	return res, nil
}

func (e *Engine) writeRevisionSidecar(ctx context.Context, target string, rev int64) error {
	p := revisionSidecar(target, rev)
	body := []byte(strconv.FormatInt(rev, 10) + "\n")
	return e.writeAtomic(ctx, p, body, 0o644)
}

// pruneRevisions enforces keep_revisions, oldest evicted first (§15.5).
//
// The cap is enforced on *superseded* revisions: the sidecar for the revision
// that is currently live is always retained, because the next conflict check
// reads it to establish "current". Evicting it would manufacture a false
// conflict on the very next write, so keep_revisions=2 leaves at most 3
// sidecars on disk (the live one plus two superseded).
func (e *Engine) pruneRevisions(slot string) {
	if e.keepRevisions <= 0 {
		return
	}
	dir := e.dirFor("saves")
	prefix := slot + ".json."
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var revs []int64
	for _, ent := range entries {
		n := ent.Name()
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimPrefix(n, prefix), 10, 64)
		if err != nil {
			continue
		}
		revs = append(revs, v)
	}
	// This runs before the new sidecar is written, so keeping keepRevisions
	// existing revisions leaves exactly keepRevisions+1 files on disk when the
	// live one is added: the live revision plus its superseded history.
	if len(revs) <= e.keepRevisions {
		return
	}
	sort.Slice(revs, func(i, j int) bool { return revs[i] < revs[j] })
	for _, v := range revs[:len(revs)-e.keepRevisions] {
		_ = os.Remove(filepath.Join(dir, prefix+strconv.FormatInt(v, 10)))
	}
}

// checkRevision implements §18.2.
func (e *Engine) checkRevision(key string, ifRev *int64) error {
	if ifRev == nil {
		return nil // unconditional overwrite, only after explicit user choice
	}
	cur := e.revisionOf(key)
	if *ifRev == cur {
		return nil
	}
	slot := strings.TrimSuffix(filepath.Base(key), ".json")
	return &ConflictError{
		Slot:     slot,
		Current:  cur,
		Modified: e.modifiedOf(slot),
	}
}

func (e *Engine) modifiedOf(slot string) string {
	p, err := e.savePath(slot)
	if err != nil {
		return ""
	}
	if h, err := e.readSaveHeader(p); err == nil {
		return h.Modified
	}
	return ""
}

// ConflictError is a stale if_revision (§18.2). It renders as the conflict
// envelope of data-api.schema.json.
type ConflictError struct {
	Slot     string
	Current  int64
	Modified string
}

func (c *ConflictError) Error() string {
	return fmt.Sprintf("revision conflict for %s: current %d", c.Slot, c.Current)
}

// AsKobra converts the conflict into its wire envelope.
//
// data-api.schema.json's #/$defs/conflict requires current_revision beside
// error/message at the top level, with modified and slot also declared there, so
// those three ride in Wire rather than in detail.
func (c *ConflictError) AsKobra() *kobraerr.KobraError {
	wire := map[string]any{
		"current_revision": c.Current,
		"slot":             c.Slot,
	}
	if c.Modified != "" {
		wire["modified"] = c.Modified
	}
	// #/$defs/conflict sets additionalProperties:false, so detail must stay
	// empty here: every field it declares lives at the top level.
	ke := kobraerr.Conflict("This slot was changed by another tab since you loaded it.", nil)
	ke.Wire = wire
	return ke
}

// ReadSave returns the save file's payload (not the envelope).
func (e *Engine) ReadSave(ctx context.Context, slot string) ([]byte, error) {
	p, err := e.savePath(slot)
	if err != nil {
		return nil, err
	}
	e.writeMu.RLock()
	defer e.writeMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			// A failed atomic write can leave only the .bak revision behind.
			// Reporting "no such slot" would read as save loss to the player.
			if bak, berr := os.ReadFile(p + ".bak"); berr == nil {
				if bhdr, berr := parseSave(bak); berr == nil && verifyChecksum(bhdr) {
					e.logf(diagnostics.LevelWarn, "save.recover", map[string]any{"slot": slot, "from": "bak"})
					return bhdr.Payload, nil
				}
			}
			return nil, kobraerr.NotFound("save slot")
		}
		return nil, mapIOError(err)
	}
	hdr, err := parseSave(raw)
	if err != nil {
		// §11.6 recovery chain: a corrupt slot falls back to .bak.
		if bak, berr := os.ReadFile(p + ".bak"); berr == nil {
			if bhdr, berr := parseSave(bak); berr == nil {
				e.logf(diagnostics.LevelWarn, "save.recover", map[string]any{"slot": slot, "from": "bak"})
				return bhdr.Payload, nil
			}
		}
		e.logf(diagnostics.LevelError, "save.corrupt", map[string]any{"slot": slot, "reason": "header"})
		return nil, kobraerr.IO("That save file could not be read.", map[string]any{"slot": slot}, err)
	}
	if !verifyChecksum(hdr) {
		if bak, berr := os.ReadFile(p + ".bak"); berr == nil {
			if bhdr, berr := parseSave(bak); berr == nil && verifyChecksum(bhdr) {
				e.logf(diagnostics.LevelWarn, "save.recover", map[string]any{"slot": slot, "from": "bak"})
				return bhdr.Payload, nil
			}
		}
		e.logf(diagnostics.LevelError, "save.corrupt", map[string]any{"slot": slot, "reason": "checksum"})
		return nil, kobraerr.IO("That save file failed its integrity check.", map[string]any{"slot": slot}, nil)
	}
	return hdr.Payload, nil
}

// ListSaves enumerates the saves directory, merging the advisory index with the
// per-file headers and preferring the file (§14.6). A missing or corrupt index
// is never an error.
func (e *Engine) ListSaves(ctx context.Context) (SaveList, error) {
	var out SaveList
	if err := ctx.Err(); err != nil {
		return out, err
	}
	e.writeMu.RLock()
	defer e.writeMu.RUnlock()

	dir := e.dirFor("saves")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			out.Saves = []SaveSummary{}
			return out, nil
		}
		return out, mapIOError(err)
	}

	index, indexOK := e.readIndex(dir)
	if !indexOK {
		out.RebuiltIndex = true
	}

	saves := make([]SaveSummary, 0, len(entries))
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		slot := strings.TrimSuffix(name, ".json")
		if err := ValidateIdentifier(slot); err != nil {
			continue
		}
		p := filepath.Join(dir, name)
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		sum := SaveSummary{
			Slot:     slot,
			Revision: e.revisionOf(filepath.ToSlash(filepath.Join("saves", name))),
			Size:     fi.Size(),
		}
		hdr, err := e.readSaveHeader(p)
		if err != nil {
			sum.Corrupt = true
			if sum.Revision == 0 {
				sum.Revision = 1
			}
		} else {
			// The file's header wins over a stale index entry.
			if hdr.Revision > 0 {
				sum.Revision = hdr.Revision
			}
			if sum.Revision == 0 {
				sum.Revision = 1
			}
			sum.Modified = hdr.Modified
			sum.GameVersion = hdr.GameVersion
			sum.SaveVersion = hdr.SaveVersion
			sum.PlaytimeSeconds = hdr.PlaytimeSeconds
			if !verifyChecksum(hdr) {
				sum.Corrupt = true
			}
		}
		if sum.Modified == "" {
			if idx, ok := index[slot]; ok {
				sum.Modified = idx
			}
		}
		if sum.Modified == "" {
			sum.Modified = fi.ModTime().UTC().Format(time.RFC3339)
		}
		saves = append(saves, sum)
	}
	sort.SliceStable(saves, func(i, j int) bool {
		if saves[i].Modified != saves[j].Modified {
			return saves[i].Modified > saves[j].Modified
		}
		return saves[i].Slot < saves[j].Slot
	})
	out.Saves = saves
	return out, nil
}

// readIndex reads data/saves/index.json, the advisory slot index (FS §11.3).
func (e *Engine) readIndex(dir string) (map[string]string, bool) {
	p := filepath.Join(dir, "index.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	var doc struct {
		Saves map[string]struct {
			Modified string `json:"modified"`
		} `json:"saves"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		e.logf(diagnostics.LevelWarn, "storage.index.corrupt", map[string]any{"reason": "parse"})
		return nil, false
	}
	out := map[string]string{}
	for slot, v := range doc.Saves {
		out[slot] = v.Modified
	}
	return out, true
}

// DeleteSave moves the slot and its .bak into data/.trash-<ts>/saves/. It never
// unlinks (§14.7).
func (e *Engine) DeleteSave(ctx context.Context, slot string) error {
	p, err := e.savePath(slot)
	if err != nil {
		return err
	}
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return kobraerr.NotFound("save slot")
		}
		return mapIOError(err)
	}
	trashDir := e.newTrashDir("saves")
	if err := e.moveToTrash(p, trashDir); err != nil {
		return mapIOError(err)
	}
	if _, err := os.Stat(p + ".bak"); err == nil {
		_ = e.moveToTrash(p+".bak", trashDir)
	}
	key := filepath.ToSlash(filepath.Join("saves", slot+".json"))
	e.setRevision(key, 0)
	return ctx.Err()
}

// newTrashDir creates data/.trash-<ts>/<area> and returns it.
func (e *Engine) newTrashDir(area string) string {
	ts := time.Now().UTC().Format("2006-01-02T15-04-05Z")
	return filepath.Join(e.dataDir, ".trash-"+ts, area)
}

// moveToTrash renames into the trash area, falling back to copy-then-remove
// across devices (§14.7, §15.4).
func (e *Engine) moveToTrash(src, trashDir string) error {
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(trashDir, filepath.Base(src))
	if err := os.Rename(src, dst); err != nil {
		if !isEXDEV(err) {
			return err
		}
		if err := e.renameCrossDevice(src, dst); err != nil {
			return err
		}
	}
	return nil
}

// QuarantineStrayTemps moves the temp files an interrupted writeAtomic left
// behind into the trash area (§26.4, FR-SHELL-4).
//
// A crash between creating the temp file and renaming it into place leaves
// ".<name>.tmp-<pid>-<seq>" in data/saves or data/config. The next start must not
// delete it — no recovery flow may remove user data — and must not leave it to
// accumulate, so it is quarantined where support can inspect it and the existing
// trash retention eventually collects it.
func (e *Engine) QuarantineStrayTemps(ctx context.Context) (int, error) {
	quarantined := 0
	trashDir := map[string]string{}
	for _, area := range []string{"saves", "config"} {
		dir := e.dirFor(area)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return quarantined, err
		}
		for _, ent := range entries {
			if err := ctx.Err(); err != nil {
				return quarantined, err
			}
			if ent.IsDir() || !isWriteTempName(ent.Name()) {
				continue
			}
			if _, ok := trashDir[area]; !ok {
				trashDir[area] = e.newTrashDir(area)
			}
			if err := e.moveToTrash(filepath.Join(dir, ent.Name()), trashDir[area]); err != nil {
				// A temp file that cannot be moved stays where it is: it is not
				// worth failing startup over, and leaving it keeps it inspectable.
				e.logf(diagnostics.LevelWarn, "storage.quarantine.failed", map[string]any{"area": area})
				continue
			}
			quarantined++
			event := "save.recover"
			if area == "config" {
				event = "config.recover"
			}
			e.logf(diagnostics.LevelWarn, event, map[string]any{"from": "quarantine"})
		}
	}
	if quarantined > 0 {
		e.logf(diagnostics.LevelInfo, "storage.quarantine", map[string]any{"files": quarantined})
	}
	return quarantined, nil
}

// isWriteTempName recognises the temp files writeAtomic creates:
// ".<name>.tmp-<pid>-<seq>".
func isWriteTempName(name string) bool {
	return strings.HasPrefix(name, ".") && strings.Contains(name, ".tmp-")
}

// CleanupTrash deletes trash directories older than the retention window. It
// runs at startup, never on the delete path (§14.7, §15.5). A retention of 0
// disables cleanup entirely.
func (e *Engine) CleanupTrash(ctx context.Context) (int, error) {
	if e.trashDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-time.Duration(e.trashDays) * 24 * time.Hour)
	entries, err := os.ReadDir(e.dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, ent := range entries {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if !ent.IsDir() || !strings.HasPrefix(ent.Name(), ".trash-") {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(e.dataDir, ent.Name())); err == nil {
			removed++
		}
	}
	if removed > 0 {
		e.logf(diagnostics.LevelInfo, "storage.trash.cleanup", map[string]any{"removed": removed})
	}
	return removed, nil
}

// --- Config (§16.3) -------------------------------------------------------

// ConfigMerge is a POST /api/config request.
type ConfigMerge struct {
	IfRevision *int64
	Merge      map[string]json.RawMessage
}

// RevisionResult is the response to a config write.
type RevisionResult struct {
	Revision int64
	Modified time.Time
}

// readConfigBytes returns the settings.json document, falling back to the .bak
// revision when the live file is missing. writeAtomic rotates the live file to
// .bak before renaming the new one into place, so a failure between those two
// steps leaves only the .bak — reading it is what keeps the user's settings
// instead of reporting an empty document. A document that has never existed is
// "{}", the documented no-settings-yet state.
func (e *Engine) readConfigBytes() (raw []byte, recovered bool, err error) {
	target := e.configPath()
	b, rerr := os.ReadFile(target)
	if rerr == nil {
		return b, false, nil
	}
	if !os.IsNotExist(rerr) {
		return nil, false, rerr
	}
	if bak, berr := os.ReadFile(target + ".bak"); berr == nil {
		return bak, true, nil
	}
	return []byte("{}\n"), false, nil
}

// ReadConfig returns the raw settings.json, or an empty object when the file
// does not exist yet.
func (e *Engine) ReadConfig(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.writeMu.RLock()
	defer e.writeMu.RUnlock()
	raw, recovered, err := e.readConfigBytes()
	if err != nil {
		return nil, mapIOError(err)
	}
	if recovered {
		e.logf(diagnostics.LevelWarn, "config.recover", map[string]any{"from": "bak"})
	}
	return raw, nil
}

// MergeConfig merges top-level keys into settings.json, preserving keys the
// request does not mention (§16.3). Unknown top-level keys are rejected by the
// caller before this point; the engine re-checks nothing about key names.
func (e *Engine) MergeConfig(ctx context.Context, m ConfigMerge) (RevisionResult, error) {
	var res RevisionResult
	if !e.DataWritable() {
		return res, kobraerr.ReadOnly(e.DataWritableReason())
	}
	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	target := e.configPath()
	key := "config/settings.json"
	if err := e.checkRevision(key, m.IfRevision); err != nil {
		return res, err
	}

	cur := map[string]json.RawMessage{}
	raw, recovered, rerr := e.readConfigBytes()
	if rerr != nil {
		return res, mapIOError(rerr)
	}
	if recovered {
		e.logf(diagnostics.LevelWarn, "config.recover", map[string]any{"from": "bak"})
	}
	if err := json.Unmarshal(raw, &cur); err != nil {
		cur = map[string]json.RawMessage{}
	}
	for k, v := range m.Merge {
		cur[k] = v
	}
	body, err := json.Marshal(cur)
	if err != nil {
		return res, kobraerr.MalformedBody(err)
	}
	body = append(body, '\n')
	if err := e.writeAtomic(ctx, target, body, 0o644); err != nil {
		return res, mapIOError(err)
	}
	rev := e.revisionOf(key) + 1
	e.setRevision(key, rev)
	now := time.Now().UTC()
	e.logf(diagnostics.LevelInfo, "config.write", map[string]any{
		"rev":  rev,
		"keys": len(m.Merge),
	})
	return RevisionResult{Revision: rev, Modified: now}, nil
}

// --- Writer election (§17.2) ---------------------------------------------

// Writer returns the session id holding the writer role, or "".
func (e *Engine) Writer() string {
	e.writerMu.Lock()
	defer e.writerMu.Unlock()
	return e.writer
}

// ClaimWriter is the §11.5 atomic claim. Claiming when already the writer is
// idempotent; claiming when another session holds it is a conflict.
func (e *Engine) ClaimWriter(sessionID string) error {
	e.writerMu.Lock()
	defer e.writerMu.Unlock()
	switch {
	case e.writer == sessionID:
		return nil
	case e.writer == "":
		e.writer = sessionID
		return nil
	default:
		return kobraerr.WriterHeld(e.writer)
	}
}

// ReleaseWriter clears the writer role when held by sessionID.
func (e *Engine) ReleaseWriter(sessionID string) {
	e.writerMu.Lock()
	defer e.writerMu.Unlock()
	if e.writer == sessionID {
		e.writer = ""
	}
}

// --- Helpers --------------------------------------------------------------

func parseSave(raw []byte) (*saveHeader, error) {
	var h saveHeader
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, err
	}
	if h.Schema != SchemaSave {
		return nil, fmt.Errorf("unexpected save schema")
	}
	if h.SaveVersion < 1 {
		return nil, fmt.Errorf("save version missing")
	}
	if len(h.Payload) == 0 {
		return nil, fmt.Errorf("save payload missing")
	}
	return &h, nil
}

func (e *Engine) readSaveHeader(path string) (*saveHeader, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseSave(raw)
}

// verifyChecksum checks the payload against the stored checksum. A missing
// checksum is treated as valid so that a save written by an older build is not
// declared corrupt.
func verifyChecksum(h *saveHeader) bool {
	if h.Checksum == "" {
		return true
	}
	canonical, err := canonicaliseJSON(h.Payload)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(canonical)
	return h.Checksum == "sha256:"+hex.EncodeToString(sum[:])
}

// canonicaliseJSON re-encodes JSON with sorted object keys and no insignificant
// whitespace so that a checksum is stable across a re-save of an unchanged
// body. Numbers are preserved via json.Number.
func canonicaliseJSON(raw json.RawMessage) ([]byte, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var buf strings.Builder
	if err := encodeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

func encodeCanonical(b *strings.Builder, v any) error {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		b.WriteString(t.String())
	case float64:
		b.WriteString(strconv.FormatFloat(t, 'g', -1, 64))
	case string:
		enc, err := json.Marshal(t)
		if err != nil {
			return err
		}
		b.Write(enc)
	case []any:
		b.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := encodeCanonical(b, item); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			enc, err := json.Marshal(k)
			if err != nil {
				return err
			}
			b.Write(enc)
			b.WriteByte(':')
			if err := encodeCanonical(b, t[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("unsupported JSON value %T", v)
	}
	return nil
}

// ValidateIdentifier is the single §16.1 rule: lowercase ASCII, a leading
// alphanumeric, 1-64 characters, and no path syntax. It is exported because the
// request pipeline validates path parameters before the engine is reached.
//
// The Windows reserved-name check is paths.ReservedName, the same predicate
// static serving uses. Keeping one implementation matters: the two copies that
// existed here and in the static package disagreed, and this one accepted
// con.txt / nul.json — which CreateFile resolves to a device, so a save slot
// named "nul" silently discarded the write.
func ValidateIdentifier(s string) error {
	if !identifierRe.MatchString(s) {
		return kobraerr.BadIdentifier("identifier", nil)
	}
	// Belt and braces: rejected even though the regex cannot match these.
	if strings.ContainsAny(s, `/\`) || s == "." || s == ".." {
		return kobraerr.BadIdentifier("identifier", nil)
	}
	if paths.ReservedName(s) {
		return kobraerr.BadIdentifier("identifier", nil)
	}
	// A trailing dot or space is stripped by Windows, so "save." and "save"
	// would name the same file. ReservedName covers "con." but not every
	// trailing-dot identifier, so this stays.
	if strings.HasSuffix(s, ".") || strings.HasSuffix(s, " ") {
		return kobraerr.BadIdentifier("identifier", nil)
	}
	return nil
}

// errnoName renders an errno for a log field. An *os.PathError is reduced to
// its errno, which carries no path; any other error is rendered as-is, and for a
// *os.LinkError or *os.SyscallError that can include a path (the diagnostics
// payload publishes the log tail, so this is the one place a path could reach
// the shell — see §22.4).
func errnoName(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return fmt.Sprintf("%v", pe.Err)
	}
	return fmt.Sprintf("%v", err)
}

// mapIOError converts a filesystem error into the documented envelope, adding
// the §17.5 insufficient-space reason where applicable.
func mapIOError(err error) error {
	var ke *kobraerr.KobraError
	if errors.As(err, &ke) {
		return ke
	}
	if isENOSPC(err) {
		return kobraerr.InsufficientSpace()
	}
	if isEROFS(err) {
		return kobraerr.ReadOnly("read_only_filesystem")
	}
	return kobraerr.IO("The launcher could not complete the file operation.", nil, err)
}

// EngineVersion is the engine version recorded in every save header. It is the
// launcher's build version, not the game's release.
func (e *Engine) EngineVersion() string { return e.engineVer }

// SaveVersion is the save format version this build writes.
func (e *Engine) SaveVersion() int { return e.saveVer }

// ReadLock takes the shared data lock and returns the release function. It
// exists so static serving of mod assets can read under the same lock the
// engine writes under, without the static package holding the engine.
func (e *Engine) ReadLock() func() {
	e.writeMu.RLock()
	return e.writeMu.RUnlock
}
