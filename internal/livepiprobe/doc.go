// Package livepiprobe holds live end-to-end probes that run the real
// pi-worker binary against a real Pi binary and a real model. They are
// compiled only under the livepi build tag, never run in CI, and are
// skipped on hosts without the required credentials and binary.
package livepiprobe
