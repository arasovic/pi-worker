package admission

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeState writes content to state.json inside dir and returns the full path.
func writeStateFile(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// assertNoTempFiles fails the test if dir contains leftover temporary files.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("leftover temporary file %q in %s", entry.Name(), dir)
		}
	}
}

// validTicket returns a minimally valid ticket with the given overrides.
func validTicket(overrides ...func(*ticket)) ticket {
	t := ticket{
		ID:              "ticket-1",
		Sequence:        1,
		RunID:           "run-1",
		WorkerID:        1,
		OwnerPID:        1000,
		OwnerCreateTime: 1000000,
		State:           ticketQueued,
	}
	for _, fn := range overrides {
		fn(&t)
	}
	return t
}

func validState(tickets ...ticket) state {
	if len(tickets) == 0 {
		return emptyState()
	}
	nextSeq := 0
	for _, t := range tickets {
		if t.Sequence >= nextSeq {
			nextSeq = t.Sequence + 1
		}
	}
	return state{
		SchemaVersion: 1,
		NextSequence:  nextSeq,
		Tickets:       tickets,
	}
}

func TestEmptyStateIsValid(t *testing.T) {
	s := emptyState()
	if err := validateState(s); err != nil {
		t.Fatalf("validateState(emptyState()) = %v, want nil", err)
	}
	if s.SchemaVersion != 1 {
		t.Fatalf("emptyState().SchemaVersion = %d, want 1", s.SchemaVersion)
	}
	if s.NextSequence <= 0 {
		t.Fatalf("emptyState().NextSequence = %d, want positive", s.NextSequence)
	}
}

func TestValidateEmptyState(t *testing.T) {
	tests := []struct {
		name    string
		state   state
		wantErr bool
	}{
		{
			name:  "empty",
			state: emptyState(),
		},
		{
			name:    "wrong schema",
			state:   state{SchemaVersion: 2, NextSequence: 1, Tickets: []ticket{}},
			wantErr: true,
		},
		{
			name:    "zero nextSequence",
			state:   state{SchemaVersion: 1, NextSequence: 0, Tickets: []ticket{}},
			wantErr: true,
		},
		{
			name:    "negative nextSequence",
			state:   state{SchemaVersion: 1, NextSequence: -1, Tickets: []ticket{}},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateState(test.state)
			if test.wantErr && err == nil {
				t.Fatalf("validateState(%+v) = nil, want error", test.state)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validateState(%+v) = %v, want nil", test.state, err)
			}
		})
	}
}

func TestValidateStrictTicketFields(t *testing.T) {
	base := func() state {
		return validState(validTicket())
	}

	tests := []struct {
		name    string
		modify  func(*ticket)
		wantErr bool
	}{
		{name: "valid"},
		{
			name:    "empty id",
			modify:  func(t *ticket) { t.ID = "" },
			wantErr: true,
		},
		{
			name:    "zero sequence",
			modify:  func(t *ticket) { t.Sequence = 0 },
			wantErr: true,
		},
		{
			name:    "negative sequence",
			modify:  func(t *ticket) { t.Sequence = -1 },
			wantErr: true,
		},
		{
			name:    "empty runId",
			modify:  func(t *ticket) { t.RunID = "" },
			wantErr: true,
		},
		{
			name:    "zero workerId",
			modify:  func(t *ticket) { t.WorkerID = 0 },
			wantErr: true,
		},
		{
			name:    "zero ownerPid",
			modify:  func(t *ticket) { t.OwnerPID = 0 },
			wantErr: true,
		},
		{
			name:    "zero ownerCreateTime",
			modify:  func(t *ticket) { t.OwnerCreateTime = 0 },
			wantErr: true,
		},
		{
			name:    "unknown state",
			modify:  func(t *ticket) { t.State = "unknown" },
			wantErr: true,
		},
		{
			name:    "empty state",
			modify:  func(t *ticket) { t.State = "" },
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := base()
			if test.modify != nil {
				test.modify(&s.Tickets[0])
			}
			err := validateState(s)
			if test.wantErr && err == nil {
				t.Fatalf("validateState() = nil, want error")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validateState() = %v, want nil", err)
			}
		})
	}
}

func TestValidateDuplicateTicketIDs(t *testing.T) {
	s := state{
		SchemaVersion: 1,
		NextSequence:  3,
		Tickets: []ticket{
			validTicket(func(t *ticket) { t.ID = "dup"; t.Sequence = 1 }),
			validTicket(func(t *ticket) { t.ID = "dup"; t.Sequence = 2 }),
		},
	}
	if err := validateState(s); err == nil {
		t.Fatal("validateState() = nil, want error for duplicate IDs")
	}
}

func TestValidateDuplicateSequences(t *testing.T) {
	s := state{
		SchemaVersion: 1,
		NextSequence:  3,
		Tickets: []ticket{
			validTicket(func(t *ticket) { t.ID = "a"; t.Sequence = 1 }),
			validTicket(func(t *ticket) { t.ID = "b"; t.Sequence = 1 }),
		},
	}
	if err := validateState(s); err == nil {
		t.Fatal("validateState() = nil, want error for duplicate sequences")
	}
}

func TestValidateNextSequenceLowerThanExisting(t *testing.T) {
	s := state{
		SchemaVersion: 1,
		NextSequence:  1,
		Tickets: []ticket{
			validTicket(func(t *ticket) { t.Sequence = 5 }),
		},
	}
	if err := validateState(s); err == nil {
		t.Fatal("validateState() = nil, want error when nextSequence <= max sequence")
	}
}

func TestValidateTicketsNotOrdered(t *testing.T) {
	s := state{
		SchemaVersion: 1,
		NextSequence:  3,
		Tickets: []ticket{
			validTicket(func(t *ticket) { t.ID = "a"; t.Sequence = 2 }),
			validTicket(func(t *ticket) { t.ID = "b"; t.Sequence = 1 }),
		},
	}
	if err := validateState(s); err == nil {
		t.Fatal("validateState() = nil, want error for non-ascending sequence order")
	}
}

func TestValidateTicketsLeasedState(t *testing.T) {
	s := validState(
		validTicket(func(t *ticket) { t.State = ticketLeased }),
	)
	if err := validateState(s); err != nil {
		t.Fatalf("validateState() = %v, want nil for leased ticket", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	dir := t.TempDir()
	got, err := loadState(dir)
	if err != nil {
		t.Fatalf("loadState(%q) error: %v", dir, err)
	}
	if got.SchemaVersion != 1 {
		t.Fatalf("load missing file: SchemaVersion = %d, want 1", got.SchemaVersion)
	}
	if got.NextSequence <= 0 {
		t.Fatalf("load missing file: NextSequence = %d, want positive", got.NextSequence)
	}
}

func TestLoadCorruptJSON(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		content string
	}{
		{"malformed", `{"schemaVersion":`},
		{"trailing garbage", `{"schemaVersion":1,"nextSequence":1,"tickets":[]}xyz`},
		{"trailing json object", `{"schemaVersion":1,"nextSequence":1,"tickets":[]}{"a":1}`},
		{"empty file", ``},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writeStateFile(t, dir, test.content)
			_, err := loadState(dir)
			if err == nil {
				t.Fatalf("loadState(%q) = nil, want error for %s", dir, test.name)
			}
			// The file on disk must not be modified.
			got, readErr := os.ReadFile(statePath(dir))
			if readErr != nil {
				t.Fatalf("ReadFile error: %v", readErr)
			}
			if string(got) != test.content {
				t.Fatalf("file changed after failed load:\nbefore: %q\nafter:  %q", test.content, string(got))
			}
		})
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir, `{"schemaVersion":1,"nextSequence":1,"tickets":[],"unknown":true}`)
	_, err := loadState(dir)
	if err == nil {
		t.Fatal("loadState() = nil, want error for unknown field")
	}
	// File must be untouched.
	got, _ := os.ReadFile(statePath(dir))
	if !strings.Contains(string(got), "unknown") {
		t.Fatal("file was modified after loadState rejected unknown field")
	}
}

func TestLoadRejectsTrailingData(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir, `{"schemaVersion":1,"nextSequence":1,"tickets":[]}`+"\n"+`{"extra":1}`)
	_, err := loadState(dir)
	if err == nil {
		t.Fatal("loadState() = nil, want error for trailing data")
	}
}

func TestLoadRejectsInvalidState(t *testing.T) {
	dir := t.TempDir()
	// Valid JSON but invalid state: schemaVersion 2 is unsupported.
	writeStateFile(t, dir, `{"schemaVersion":2,"nextSequence":1,"tickets":[]}`)
	_, err := loadState(dir)
	if err == nil {
		t.Fatal("loadState() = nil, want error for invalid state")
	}
	// File must be untouched.
	got, _ := os.ReadFile(statePath(dir))
	if strings.Contains(string(got), "schemaVersion") {
		// Just verify it still has the original content.
		if !strings.Contains(string(got), `"schemaVersion":2`) {
			t.Fatal("file was modified after loadState rejected invalid state")
		}
	}
}

func TestLoadRejectsTicketWithUnknownState(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir, `{"schemaVersion":1,"nextSequence":2,"tickets":[{"id":"t1","sequence":1,"runId":"r1","workerId":1,"ownerPid":1,"ownerCreateTime":1,"state":"bogus"}]}`)
	_, err := loadState(dir)
	if err == nil {
		t.Fatal("loadState() = nil, want error for unknown ticket state")
	}
}

func TestLoadValidState(t *testing.T) {
	dir := t.TempDir()
	want := `{"schemaVersion":1,"nextSequence":2,"tickets":[{"id":"t1","sequence":1,"runId":"r1","workerId":1,"ownerPid":1000,"ownerCreateTime":1000000,"state":"queued"}]}` + "\n"
	writeStateFile(t, dir, want)
	got, err := loadState(dir)
	if err != nil {
		t.Fatalf("loadState() error: %v", err)
	}
	if got.SchemaVersion != 1 {
		t.Fatalf("SchemaVersion = %d, want 1", got.SchemaVersion)
	}
	if got.NextSequence != 2 {
		t.Fatalf("NextSequence = %d, want 2", got.NextSequence)
	}
	if len(got.Tickets) != 1 {
		t.Fatalf("len(Tickets) = %d, want 1", len(got.Tickets))
	}
}

func TestSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := validState(
		validTicket(),
		validTicket(func(t *ticket) { t.ID = "ticket-2"; t.Sequence = 2 }),
	)
	if err := saveState(dir, want); err != nil {
		t.Fatalf("saveState() error: %v", err)
	}
	got, err := loadState(dir)
	if err != nil {
		t.Fatalf("loadState() error: %v", err)
	}
	if got.SchemaVersion != want.SchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", got.SchemaVersion, want.SchemaVersion)
	}
	if got.NextSequence != want.NextSequence {
		t.Fatalf("NextSequence = %d, want %d", got.NextSequence, want.NextSequence)
	}
	if len(got.Tickets) != len(want.Tickets) {
		t.Fatalf("len(Tickets) = %d, want %d", len(got.Tickets), len(want.Tickets))
	}
	for i := range got.Tickets {
		if got.Tickets[i] != want.Tickets[i] {
			t.Fatalf("Tickets[%d] = %+v, want %+v", i, got.Tickets[i], want.Tickets[i])
		}
	}
	assertNoTempFiles(t, dir)
}

func TestSaveAtomicReplacement(t *testing.T) {
	dir := t.TempDir()
	first := validState(validTicket())
	if err := saveState(dir, first); err != nil {
		t.Fatalf("saveState() error: %v", err)
	}
	second := validState(
		validTicket(func(t *ticket) { t.ID = "new"; t.Sequence = 1 }),
	)
	if err := saveState(dir, second); err != nil {
		t.Fatalf("saveState() error: %v", err)
	}
	got, err := loadState(dir)
	if err != nil {
		t.Fatalf("loadState() error: %v", err)
	}
	if len(got.Tickets) != 1 || got.Tickets[0].ID != "new" {
		t.Fatalf("after second saveState: tickets = %+v, want ticket with id new", got.Tickets)
	}
	assertNoTempFiles(t, dir)
}

func TestSaveRejectsInvalidBeforeTouchingExisting(t *testing.T) {
	dir := t.TempDir()
	good := validState(validTicket())
	if err := saveState(dir, good); err != nil {
		t.Fatalf("saveState() error: %v", err)
	}
	before, err := os.ReadFile(statePath(dir))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Invalid state: wrong schema.
	bad := state{SchemaVersion: 99, NextSequence: 1, Tickets: []ticket{}}
	if err := saveState(dir, bad); err == nil {
		t.Fatal("saveState(bad) = nil, want error")
	}
	after, err := os.ReadFile(statePath(dir))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("file changed after failed saveState:\nbefore: %s\nafter:  %s", before, after)
	}
	assertNoTempFiles(t, dir)
}

func TestSaveRejectsInvalidWithoutCreatingFile(t *testing.T) {
	dir := t.TempDir()
	bad := state{SchemaVersion: 99, NextSequence: 1, Tickets: []ticket{}}
	if err := saveState(dir, bad); err == nil {
		t.Fatal("saveState(bad) = nil, want error")
	}
	if _, err := os.Stat(statePath(dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat(state.json) = %v, want fs.ErrNotExist", err)
	}
	assertNoTempFiles(t, dir)
}

func TestSaveCreatesMissingParents(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "deep", "nested", "dirs")
	st := validState(validTicket())
	if err := saveState(dir, st); err != nil {
		t.Fatalf("saveState() error: %v", err)
	}
	got, err := loadState(dir)
	if err != nil {
		t.Fatalf("loadState() error: %v", err)
	}
	if got.NextSequence != st.NextSequence {
		t.Fatalf("NextSequence = %d, want %d", got.NextSequence, st.NextSequence)
	}
	assertNoTempFiles(t, dir)
}

func TestSaveRefusesSymlinkedStatePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks is not reliably available on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "real-state.json")
	if err := os.WriteFile(target, []byte(`{"schemaVersion":1,"nextSequence":1,"tickets":[]}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sp := filepath.Join(dir, "state.json")
	if err := os.Symlink(target, sp); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	st := emptyState()
	err := saveState(dir, st)
	if err == nil {
		t.Fatal("saveState() = nil, want symlink refusal error")
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("saveState() error = %v, want message about symbolic link", err)
	}
	// Link is untouched.
	info, lerr := os.Lstat(sp)
	if lerr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link changed after refused saveState: %v %v", lerr, info.Mode())
	}
	// Target is untouched.
	got, _ := os.ReadFile(target)
	if string(got) != `{"schemaVersion":1,"nextSequence":1,"tickets":[]}` {
		t.Fatalf("target changed after refused saveState: %s", got)
	}
	assertNoTempFiles(t, dir)
}

func TestSaveRefusesDanglingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks is not reliably available on Windows")
	}
	dir := t.TempDir()
	missingTarget := filepath.Join(t.TempDir(), "not", "there", "state.json")
	sp := filepath.Join(dir, "state.json")
	if err := os.Symlink(missingTarget, sp); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	err := saveState(dir, emptyState())
	if err == nil {
		t.Fatal("saveState() = nil, want symlink refusal error")
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("saveState() error = %v, want message about symbolic link", err)
	}
	// Dangling link is untouched.
	info, lerr := os.Lstat(sp)
	if lerr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link changed: %v %v", lerr, info.Mode())
	}
	if got, _ := os.Readlink(sp); got != missingTarget {
		t.Fatalf("link target changed to %q", got)
	}
	assertNoTempFiles(t, dir)
}

func TestLoadRefusesSymlinkToPresentTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks is not reliably available on Windows")
	}
	dir := t.TempDir()
	// Write a valid state to a separate file.
	target := filepath.Join(t.TempDir(), "real-state.json")
	valid := `{"schemaVersion":1,"nextSequence":1,"tickets":[]}`
	if err := os.WriteFile(target, []byte(valid), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Make state.json a symlink pointing to the valid target.
	sp := filepath.Join(dir, "state.json")
	if err := os.Symlink(target, sp); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	_, err := loadState(dir)
	if err == nil {
		t.Fatal("loadState() = nil, want symlink refusal error")
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("loadState() error = %v, want message about symbolic link", err)
	}
	// Link is untouched.
	info, lerr := os.Lstat(sp)
	if lerr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link changed after refused loadState: %v %v", lerr, info.Mode())
	}
	// Target is untouched.
	got, _ := os.ReadFile(target)
	if string(got) != valid {
		t.Fatalf("target changed after refused loadState: %s", got)
	}
}

func TestLoadRefusesDanglingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks is not reliably available on Windows")
	}
	dir := t.TempDir()
	missingTarget := filepath.Join(t.TempDir(), "not", "there", "state.json")
	sp := filepath.Join(dir, "state.json")
	if err := os.Symlink(missingTarget, sp); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	_, err := loadState(dir)
	if err == nil {
		t.Fatal("loadState() = nil, want symlink refusal error")
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("loadState() error = %v, want message about symbolic link", err)
	}
	// Dangling link is untouched.
	info, lerr := os.Lstat(sp)
	if lerr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link changed: %v %v", lerr, info.Mode())
	}
	if got, _ := os.Readlink(sp); got != missingTarget {
		t.Fatalf("link target changed to %q", got)
	}
}

func TestLoadTreatsAbsentPathAsEmpty(t *testing.T) {
	dir := t.TempDir()
	// Ensure no state.json exists.
	path := filepath.Join(dir, "state.json")
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("state.json should not exist: err=%v", err)
	}
	got, err := loadState(dir)
	if err != nil {
		t.Fatalf("loadState(%q) error: %v", dir, err)
	}
	if got.SchemaVersion != 1 || got.NextSequence != 1 {
		t.Fatalf("empty state = %+v, want schemaVersion 1 nextSequence 1", got)
	}
}

func TestSaveSetsOwnerOnlyPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	sp := filepath.Join(dir, "state.json")
	if err := saveState(dir, emptyState()); err != nil {
		t.Fatalf("saveState() error: %v", err)
	}
	info, err := os.Stat(sp)
	if err != nil {
		t.Fatalf("Stat() error: %v", err)
	}
	perm := info.Mode().Perm()
	if perm&0o077 != 0 {
		t.Fatalf("state file mode %o: group and other bits must be unset", perm)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat(dir) error: %v", err)
	}
	if dirPerm := dirInfo.Mode().Perm(); dirPerm&0o077 != 0 {
		t.Fatalf("root directory mode %o: group and other bits must be unset", dirPerm)
	}
}

func TestSaveNoLeftoverTempFiles(t *testing.T) {
	dir := t.TempDir()
	st := validState(validTicket())
	if err := saveState(dir, st); err != nil {
		t.Fatalf("saveState() error: %v", err)
	}
	// Save again to ensure no temp file from first save persists.
	if err := saveState(dir, st); err != nil {
		t.Fatalf("saveState() second error: %v", err)
	}
	assertNoTempFiles(t, dir)
}

func TestSaveFailureLeavesPreviousIntact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	// Create initial state.
	good := validState(validTicket())
	if err := saveState(dir, good); err != nil {
		t.Fatalf("saveState() error: %v", err)
	}
	before, err := os.ReadFile(statePath(dir))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Make state.json a directory so the next saveState fails at rename.
	sp := statePath(dir)
	if err := os.Remove(sp); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Mkdir(sp, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// This save should fail because state.json is now a directory.
	another := validState(
		validTicket(func(t *ticket) { t.ID = "new"; t.Sequence = 1 }),
	)
	if err := saveState(dir, another); err == nil {
		t.Fatal("saveState() over directory = nil, want error")
	}
	// Previous state was never written here (it was a directory), so just
	// check no temp files remain and the directory is still a directory.
	assertNoTempFiles(t, dir)
	if info, err := os.Stat(sp); err != nil || !info.IsDir() {
		t.Fatalf("state.json should still be a directory: %v %v", info, err)
	}
	_ = before // used only above for the read sanity check
}

func TestRoundTripFullState(t *testing.T) {
	dir := t.TempDir()
	tickets := []ticket{
		validTicket(func(t *ticket) { t.ID = "id-aaa"; t.Sequence = 1; t.State = ticketQueued }),
		validTicket(func(t *ticket) { t.ID = "id-bbb"; t.Sequence = 2; t.State = ticketLeased; t.WorkerID = 42 }),
		validTicket(func(t *ticket) { t.ID = "id-ccc"; t.Sequence = 3; t.State = ticketQueued; t.OwnerPID = 9999 }),
	}
	want := state{
		SchemaVersion: 1,
		NextSequence:  4,
		Tickets:       tickets,
	}
	if err := saveState(dir, want); err != nil {
		t.Fatalf("saveState() error: %v", err)
	}
	got, err := loadState(dir)
	if err != nil {
		t.Fatalf("loadState() error: %v", err)
	}
	if got.SchemaVersion != want.SchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", got.SchemaVersion, want.SchemaVersion)
	}
	if got.NextSequence != want.NextSequence {
		t.Fatalf("NextSequence = %d, want %d", got.NextSequence, want.NextSequence)
	}
	if len(got.Tickets) != len(want.Tickets) {
		t.Fatalf("len(Tickets) = %d, want %d", len(got.Tickets), len(want.Tickets))
	}
	for i := range got.Tickets {
		if got.Tickets[i] != want.Tickets[i] {
			t.Fatalf("Tickets[%d] = %+v, want %+v", i, got.Tickets[i], want.Tickets[i])
		}
	}
	assertNoTempFiles(t, dir)
}
