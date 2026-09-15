package update

// This file implements the progress and result records of Updater spec §11.
//
// Under decision D1 the download happens at startup, before any server exists,
// so progress cannot be streamed to a browser. It is therefore a *record*: the
// apply rewrites progress.json on every tick, and any later session reads the
// whole story through GET /api/update/progress. If a future release moves the
// download in-session, that endpoint serves the same file live and no protocol
// changes (spec §11.1).

// Phases of an apply (spec R11.2). The set is closed: the endpoint's consumers
// may switch on it exhaustively. A recorded-but-not-yet-started update is
// "idle" with a target release, because the download itself has not begun.
const (
	PhaseIdle      = "idle"
	PhaseDownload  = "downloading"
	PhaseVerify    = "verifying"
	PhaseExtract   = "extracting"
	PhaseSwap      = "swapping"
	PhaseComplete  = "complete"
	PhaseFailed    = "failed"
	PhaseCancelled = "cancelled"
)

// Progress is the polled record of spec R11.1. Percent is always derived from
// the manifest's declared archive size, never from Content-Length (R11.2), and
// Seq increases monotonically within one apply so a reader can detect a stale
// atomic swap (R11.3).
type Progress struct {
	Schema        string `json:"schema"`
	Seq           uint64 `json:"seq"`
	Phase         string `json:"phase"`
	FromRelease   string `json:"from_release,omitempty"`
	TargetRelease string `json:"target_release,omitempty"`
	Bytes         int64  `json:"bytes"`
	TotalBytes    int64  `json:"total_bytes"`
	Percent       int    `json:"percent"`
	Cancellable   bool   `json:"cancellable"`
	Message       string `json:"message,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Updated       string `json:"updated"`
}

// Result is the outcome record, written at every terminal outcome (spec R11.5).
// The shell reads it once on the boot that follows an apply (R11.8) to report
// what happened, which is a response to the user's earlier action rather than a
// background poll.
type Result struct {
	Schema string `json:"schema"`
	// Outcome has the wire name "release" by spec (R11.5), while the API type
	// uses "outcome" (apitypes.UpdateResult). Both names are load-bearing for
	// their readers; the tag here is not a typo.
	Outcome          string `json:"release"`
	TargetRelease    string `json:"target_release,omitempty"`
	InstalledRelease string `json:"installed_release,omitempty"`
	Message          string `json:"message,omitempty"`
	Reason           string `json:"reason,omitempty"`
	BytesWritten     int64  `json:"bytes_written,omitempty"`
	// RequiredBytes and AvailableBytes are set when the disk preflight refuses
	// the update. R10.6 requires the counts here so the UI can explain the
	// refusal rather than only name it. Both are byte counts, never paths.
	RequiredBytes  int64  `json:"required_bytes,omitempty"`
	AvailableBytes int64  `json:"available_bytes,omitempty"`
	RetainedBackup bool   `json:"retained_backup"`
	At             string `json:"at"`
}

// Outcome values recorded in Result.Outcome.
const (
	OutcomeComplete  = "complete"
	OutcomeFailed    = "failed"
	OutcomeCancelled = "cancelled"
	OutcomeSkipped   = "skipped"
)

// progressWriter serialises updates to progress.json. It owns the sequence
// counter and the last written phase so that an unchanged tick does not rewrite
// the file (a download progress callback fires several times a second).
type progressWriter struct {
	sidecarDir string
	p          Progress
	lastPhase  string
	written    bool
}

// initialProgress is the record written when intent is first recorded, so the
// shell sees the pending update with the size the user confirmed before the
// launch that installs it.
func initialProgress(p *Pending) Progress {
	pr := Progress{
		Schema:      ProgressSchema,
		Phase:       PhaseIdle,
		Cancellable: true,
		Updated:     nowRFC3339(),
	}
	if p != nil {
		pr.FromRelease = p.FromRelease
		pr.TargetRelease = p.TargetRelease
		pr.TotalBytes = p.ArchiveSize
		pr.Message = "The update is waiting to be installed."
	}
	return pr
}

func newProgressWriter(sidecarDir string, p *Pending) *progressWriter {
	pr := initialProgress(p)
	pr.Seq = 0
	return &progressWriter{sidecarDir: sidecarDir, p: pr}
}

// setPhase moves the writer to a new phase and flushes. A phase change is
// always written, even when nothing else changed.
func (w *progressWriter) setPhase(phase, message string) {
	w.p.Phase = phase
	w.p.Message = message
	w.p.Cancellable = phase == PhaseDownload
	if phase == PhaseComplete {
		w.p.Percent = 100
	}
	w.flush(true)
}

// setBytes records download progress. It is a no-op when the phase and the byte
// count are unchanged, which keeps a long download from writing a file four
// times a second for no gain.
func (w *progressWriter) setBytes(n, total int64) {
	if w.p.Phase == PhaseDownload && w.p.Bytes == n && w.p.TotalBytes == total {
		return
	}
	w.p.Phase = PhaseDownload
	w.p.Bytes = n
	w.p.TotalBytes = total
	w.p.Percent = percentOf(n, total)
	w.p.Cancellable = true
	w.flush(false)
}

func (w *progressWriter) flush(force bool) {
	if !force && !w.written {
		// The first tick of a phase establishes the file.
		force = true
	}
	w.p.Schema = ProgressSchema
	w.p.Seq++
	w.p.Updated = nowRFC3339()
	_ = writeJSONAtomic(updatePath(w.sidecarDir, ProgressFileName), w.p)
	w.written = true
	if w.p.Phase != PhaseIdle {
		w.lastPhase = w.p.Phase
	}
}

// percentOf derives an integer percentage from declared sizes only (R11.2).
// A non-positive total yields 0 rather than a division by zero.
func percentOf(written, total int64) int {
	if total <= 0 {
		return 0
	}
	if written <= 0 {
		return 0
	}
	if written >= total {
		return 100
	}
	p := int((written * 100) / total)
	if p > 100 {
		p = 100
	}
	return p
}

// ReadProgress loads the progress record. A missing record yields an idle
// record rather than an error, which is what the endpoint reports when no
// update has ever been requested (spec R11.7).
func ReadProgress(sidecarDir string) Progress {
	var p Progress
	ok, err := readJSON(updatePath(sidecarDir, ProgressFileName), &p)
	if !ok || err != nil {
		return Progress{Schema: ProgressSchema, Phase: PhaseIdle}
	}
	if p.Phase == "" {
		p.Phase = PhaseIdle
	}
	return p
}

// ReadResult loads the outcome record, returning nil when there is none.
func ReadResult(sidecarDir string) *Result {
	var r Result
	ok, err := readJSON(updatePath(sidecarDir, ResultFileName), &r)
	if !ok || err != nil {
		return nil
	}
	return &r
}

// WriteResult records a terminal outcome. Failure to write is logged but never
// fails an apply: the record is for the user's benefit, not for correctness.
func WriteResult(sidecarDir string, r Result) {
	r.Schema = ResultSchema
	r.At = nowRFC3339()
	if err := writeJSONAtomic(updatePath(sidecarDir, ResultFileName), r); err != nil {
		logWarn("update.result.write_failed", map[string]any{"reason": "io"})
	}
}

// clearResult removes a previous outcome so a new apply does not report the
// last one's ending.
func clearResult(sidecarDir string) {
	removeIfPresent(updatePath(sidecarDir, ResultFileName))
}
