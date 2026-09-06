// Package run coordinates bounded parallel worker slices: up to three
// foreground workers execute accepted tasks concurrently in one shared
// workspace, and the controller aggregates their outcomes into one run
// result.
package run

import (
	"crypto/sha256"
	"encoding/hex"
	"unicode/utf8"

	"github.com/arasovic/pi-worker/internal/pi"
)

// promptCap is the largest prompt the start line records verbatim, in
// bytes. A longer prompt is cut to this many bytes and marked truncated
// in the record; the run itself is unaffected — the worker always
// receives the full prompt. The cap bounds the record, not the run.
const promptCap = 4096

// TaskProjection is one task's read-only slice through the run log. It
// carries model, thinking level, and the bounded prompt projection persisted for the run,
// plus whatever the task declared and carried, stripped to a safe
// shape: no raw file content, because that would leak a material file's
// contents into the persistent run record. Declared and paths stay
// separate so a caller can tell "said nothing" from "declared empty".
type TaskProjection struct {
	Model           string        `json:"model"`
	ThinkingLevel   string        `json:"thinkingLevel"`
	Prompt          string        `json:"prompt"`
	PromptTruncated bool          `json:"promptTruncated"`
	WritesDeclared  bool          `json:"writesDeclared"`
	Writes          []string      `json:"writes,omitempty"`
	Data            []pi.DataFile `json:"data,omitempty"`
}

// ProjectTasks projects a run's tasks into the start line. Content must
// not leak: Task carries the bytes of files composed into the prompt,
// and only their path, byte count and SHA-256 are recorded. The hash
// is computed over the content exactly as carried, so it matches a
// checksum of the file on disk and stays consistent with the recorded
// byte count. The write declaration keeps its two facts separate,
// exactly as in WriteDeclaration: a task that declared nothing differs
// from a task that declared an empty write set, and collapsing them
// would erase that difference.
func ProjectTasks(tasks []Task) []TaskProjection {
	projected := make([]TaskProjection, len(tasks))
	for i, task := range tasks {
		prompt, truncated := capPrompt(task.Prompt)
		data := make([]pi.DataFile, 0, len(task.Data))
		for _, file := range task.Data {
			sum := sha256.Sum256(file.Content)
			data = append(data, pi.DataFile{Path: file.Path, Bytes: len(file.Content), SHA256: hex.EncodeToString(sum[:])})
		}
		projected[i] = TaskProjection{
			Model:           task.Model,
			ThinkingLevel:   string(task.ThinkingLevel),
			Prompt:          prompt,
			PromptTruncated: truncated,
			WritesDeclared:  task.Writes.Declared,
			Writes:          task.Writes.Paths,
			Data:            data,
		}
	}
	return projected
}

// capPrompt bounds a recorded prompt to promptCap bytes. A prompt at or
// below the cap is returned verbatim with truncated false. A longer
// prompt is cut to the cap and then loses trailing bytes while the
// result is not valid UTF-8, so a multi-byte character is never split:
// the cut backs off to the last complete character boundary at or
// before the cap.
func capPrompt(prompt string) (string, bool) {
	if len(prompt) <= promptCap {
		return prompt, false
	}
	cut := prompt[:promptCap]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut, true
}
