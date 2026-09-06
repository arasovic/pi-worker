package run

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/arasovic/pi-worker/internal/pi"
)

// TestProjectExact4096Bytes asserts a prompt at exactly promptCap bytes
// lands verbatim on the projection with truncated false.
func TestProjectExact4096Bytes(t *testing.T) {
	prompt := strings.Repeat("A", promptCap)
	if len(prompt) != promptCap {
		t.Fatalf("prompt length = %d, want %d", len(prompt), promptCap)
	}
	tasks := []Task{{Prompt: prompt, Model: "acme/m-1"}}
	out := ProjectTasks(tasks)
	if len(out) != 1 {
		t.Fatalf("output length = %d, want 1", len(out))
	}
	got := out[0]
	if got.Prompt != prompt {
		t.Fatal("prompt differs")
	}
	if got.PromptTruncated {
		t.Fatal("promptTruncated should be false for exact-cap input")
	}
}

// TestProjectOverLimitASCII asserts a prompt longer than promptCap is
// cut to promptCap bytes, marked truncated, and the original content
// bytes never appear past position promptCap in the marshaled JSON.
func TestProjectOverLimitASCII(t *testing.T) {
	// Use two distinct byte sequences so a contains-check on the
	// whole JSON cannot confuse the truncated prefix with the tail.
	head := strings.Repeat("X", promptCap)
	tail := strings.Repeat("Y", 100)
	prompt := head + tail
	tasks := []Task{{Prompt: prompt, Model: "acme/m-1"}}
	out := ProjectTasks(tasks)
	if len(out) != 1 {
		t.Fatalf("output length = %d, want 1", len(out))
	}
	got := out[0]
	if len(got.Prompt) != promptCap {
		t.Fatalf("prompt length = %d, want %d", len(got.Prompt), promptCap)
	}
	if got.Prompt != head {
		t.Fatalf("projected prompt lost its prefix")
	}
	if !got.PromptTruncated {
		t.Fatal("promptTruncated should be true for over-limit input")
	}
	// The projection carries only the first promptCap bytes of the
	// original; the tail of Y's must not appear in the JSON at all.
	blob, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), tail) {
		t.Fatalf("project JSON leaks trailing source content: %s", tail)
	}
}

// TestProjectUTF8CutThroughMultiByte asserts that when a prompt longer
// than promptCap cuts through a multi-byte UTF-8 character, trailing
// incomplete bytes are dropped so the result is valid UTF-8 and its
// length stays at most promptCap.
func TestProjectUTF8CutThroughMultiByte(t *testing.T) {
	// \U0001D11E is 4 bytes in UTF-8. 1250 repetitions = 5000 bytes.
	prompt := strings.Repeat("\U0001D11E", 1250)
	if len(prompt) != 5000 {
		t.Fatalf("prompt length = %d, want 5000", len(prompt))
	}
	tasks := []Task{{Prompt: prompt, Model: "acme/m-1"}}
	out := ProjectTasks(tasks)
	if len(out) != 1 {
		t.Fatalf("output length = %d, want 1", len(out))
	}
	got := out[0]
	if len(got.Prompt) > promptCap {
		t.Fatalf("prompt length = %d, want at most %d", len(got.Prompt), promptCap)
	}
	if !utf8.ValidString(got.Prompt) {
		t.Fatalf("recorded prompt is not valid UTF-8")
	}
	if !got.PromptTruncated {
		t.Fatal("promptTruncated should be true for over-limit input")
	}
}

// TestProjectWriteDeclaredOmittedVsEmpty asserts that a task that
// declared nothing records writesDeclared false with no writes field,
// while a task that declared an empty set records writesDeclared:true
// while the optional writes field is omitted — both survive JSON
// marshalling intact.
func TestProjectWriteDeclaredOmittedVsEmpty(t *testing.T) {
	tasks := []Task{
		{Prompt: "no decl", Model: "m", Writes: WriteDeclaration{Declared: false}},
		{Prompt: "empty decl", Model: "m", Writes: WriteDeclaration{Declared: true}},
		{Prompt: "declared paths", Model: "m", Writes: WriteDeclaration{Declared: true, Paths: []string{"a.txt"}}},
	}
	out := ProjectTasks(tasks)
	if len(out) != 3 {
		t.Fatalf("output length = %d, want 3", len(out))
	}
	// First task: writesDeclared false, no writes field.
	b0, err := json.Marshal(out[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b0), `"writesDeclared":false`) {
		t.Fatalf("task 0 missing writesDeclared=false: %s", b0)
	}
	if strings.Contains(string(b0), `"writes"`) {
		t.Fatalf("task 0 should not have writes field: %s", b0)
	}
	// Second task: writesDeclared true, optional writes field omitted.
	b1, err := json.Marshal(out[1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b1), `"writesDeclared":true`) {
		t.Fatalf("task 1 missing writesDeclared=true: %s", b1)
	}
	if strings.Contains(string(b1), `"writes"`) {
		t.Fatalf("task 1 should not have writes field: %s", b1)
	}
	// Third task: writesDeclared true, writes contains one path.
	b2, err := json.Marshal(out[2])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b2), `"writesDeclared":true`) {
		t.Fatalf("task 2 missing writesDeclared=true: %s", b2)
	}
	if !strings.Contains(string(b2), `"writes":["a.txt"]`) {
		t.Fatalf("task 2 missing writes field: %s", b2)
	}
}

// TestProjectDataPathByteCountSHA256 asserts that carried files produce
// Data entries with correct path, byte count, and SHA-256, while the
// content itself is absent from the marshaled JSON.
func TestProjectDataPathByteCountSHA256(t *testing.T) {
	const content = "hello material content"
	sum := sha256.Sum256([]byte(content))
	dataFile := DataFile{Path: "readme.md", Content: []byte(content)}
	tasks := []Task{{
		Prompt: "p",
		Model:  "acme/m-1",
		Data:   []DataFile{dataFile},
	}}
	out := ProjectTasks(tasks)
	if len(out) != 1 {
		t.Fatalf("output length = %d, want 1", len(out))
	}
	d := out[0].Data
	if len(d) != 1 {
		t.Fatalf("data length = %d, want 1", len(d))
	}
	if d[0].Path != "readme.md" {
		t.Fatalf("path = %q, want readme.md", d[0].Path)
	}
	if d[0].Bytes != len(content) {
		t.Fatalf("byteCount = %d, want %d", d[0].Bytes, len(content))
	}
	wantHash := hex.EncodeToString(sum[:])
	if d[0].SHA256 != wantHash {
		t.Fatalf("sha256 = %q, want %q", d[0].SHA256, wantHash)
	}
	blob, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), content) {
		t.Fatalf("marshaled project carries raw content: %s", blob)
	}
}

// TestProjectInputOrderPreserved asserts that tasks come out of
// ProjectTasks in the same order they went in, even though the worker
// goroutines inside Run may complete in any order.
func TestProjectInputOrderPreserved(t *testing.T) {
	models := []string{"m1", "m2", "m3"}
	tasks := make([]Task, 3)
	for i, m := range models {
		tasks[i] = Task{Model: m, Prompt: "p", ThinkingLevel: pi.ThinkingOff}
	}
	out := ProjectTasks(tasks)
	for i, o := range out {
		if o.Model != models[i] {
			t.Fatalf("index %d model = %q, want %q", i, o.Model, models[i])
		}
	}
}

// TestProjectSourceContentNeverMarshaled asserts that the raw content
// bytes of any DataFile supplied to ProjectTasks are absent from the
// final JSON serialization of the projections.
func TestProjectSourceContentNeverMarshaled(t *testing.T) {
	content := "SECRET-PROMPT-DATA-never-log-me"
	dataFile := DataFile{Path: "secret.txt", Content: []byte(content)}
	tasks := []Task{{
		Prompt: "do something",
		Model:  "acme/m-1",
		Data:   []DataFile{dataFile},
	}}
	out := ProjectTasks(tasks)
	blob, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), content) {
		t.Fatalf("project JSON leaks raw content:\n%s\ncontains:\n%s", blob, content)
	}
}
