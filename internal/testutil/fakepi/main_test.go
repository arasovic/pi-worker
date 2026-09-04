package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arasovic/pi-worker/internal/testutil/fakepi/script"
)

func TestLoadSequenceIndexReturnsErrorForMalformedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sequence-state.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatalf("write malformed state: %v", err)
	}
	if _, err := loadSequenceIndex(path); err == nil {
		t.Fatal("loadSequenceIndex succeeded on malformed state, want error")
	}
}

func TestSaveSequenceIndexCleansTempFileOnRenameFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sequence-state.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("make target directory: %v", err)
	}
	if err := saveSequenceIndex(path, map[string]int{"get_available_models": 1}); err == nil {
		t.Fatal("saveSequenceIndex succeeded when rename should fail")
	}
	tempFiles, err := filepath.Glob(filepath.Join(dir, ".fakepi-sequence-state-*.tmp"))
	if err != nil {
		t.Fatalf("glob temp files: %v", err)
	}
	if len(tempFiles) != 0 {
		t.Fatalf("temporary files left behind after failed save: %v", tempFiles)
	}
}

func TestRunReportsSequenceStateFailuresWithFixedDiagnostic(t *testing.T) {
	const fixedDiagnostic = "fakepi: sequence state unavailable\n"

	writeScript := func(t *testing.T, cfg *script.Script) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "script.json")
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshal script: %v", err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write script: %v", err)
		}
		return path
	}

	t.Run("load-failure", func(t *testing.T) {
		dir := t.TempDir()
		scriptPath := writeScript(t, &script.Script{})
		statePath := filepath.Join(dir, "sequence-state.json")
		if err := os.WriteFile(statePath, []byte("not-json"), 0o600); err != nil {
			t.Fatalf("write malformed state: %v", err)
		}
		t.Setenv("FAKEPI_SCRIPT", scriptPath)
		t.Setenv("FAKEPI_SEQUENCE_STATE", statePath)

		var stdout, stderr bytes.Buffer
		if got := run([]string{"--mode", "rpc", "--tools", "allowed"}, strings.NewReader(""), &stdout, &stderr); got != 1 {
			t.Fatalf("run() = %d, want 1", got)
		}
		if stderr.String() != fixedDiagnostic {
			t.Fatalf("stderr = %q, want fixed diagnostic %q", stderr.String(), fixedDiagnostic)
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout = %q, want empty", stdout.String())
		}
	})

	t.Run("save-failure", func(t *testing.T) {
		dir := t.TempDir()
		scriptPath := writeScript(t, &script.Script{TriggerSequences: map[string][][]script.Step{
			"get_available_models": {
				{{Response: &script.Response{Success: true, Data: json.RawMessage(`{"models":[{"provider":"acme","id":"m-1"}]}`)}}},
			},
		}})
		statePath := filepath.Join(dir, "sequence-state.json")
		if err := os.WriteFile(statePath, []byte(`{}`), 0o600); err != nil {
			t.Fatalf("seed state: %v", err)
		}
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatalf("chmod state parent read-only: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		t.Setenv("FAKEPI_SCRIPT", scriptPath)
		t.Setenv("FAKEPI_SEQUENCE_STATE", statePath)

		stdinR, stdinW := io.Pipe()
		t.Cleanup(func() { _ = stdinR.Close() })
		started := make(chan int, 1)
		var stdout, stderr bytes.Buffer
		go func() {
			started <- run([]string{"--mode", "rpc", "--tools", "allowed"}, stdinR, &stdout, &stderr)
		}()
		if _, err := stdinW.Write([]byte(`{"id":"1","type":"get_available_models"}
`)); err != nil {
			t.Fatalf("write request: %v", err)
		}
		if err := stdinW.Close(); err != nil {
			t.Fatalf("close stdin: %v", err)
		}
		if got := <-started; got != 1 {
			t.Fatalf("run() = %d, want 1", got)
		}
		if stderr.String() != fixedDiagnostic {
			t.Fatalf("stderr = %q, want fixed diagnostic %q", stderr.String(), fixedDiagnostic)
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout = %q, want empty", stdout.String())
		}
		if tempFiles, err := filepath.Glob(filepath.Join(dir, ".fakepi-sequence-state-*.tmp")); err != nil {
			t.Fatalf("glob temp files: %v", err)
		} else if len(tempFiles) != 0 {
			t.Fatalf("temporary files left behind after failed save: %v", tempFiles)
		}
	})
}
