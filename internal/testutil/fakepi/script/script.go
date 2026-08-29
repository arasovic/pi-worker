// Package script defines the deterministic scripting format for the fakepi
// test double. It is a separate importable package so tests can construct
// scripts without importing the fakepi main program.
package script

import "encoding/json"

// Script maps an incoming RPC request type to the deterministic sequence of
// frames fakepi emits for that request. A request type without a trigger
// receives a default successful response.
type Script struct {
	Triggers         map[string][]Step   `json:"triggers"`
	TriggerSequences map[string][][]Step `json:"triggerSequences,omitempty"`
}

// Step is one action in a trigger sequence.
type Step struct {
	// Response writes a response frame for the incoming request. When ID or
	// Command are empty, the request id and request type are echoed, matching
	// Pi response correlation.
	Response *Response `json:"response,omitempty"`
	// Event writes an event frame verbatim.
	Event json.RawMessage `json:"event,omitempty"`
	// Raw writes a literal stdout line, appending a trailing LF when missing.
	// It is used to simulate malformed and oversized frames.
	Raw string `json:"raw,omitempty"`
	// WriteFile writes a file at path, relative to the process's working
	// directory, before continuing the sequence. It lets scripts leave a
	// workspace path behind mid-run the way a real worker's tools would.
	WriteFile string `json:"writeFile,omitempty"`
	// SleepMS pauses before the next step.
	SleepMS int `json:"sleepMs,omitempty"`
	// Exit terminates fakepi immediately without further output.
	Exit bool `json:"exit,omitempty"`
}

// Response describes the response frame written for the incoming request.
type Response struct {
	// ID overrides the echoed request id when non-empty.
	ID string `json:"id,omitempty"`
	// Command overrides the echoed request type when non-empty.
	Command string          `json:"command,omitempty"`
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
}
