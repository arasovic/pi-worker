package cli

import (
	"bytes"
	"io"
	"os"
	"time"
)

// debugFollowInterval is how often followDebugLog looks for new debug lines.
const debugFollowInterval = 50 * time.Millisecond

// followDebugLog copies the whole lines appended to a background run's debug
// file onto w until the returned stop is called. A file that does not exist
// yet is looked for again on the next tick, and a trailing line without its
// newline is held back until it is complete. stop copies what is left and
// returns once nothing more will be written to w, so a caller that stops
// after the run is terminal has every line the run wrote.
func followDebugLog(path string, w io.Writer) (stop func()) {
	var file *os.File
	var pending []byte
	buf := make([]byte, 32<<10)
	poll := func() {
		if file == nil {
			opened, err := os.Open(path)
			if err != nil {
				return
			}
			file = opened
		}
		for {
			n, err := file.Read(buf)
			pending = append(pending, buf[:n]...)
			if end := bytes.LastIndexByte(pending, '\n'); end >= 0 {
				_, _ = w.Write(pending[:end+1])
				pending = append(pending[:0], pending[end+1:]...)
			}
			if err != nil || n == 0 {
				return
			}
		}
	}

	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(debugFollowInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				poll()
			case <-quit:
				poll()
				if file != nil {
					_ = file.Close()
				}
				return
			}
		}
	}()
	return func() {
		close(quit)
		<-done
	}
}
