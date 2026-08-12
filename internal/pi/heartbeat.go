package pi

import (
	"sync"
	"time"
)

// debugTimer is the small timer seam used by the lifecycle heartbeat. Tests
// replace the sink factory with a deterministic timer; production uses a
// standard library timer.
type debugTimer interface {
	C() <-chan time.Time
	Reset(time.Duration)
	Stop()
}

type standardDebugTimer struct{ timer *time.Timer }

func (t *standardDebugTimer) C() <-chan time.Time   { return t.timer.C }
func (t *standardDebugTimer) Reset(d time.Duration) { t.timer.Reset(d) }
func (t *standardDebugTimer) Stop()                 { t.timer.Stop() }

func newStandardDebugTimer(d time.Duration) debugTimer {
	return &standardDebugTimer{timer: time.NewTimer(d)}
}

// startHeartbeat starts the one lifecycle-wide liveness clock for this
// worker. It deliberately does nothing for a disabled scope, and its returned
// closure stops and joins the loop before returning.
func (w *WorkerScope) startHeartbeat(alive func() bool) func() {
	if !w.enabled() {
		return func() {}
	}

	timerFactory := w.sink.newHeartbeatTimer
	if timerFactory == nil {
		timerFactory = newStandardDebugTimer
	}
	timer := timerFactory(debugHeartbeatInterval)
	if timer == nil {
		return func() {}
	}

	w.heartbeatMu.Lock()
	if w.heartbeatRunning {
		stop := w.heartbeatStop
		w.heartbeatMu.Unlock()
		timer.Stop()
		return stop
	}
	stopCh := make(chan struct{})
	done := make(chan struct{})
	activity := make(chan struct{}, 1)
	w.heartbeatRunning = true
	w.heartbeatActivity = activity
	w.lastActivity = w.Elapsed()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			close(stopCh)
			timer.Stop()
			<-done
			w.heartbeatMu.Lock()
			w.heartbeatRunning = false
			w.heartbeatActivity = nil
			w.heartbeatStop = nil
			w.heartbeatMu.Unlock()
		})
	}
	w.heartbeatStop = stop
	w.heartbeatMu.Unlock()

	go func() {
		defer close(done)
		defer timer.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-activity:
				resetDebugTimer(timer, debugHeartbeatInterval)
			case tick := <-timer.C():
				if alive != nil && !alive() {
					return
				}
				elapsed := w.Elapsed()
				w.heartbeatMu.Lock()
				lastActivity := w.lastActivity
				lastPhase := w.lastPhase
				w.heartbeatMu.Unlock()
				silence := elapsed - lastActivity
				if silence < debugHeartbeatInterval {
					resetDebugTimer(timer, debugHeartbeatInterval-silence)
					continue
				}
				if lastPhase == "" {
					lastPhase = "starting"
				}
				// The timer value is intentionally not used for the reported
				// duration: the sink clock describes visible-line silence.
				_ = tick
				if w.sink.logHeartbeat(w, "phase=waiting-for-pi", "last-phase="+lastPhase,
					"silence="+silence.Round(time.Millisecond).String(), "process=alive") {
					// The heartbeat is itself a visible debug line, so the next
					// silence interval begins here without feeding the loop's
					// activity channel back into itself.
					w.heartbeatMu.Lock()
					w.lastActivity = elapsed
					w.heartbeatMu.Unlock()
				}
				resetDebugTimer(timer, debugHeartbeatInterval)
			}
		}
	}()

	return stop
}

func resetDebugTimer(timer debugTimer, d time.Duration) {
	timer.Stop()
	select {
	case <-timer.C():
	default:
	}
	timer.Reset(d)
}
