package broker_test

import (
	"os/exec"
	"testing"
)

// The story-runner commits every file /spec-gather writes under docs/<slug>/,
// including .ctx.md and TASKS.md. A .gitignore line for either made the commit
// fail ("paths are ignored by one of your .gitignore files").
func TestSpecFolderFilesAreNotGitignored(t *testing.T) {
	for _, path := range []string{
		"docs/some-feature/requirements.md",
		"docs/some-feature/plan.md",
		"docs/some-feature/steps.md",
		"docs/some-feature/.ctx.md",
		"docs/some-feature/TASKS.md",
	} {
		// git check-ignore exits 0 when the path IS ignored, 1 when it is not.
		err := exec.Command("git", "-C", "..", "check-ignore", "-q", "--no-index", path).Run()
		if err == nil {
			t.Errorf("%s is gitignored; the runner cannot commit it", path)
		}
	}
}
