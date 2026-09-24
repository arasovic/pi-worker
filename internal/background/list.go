package background

import (
	"os"
	"sort"
	"time"

	"github.com/arasovic/pi-worker/internal/runlog"
)

// ListRuns returns one entry per background run directory under root,
// newest first — run ids begin with a fixed-width UTC timestamp, so
// sorting the ids as plain strings descending is chronological, exactly
// as runlog.List sorts its own entries. The entries carry the same
// shape runlog.List produces, so runs list can merge the two streams
// into one inventory.
//
// Only directory entries whose name is a valid run id are considered;
// every other entry is skipped silently, the same way runlog.List
// skips every non-record file. A missing root is not an error: there
// are no background runs to list.
//
// A snapshot that cannot be loaded is still listed, carrying what the
// directory name and the fixed snapshot path alone tell: the run id,
// the path, outcome "unknown", and the empty models slice. The load
// failure is not returned — an unreadable entry is an inventory fact,
// not a failure of the listing, exactly as runlog.List treats an
// unreadable record. Only a read error on root itself, other than
// not-exist, is returned.
//
// The outcome of a readable snapshot is decided by the same liveness
// seam runlog.List uses, never a second answer to the same question:
// the snapshot's own outcome when it is terminal and carries one;
// otherwise "running" when the recorded supervisor process is still
// alive, and "interrupted" when it is gone.
func ListRuns(root string) ([]runlog.Run, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []runlog.Run{}, nil
		}
		return nil, err
	}
	store, err := NewStore(root)
	if err != nil {
		return nil, err
	}
	runs := make([]runlog.Run, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() {
			continue
		}
		if _, err := runlog.ParseRunID(name); err != nil {
			continue
		}
		path := snapshotPath(root, name)
		snap, err := store.Load(name)
		if err != nil {
			runs = append(runs, runlog.Run{RunID: name, Models: []string{}, Outcome: "unknown", Path: path})
			continue
		}
		outcome := ""
		switch {
		case snap.Terminal && snap.Outcome != nil:
			outcome = string(*snap.Outcome)
		case runlog.ProcessAlive(snap.Supervisor.PID, snap.Supervisor.CreateTime):
			outcome = "running"
		default:
			outcome = "interrupted"
		}
		runs = append(runs, runlog.Run{
			RunID:     snap.RunID,
			StartedAt: snap.AcceptedAt.UTC().Format(time.RFC3339),
			Workspace: snap.Workspace,
			Tasks:     len(snap.Workers),
			Models:    workerModels(snap.Workers),
			Outcome:   outcome,
			Path:      path,
		})
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].RunID > runs[j].RunID })
	return runs, nil
}

// workerModels returns the distinct models the snapshot's workers named,
// in worker order, without repeats and without empty strings. The
// result is never nil: a snapshot whose workers name no model yields
// the empty slice, so an entry's models field serialises as [] and
// never null.
func workerModels(workers []WorkerSnapshot) []string {
	models := []string{}
	seen := make(map[string]struct{}, len(workers))
	for _, worker := range workers {
		model := worker.Task.Model
		if model == "" {
			continue
		}
		if _, dup := seen[model]; dup {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	return models
}
