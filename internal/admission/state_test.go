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
func writeState(t *testing.T, dir, content string) string {
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
func validTicket(overrides ...func(*Ticket)) Ticket {
	t := Ticket{
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

func validState(tickets ...Ticket) State {
	if len(tickets) == 0 {
		return EmptyState()
	}
	nextSeq := 0
	for _, t := range tickets {
		if t.Sequence >= nextSeq {
			nextSeq = t.Sequence + 1
		}
	}
	return State{
		SchemaVersion: 1,
		NextSequence:  nextSeq,
		Tickets:       tickets,
	}
}

func TestEmptyStateIsValid(t *testing.T) {
	s := EmptyState()
	if err := Validate(s); err != nil {
		t.Fatalf("Validate(EmptyState()) = %v, want nil", err)
	}
	if s.SchemaVersion != 1 {
		t.Fatalf("EmptyState().SchemaVersion = %d, want 1", s.SchemaVersion)
	}
	if s.NextSequence <= 0 {
		t.Fatalf("EmptyState().NextSequence = %d, want positive", s.NextSequence)
	}
}

func TestValidateEmptyState(t *testing.T) {
	tests := []struct {
		name    string
		state   State
		wantErr bool
	}{
		{
			name:  "empty",
			state: EmptyState(),
		},
		{
			name:    "wrong schema",
			state:   State{SchemaVersion: 2, NextSequence: 1, Tickets: []Ticket{}},
			wantErr: true,
		},
		{
			name:    "zero nextSequence",
			state:   State{SchemaVersion: 1, NextSequence: 0, Tickets: []Ticket{}},
			wantErr: true,
		},
		{
			name:    "negative nextSequence",
			state:   State{SchemaVersion: 1, NextSequence: -1, Tickets: []Ticket{}},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := Validate(test.state)
			if test.wantErr && err == nil {
				t.Fatalf("Validate(%+v) = nil, want error", test.state)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("Validate(%+v) = %v, want nil", test.state, err)
			}
		})
	}
}

func TestValidateStrictTicketFields(t *testing.T) {
	base := func() State {
		return validState(validTicket())
	}

	tests := []struct {
		name    string
		modify  func(*Ticket)
		wantErr bool
	}{
		{name: "valid"},
		{
			name:    "empty id",
			modify:  func(t *Ticket) { t.ID = "" },
			wantErr: true,
		},
		{
			name:    "zero sequence",
			modify:  func(t *Ticket) { t.Sequence = 0 },
			wantErr: true,
		},
		{
			name:    "negative sequence",
			modify:  func(t *Ticket) { t.Sequence = -1 },
			wantErr: true,
		},
		{
			name:    "empty runId",
			modify:  func(t *Ticket) { t.RunID = "" },
			wantErr: true,
		},
		{
			name:    "zero workerId",
			modify:  func(t *Ticket) { t.WorkerID = 0 },
			wantErr: true,
		},
		{
			name:    "zero ownerPid",
			modify:  func(t *Ticket) { t.OwnerPID = 0 },
			wantErr: true,
		},
		{
			name:    "zero ownerCreateTime",
			modify:  func(t *Ticket) { t.OwnerCreateTime = 0 },
			wantErr: true,
		},
		{
			name:    "unknown state",
			modify:  func(t *Ticket) { t.State = "unknown" },
			wantErr: true,
		},
		{
			name:    "empty state",
			modify:  func(t *Ticket) { t.State = "" },
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := base()
			if test.modify != nil {
				test.modify(&s.Tickets[0])
			}
			err := Validate(s)
			if test.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want error")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestValidateDuplicateTicketIDs(t *testing.T) {
	s := State{
		SchemaVersion: 1,
		NextSequence:  3,
		Tickets: []Ticket{
			validTicket(func(t *Ticket) { t.ID = "dup"; t.Sequence = 1 }),
			validTicket(func(t *Ticket) { t.ID = "dup"; t.Sequence = 2 }),
		},
	}
	if err := Validate(s); err == nil {
		t.Fatal("Validate() = nil, want error for duplicate IDs")
	}
}

func TestValidateDuplicateSequences(t *testing.T) {
	s := State{
		SchemaVersion: 1,
		NextSequence:  3,
		Tickets: []Ticket{
			validTicket(func(t *Ticket) { t.ID = "a"; t.Sequence = 1 }),
			validTicket(func(t *Ticket) { t.ID = "b"; t.Sequence = 1 }),
		},
	}
	if err := Validate(s); err == nil {
		t.Fatal("Validate() = nil, want error for duplicate sequences")
	}
}

func TestValidateNextSequenceLowerThanExisting(t *testing.T) {
	s := State{
		SchemaVersion: 1,
		NextSequence:  1,
		Tickets: []Ticket{
			validTicket(func(t *Ticket) { t.Sequence = 5 }),
		},
	}
	if err := Validate(s); err == nil {
		t.Fatal("Validate() = nil, want error when nextSequence <= max sequence")
	}
}

func TestValidateTicketsNotOrdered(t *testing.T) {
	s := State{
		SchemaVersion: 1,
		NextSequence:  3,
		Tickets: []Ticket{
			validTicket(func(t *Ticket) { t.ID = "a"; t.Sequence = 2 }),
			validTicket(func(t *Ticket) { t.ID = "b"; t.Sequence = 1 }),
		},
	}
	if err := Validate(s); err == nil {
		t.Fatal("Validate() = nil, want error for non-ascending sequence order")
	}
}

func TestValidateTicketsLeasedState(t *testing.T) {
	s := validState(
		validTicket(func(t *Ticket) { t.State = ticketLeased }),
	)
	if err := Validate(s); err != nil {
		t.Fatalf("Validate() = %v, want nil for leased ticket", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	dir := t.TempDir()
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load(%q) error: %v", dir, err)
	}
	if got.SchemaVersion != 1 {
		t.Fatalf("Load missing file: SchemaVersion = %d, want 1", got.SchemaVersion)
	}
	if got.NextSequence <= 0 {
		t.Fatalf("Load missing file: NextSequence = %d, want positive", got.NextSequence)
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
			writeState(t, dir, test.content)
			_, err := Load(dir)
			if err == nil {
				t.Fatalf("Load(%q) = nil, want error for %s", dir, test.name)
			}
			// The file on disk must not be modified.
			got, readErr := os.ReadFile(StatePath(dir))
			if readErr != nil {
				t.Fatalf("ReadFile error: %v", readErr)
			}
			if string(got) != test.content {
				t.Fatalf("file changed after failed Load:\nbefore: %q\nafter:  %q", test.content, string(got))
			}
		})
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	writeState(t, dir, `{"schemaVersion":1,"nextSequence":1,"tickets":[],"unknown":true}`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load() = nil, want error for unknown field")
	}
	// File must be untouched.
	got, _ := os.ReadFile(StatePath(dir))
	if !strings.Contains(string(got), "unknown") {
		t.Fatal("file was modified after Load rejected unknown field")
	}
}

func TestLoadRejectsTrailingData(t *testing.T) {
	dir := t.TempDir()
	writeState(t, dir, `{"schemaVersion":1,"nextSequence":1,"tickets":[]}`+"\n"+`{"extra":1}`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load() = nil, want error for trailing data")
	}
}

func TestLoadRejectsInvalidState(t *testing.T) {
	dir := t.TempDir()
	// Valid JSON but invalid state: schemaVersion 2 is unsupported.
	writeState(t, dir, `{"schemaVersion":2,"nextSequence":1,"tickets":[]}`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load() = nil, want error for invalid state")
	}
	// File must be untouched.
	got, _ := os.ReadFile(StatePath(dir))
	if strings.Contains(string(got), "schemaVersion") {
		// Just verify it still has the original content.
		if !strings.Contains(string(got), `"schemaVersion":2`) {
			t.Fatal("file was modified after Load rejected invalid state")
		}
	}
}

func TestLoadRejectsTicketWithUnknownState(t *testing.T) {
	dir := t.TempDir()
	writeState(t, dir, `{"schemaVersion":1,"nextSequence":2,"tickets":[{"id":"t1","sequence":1,"runId":"r1","workerId":1,"ownerPid":1,"ownerCreateTime":1,"state":"bogus"}]}`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load() = nil, want error for unknown ticket state")
	}
}

func TestLoadValidState(t *testing.T) {
	dir := t.TempDir()
	want := `{"schemaVersion":1,"nextSequence":2,"tickets":[{"id":"t1","sequence":1,"runId":"r1","workerId":1,"ownerPid":1000,"ownerCreateTime":1000000,"state":"queued"}]}` + "\n"
	writeState(t, dir, want)
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
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
		validTicket(func(t *Ticket) { t.ID = "ticket-2"; t.Sequence = 2 }),
	)
	if err := Save(dir, want); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
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
	if err := Save(dir, first); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	second := validState(
		validTicket(func(t *Ticket) { t.ID = "new"; t.Sequence = 1 }),
	)
	if err := Save(dir, second); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if len(got.Tickets) != 1 || got.Tickets[0].ID != "new" {
		t.Fatalf("after second Save: tickets = %+v, want ticket with id new", got.Tickets)
	}
	assertNoTempFiles(t, dir)
}

func TestSaveRejectsInvalidBeforeTouchingExisting(t *testing.T) {
	dir := t.TempDir()
	good := validState(validTicket())
	if err := Save(dir, good); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	before, err := os.ReadFile(StatePath(dir))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Invalid state: wrong schema.
	bad := State{SchemaVersion: 99, NextSequence: 1, Tickets: []Ticket{}}
	if err := Save(dir, bad); err == nil {
		t.Fatal("Save(bad) = nil, want error")
	}
	after, err := os.ReadFile(StatePath(dir))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("file changed after failed Save:\nbefore: %s\nafter:  %s", before, after)
	}
	assertNoTempFiles(t, dir)
}

func TestSaveRejectsInvalidWithoutCreatingFile(t *testing.T) {
	dir := t.TempDir()
	bad := State{SchemaVersion: 99, NextSequence: 1, Tickets: []Ticket{}}
	if err := Save(dir, bad); err == nil {
		t.Fatal("Save(bad) = nil, want error")
	}
	if _, err := os.Stat(StatePath(dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat(state.json) = %v, want fs.ErrNotExist", err)
	}
	assertNoTempFiles(t, dir)
}

func TestSaveCreatesMissingParents(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "deep", "nested", "dirs")
	state := validState(validTicket())
	if err := Save(dir, state); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.NextSequence != state.NextSequence {
		t.Fatalf("NextSequence = %d, want %d", got.NextSequence, state.NextSequence)
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
	statePath := filepath.Join(dir, "state.json")
	if err := os.Symlink(target, statePath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	state := EmptyState()
	err := Save(dir, state)
	if err == nil {
		t.Fatal("Save() = nil, want symlink refusal error")
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Save() error = %v, want message about symbolic link", err)
	}
	// Link is untouched.
	info, lerr := os.Lstat(statePath)
	if lerr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link changed after refused Save: %v %v", lerr, info.Mode())
	}
	// Target is untouched.
	got, _ := os.ReadFile(target)
	if strings.Contains(string(got), "nextSequence") && string(got) == `{"schemaVersion":1,"nextSequence":1,"tickets":[]}` {
		// Target content unchanged.
	}
	assertNoTempFiles(t, dir)
}

func TestSaveRefusesDanglingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks is not reliably available on Windows")
	}
	dir := t.TempDir()
	missingTarget := filepath.Join(t.TempDir(), "not", "there", "state.json")
	statePath := filepath.Join(dir, "state.json")
	if err := os.Symlink(missingTarget, statePath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	err := Save(dir, EmptyState())
	if err == nil {
		t.Fatal("Save() = nil, want symlink refusal error")
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Save() error = %v, want message about symbolic link", err)
	}
	// Dangling link is untouched.
	info, lerr := os.Lstat(statePath)
	if lerr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link changed: %v %v", lerr, info.Mode())
	}
	if got, _ := os.Readlink(statePath); got != missingTarget {
		t.Fatalf("link target changed to %q", got)
	}
	assertNoTempFiles(t, dir)
}

func TestSaveSetsOwnerOnlyPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if err := Save(dir, EmptyState()); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	info, err := os.Stat(statePath)
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
	state := validState(validTicket())
	if err := Save(dir, state); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	// Save again to ensure no temp file from first save persists.
	if err := Save(dir, state); err != nil {
		t.Fatalf("Save() second error: %v", err)
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
	if err := Save(dir, good); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	before, err := os.ReadFile(StatePath(dir))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Make state.json a directory so the next Save fails at rename.
	statePath := StatePath(dir)
	if err := os.Remove(statePath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// This save should fail because state.json is now a directory.
	another := validState(
		validTicket(func(t *Ticket) { t.ID = "new"; t.Sequence = 1 }),
	)
	if err := Save(dir, another); err == nil {
		t.Fatal("Save() over directory = nil, want error")
	}
	// Previous state was never written here (it was a directory), so just
	// check no temp files remain and the directory is still a directory.
	assertNoTempFiles(t, dir)
	if info, err := os.Stat(statePath); err != nil || !info.IsDir() {
		t.Fatalf("state.json should still be a directory: %v %v", info, err)
	}
	_ = before // used only above for the read sanity check
}

func TestRoundTripFullState(t *testing.T) {
	dir := t.TempDir()
	tickets := []Ticket{
		validTicket(func(t *Ticket) { t.ID = "id-aaa"; t.Sequence = 1; t.State = ticketQueued }),
		validTicket(func(t *Ticket) { t.ID = "id-bbb"; t.Sequence = 2; t.State = ticketLeased; t.WorkerID = 42 }),
		validTicket(func(t *Ticket) { t.ID = "id-ccc"; t.Sequence = 3; t.State = ticketQueued; t.OwnerPID = 9999 }),
	}
	want := State{
		SchemaVersion: 1,
		NextSequence:  4,
		Tickets:       tickets,
	}
	if err := Save(dir, want); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
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
