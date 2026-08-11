// Command fakepi is a deterministic scripted stand-in for the Pi 0.84.1 RPC
// mode that internal/pi tests drive as a child process. It accepts the exact
// pi CLI flags passed by internal/pi/process.go, reads one JSON object per
// stdin line, and emits one JSON object per stdout line. Test configuration
// is carried in environment variables so the fixed worker argv never embeds
// test state. It never contains or reads real credentials.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"

	"pi-worker/internal/testutil/fakepi/script"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("fakepi", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	mode := flags.String("mode", "", "pi rpc mode")
	sessionDir := flags.String("session-dir", "", "pi session directory")
	name := flags.String("name", "", "pi session name")
	noContextFiles := flags.Bool("no-context-files", false, "pi disabling flag")
	noExtensions := flags.Bool("no-extensions", false, "pi disabling flag")
	noSkills := flags.Bool("no-skills", false, "pi disabling flag")
	noPromptTemplates := flags.Bool("no-prompt-templates", false, "pi disabling flag")
	noThemes := flags.Bool("no-themes", false, "pi disabling flag")
	noApprove := flags.Bool("no-approve", false, "pi disabling flag")
	tools := flags.String("tools", "", "pi tool allowlist")
	hold := flags.Bool("hold", false, "block forever as a spawned test descendant")
	if err := flags.Parse(args); err != nil {
		fmt.Fprintf(stderr, "fakepi: invalid pi flags: %v\n", err)
		return 2
	}
	if *hold {
		// A spawned test descendant: block forever (a bare select {}
		// would trip the runtime deadlock detector) so the worker's
		// inherited process-group/job cleanup must terminate it.
		for {
			time.Sleep(24 * time.Hour)
		}
	}
	_ = sessionDir
	_ = name
	_ = noContextFiles
	_ = noExtensions
	_ = noSkills
	_ = noPromptTemplates
	_ = noThemes
	_ = noApprove
	if *mode != "rpc" {
		fmt.Fprintf(stderr, "fakepi: expected --mode rpc, got %q\n", *mode)
		return 2
	}
	if *tools == "" {
		fmt.Fprintf(stderr, "fakepi: expected a --tools allowlist\n")
		return 2
	}

	scriptPath := os.Getenv("FAKEPI_SCRIPT")
	logPath := os.Getenv("FAKEPI_LOG")
	metaPath := os.Getenv("FAKEPI_META")
	envEcho := os.Getenv("FAKEPI_ENV")

	if metaPath != "" {
		writeJSONFile(metaPath, map[string]any{
			"argv": os.Args,
			"cwd":  mustGetwd(),
			"env":  map[string]string{envEcho: os.Getenv(envEcho)},
		})
	}

	// FAKEPI_PIDFILE records fakepi's own pid; FAKEPI_SPAWN_PIDFILE makes
	// fakepi launch a long-lived descendant and record that pid. Together
	// they let lifecycle tests prove the worker terminates the direct child
	// and descendants that remain in its inherited process group/job.
	// FAKEPI_SPAWN_DETACH_PIDFILE does the same in a fresh session and
	// process group, outside the inherited boundary, as Pi's built-in bash
	// tool does when it starts commands in their own group.
	// FAKEPI_SPAWN_DETACH_STDERR_PIDFILE additionally binds the descendant's
	// stderr to fakepi's own stderr, so the descendant inherits fakepi's
	// fd 2 and keeps it open after fakepi exits: the worker-side stderr
	// pipe is not drained until the detached descendant dies.
	if pidPath := os.Getenv("FAKEPI_PIDFILE"); pidPath != "" {
		writePIDFile(pidPath, os.Getpid())
	}
	if pidPath := os.Getenv("FAKEPI_SPAWN_PIDFILE"); pidPath != "" {
		spawnDescendant(pidPath)
	}
	if pidPath := os.Getenv("FAKEPI_SPAWN_DETACH_PIDFILE"); pidPath != "" {
		spawnDetachedDescendant(pidPath)
	}
	if pidPath := os.Getenv("FAKEPI_SPAWN_DETACH_STDERR_PIDFILE"); pidPath != "" {
		spawnDetachedStderrDescendant(pidPath)
	}

	// FAKEPI_STDERR lets tests place distinctive content on the child stderr
	// stream to verify pi-worker never surfaces child stderr in debug output.
	if stderrEcho := os.Getenv("FAKEPI_STDERR"); stderrEcho != "" {
		fmt.Fprintln(stderr, stderrEcho)
	}

	scriptConfig := script.Script{}
	if scriptPath != "" {
		data, err := os.ReadFile(scriptPath)
		if err != nil {
			fmt.Fprintf(stderr, "fakepi: read script: %v\n", err)
			return 1
		}
		if err := json.Unmarshal(data, &scriptConfig); err != nil {
			fmt.Fprintf(stderr, "fakepi: decode script: %v\n", err)
			return 1
		}
	}

	out := bufio.NewWriter(stdout)
	defer out.Flush()

	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	sequenceIndex := make(map[string]int)
	for scanner.Scan() {
		var req struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue // the client under test never sends malformed requests
		}
		logRequest(logPath, req.ID, req.Type)

		steps := scriptConfig.Triggers[req.Type]
		if sequences := scriptConfig.TriggerSequences[req.Type]; len(sequences) > 0 {
			index := sequenceIndex[req.Type]
			sequenceIndex[req.Type] = index + 1
			if index < len(sequences) {
				steps = sequences[index]
			} else {
				steps = nil
			}
		}
		if len(steps) == 0 {
			writeResponse(out, req.ID, req.Type, &script.Response{Success: true})
			continue
		}
		for _, step := range steps {
			switch {
			case step.Exit:
				return 0
			case step.SleepMS > 0:
				time.Sleep(time.Duration(step.SleepMS) * time.Millisecond)
			case step.Response != nil:
				writeResponse(out, req.ID, req.Type, step.Response)
			case len(step.Event) > 0:
				writeRaw(out, step.Event)
			default:
				writeRaw(out, []byte(step.Raw))
			}
		}
	}

	if os.Getenv("FAKEPI_HOLD") == "1" {
		// Ignore stdin EOF forever (a bare select {} would trip the runtime
		// deadlock detector and exit by itself, defeating the hold).
		for {
			time.Sleep(24 * time.Hour)
		}
	}
	return 0
}

func writeResponse(out *bufio.Writer, reqID, reqType string, resp *script.Response) {
	frame := map[string]any{
		"type":    "response",
		"command": resp.Command,
		"success": resp.Success,
	}
	if frame["command"] == "" {
		frame["command"] = reqType
	}
	id := resp.ID
	if id == "" {
		id = reqID
	}
	if id != "" {
		frame["id"] = id
	}
	if len(resp.Data) > 0 {
		frame["data"] = json.RawMessage(resp.Data)
	}
	if resp.Error != "" {
		frame["error"] = resp.Error
	}
	writeRaw(out, mustMarshal(frame))
}

func writeRaw(out *bufio.Writer, payload []byte) {
	if _, err := out.Write(payload); err != nil {
		return
	}
	if len(payload) == 0 || payload[len(payload)-1] != '\n' {
		_ = out.WriteByte('\n')
	}
	_ = out.Flush()
}

func logRequest(path, id, typ string) {
	if path == "" {
		return
	}
	frame := map[string]string{"type": typ}
	if id != "" {
		frame["id"] = id
	}
	data := mustMarshal(frame)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(data)
	_, _ = file.Write([]byte("\n"))
}

func mustMarshal(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return wd
}

func writeJSONFile(path string, v any) {
	if err := os.WriteFile(path, mustMarshal(v), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "fakepi: write %s: %v\n", path, err)
	}
}

// spawnDescendant launches a long-lived child in fakepi's own process
// group, so lifecycle tests can prove the worker kills inherited boundary
// members, not just the direct child. The descendant's pid is recorded in
// pidPath when the spawn succeeds; it is intentionally never waited on.
func spawnDescendant(pidPath string) {
	cmd := exec.Command(os.Args[0], "--hold")
	if err := cmd.Start(); err != nil {
		return
	}
	writePIDFile(pidPath, cmd.Process.Pid)
}

// spawnDetachedDescendant launches a long-lived child in its own session
// (and therefore its own process group), outside fakepi's inherited
// boundary. This mirrors Pi's built-in bash tool starting a command in a
// different process group: a group-only kill misses it. The descendant's
// pid is recorded in pidPath when the spawn succeeds; it is intentionally
// never waited on. The detach mechanism is platform-split (detach_unix.go,
// detach_other.go); the Unix regression tests only run on darwin/linux.
func spawnDetachedDescendant(pidPath string) {
	cmd := exec.Command(os.Args[0], "--hold")
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return
	}
	writePIDFile(pidPath, cmd.Process.Pid)
}

// spawnDetachedStderrDescendant launches a long-lived child in its own
// session (and therefore its own process group) with cmd.Stderr explicitly
// set to os.Stderr, so the descendant inherits fakepi's fd 2 and keeps it
// open after fakepi exits. From the worker's side that fd is the write end
// of the stderr pipe created by exec.Cmd when the worker redirects child
// stderr, so a worker that waits for its stderr copy goroutine cannot
// return while the detached descendant lives. The descendant's pid is
// recorded in pidPath when the spawn succeeds; it is intentionally never
// waited on. Test-only: it never prints or logs environment values.
func spawnDetachedStderrDescendant(pidPath string) {
	cmd := exec.Command(os.Args[0], "--hold")
	cmd.Stderr = os.Stderr
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return
	}
	writePIDFile(pidPath, cmd.Process.Pid)
}

func writePIDFile(path string, pid int) {
	_ = os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o600)
}
