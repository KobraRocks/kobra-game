package update

// This file implements the stall watchdog of Updater spec R8.5.
//
// Client.Timeout is unusable for an archive download: it bounds the *whole*
// request, so a 20-second value (the existing manifest client, §8.2) kills every
// realistic download, and raising it kills a genuinely stalled one just as
// surely. The correct limit is an idle deadline that resets on every read, so a
// slow-but-progressing transfer survives while a hung connection does not.

import (
	"io"
	"sync"
	"time"
)

// stallGuard cancels a download when no bytes have arrived for a while. It
// reports activity from the reader goroutine and runs the timer on its own, so
// a slow local disk cannot be mistaken for a stalled network.
type stallGuard struct {
	timeout  time.Duration
	cancel   func()
	activity chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// newStallGuard builds a watchdog. A non-positive timeout means "use the
// default idle deadline" (R8.5). Every production caller passes zero, because
// ApplyOptions.StallTimeout is documented as a test override, so the default is
// applied here rather than being left to each call site.
func newStallGuard(timeout time.Duration, cancel func()) *stallGuard {
	if timeout <= 0 {
		timeout = dlStallTimeout
	}
	g := &stallGuard{
		timeout:  timeout,
		cancel:   cancel,
		activity: make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	return g
}

// start runs the watchdog until stop is called. The caller must call stop.
func (g *stallGuard) start() {
	if g == nil {
		return
	}
	go g.loop()
}

func (g *stallGuard) loop() {
	t := time.NewTimer(g.timeout)
	defer t.Stop()
	for {
		select {
		case <-g.done:
			return
		case <-g.activity:
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			t.Reset(g.timeout)
		case <-t.C:
			// No successful read within the window: call the connection dead.
			g.cancel()
			return
		}
	}
}

// touch reports that bytes arrived. It never blocks: a full channel already
// means the watchdog is about to be reset.
func (g *stallGuard) touch() {
	if g == nil {
		return
	}
	select {
	case g.activity <- struct{}{}:
	default:
	}
}

func (g *stallGuard) stop() {
	if g == nil {
		return
	}
	g.stopOnce.Do(func() { close(g.done) })
}

// guardedReader reports every successful read to the watchdog. It wraps only
// the network body, so a slow filesystem write is not counted as a stall.
type guardedReader struct {
	r io.Reader
	g *stallGuard
}

func (r *guardedReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.g.touch()
	}
	return n, err
}
