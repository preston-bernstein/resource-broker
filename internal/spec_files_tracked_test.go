package broker_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The story-runner commits every file /spec-gather writes under docs/<slug>/,
// including .ctx.md and TASKS.md. A .gitignore line for either made the commit
// fail ("paths are ignored by one of your .gitignore files").
func TestSpecFolderFilesAreNotGitignored(t *testing.T) {
	paths := []string{
		"docs/some-feature/requirements.md",
		"docs/some-feature/plan.md",
		"docs/some-feature/steps.md",
		"docs/some-feature/.ctx.md",
		"docs/some-feature/TASKS.md",
	}
	// The paths go in on stdin so every command argument stays constant.
	// git check-ignore exits 0 and lists the paths that ARE ignored, and exits
	// 1 when none are.
	cmd := exec.CommandContext(t.Context(), "git", "-C", "..", "check-ignore", "--no-index", "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		return
	}
	for _, path := range strings.Fields(string(out)) {
		t.Errorf("%s is gitignored; the runner cannot commit it", path)
	}
}
