package run

import (
	"context"
	"strings"
	"testing"
)

const siblingNotePrefix = "Another worker is running in this workspace at the same time as you.\n" +
	"Its task declared these paths: "

func expectedSiblingPrompt(prompt string, paths []string) string {
	return prompt + "\n\n" + siblingNotePrefix + strings.Join(paths, ", ") + "\n" +
		"Files there may change while you work. Those changes are that worker's,\n" +
		"made during this run — not the state the workspace was in when you\n" +
		"started, and not a consequence of your own edits."
}

func promptsForSiblingTest(t *testing.T, tasks []Task) []string {
	t.Helper()
	worker := newScriptedWorker()
	_, err := New(worker).Run(context.Background(), Request{
		Tasks:     tasks,
		Workspace: "/workspace",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	prompts := make([]string, len(tasks))
	for i := range tasks {
		prompts[i] = worker.promptForWorker(i + 1)
	}
	return prompts
}

func TestSiblingNotesNameOnlyTheOtherTasksPaths(t *testing.T) {
	tasks := validRequest("task one", "task two").Tasks
	tasks[0].Writes = declaredPaths("one/first.go", "one/second.go")
	tasks[1].Writes = declaredPaths("two/first.go", "two/second.go")

	prompts := promptsForSiblingTest(t, tasks)
	want := []string{
		expectedSiblingPrompt(tasks[0].Prompt, tasks[1].Writes.Paths),
		expectedSiblingPrompt(tasks[1].Prompt, tasks[0].Writes.Paths),
	}
	for i, prompt := range prompts {
		if prompt != want[i] {
			t.Fatalf("task %d prompt = %q, want %q", i+1, prompt, want[i])
		}
		for _, ownPath := range tasks[i].Writes.Paths {
			if strings.Contains(prompt, ownPath) {
				t.Fatalf("task %d prompt contains its own declared path %q: %q", i+1, ownPath, prompt)
			}
		}
	}
}

func TestSiblingNotesAbsentWhenNeitherTaskDeclares(t *testing.T) {
	tasks := validRequest("task one", "task two").Tasks
	prompts := promptsForSiblingTest(t, tasks)
	for i, prompt := range prompts {
		if prompt != tasks[i].Prompt {
			t.Fatalf("task %d prompt = %q, want the task text byte-identical", i+1, prompt)
		}
	}
}

func TestSiblingNotesSkipDeclaredEmptyTask(t *testing.T) {
	tasks := validRequest("task with paths", "task without paths").Tasks
	tasks[0].Writes = declaredPaths("src/one.go", "src/two.go")
	tasks[1].Writes = declaredPaths()

	prompts := promptsForSiblingTest(t, tasks)
	if prompts[0] != tasks[0].Prompt {
		t.Fatalf("task 1 prompt = %q, want no note because its only sibling declared no paths", prompts[0])
	}
	want := expectedSiblingPrompt(tasks[1].Prompt, tasks[0].Writes.Paths)
	if prompts[1] != want {
		t.Fatalf("task 2 prompt = %q, want %q", prompts[1], want)
	}
}

func TestSiblingNotesAbsentForSingleDeclaredTask(t *testing.T) {
	tasks := validRequest("one task").Tasks
	tasks[0].Writes = declaredPaths("only/this.go")

	prompts := promptsForSiblingTest(t, tasks)
	if prompts[0] != tasks[0].Prompt {
		t.Fatalf("prompt = %q, want the task text byte-identical", prompts[0])
	}
}
