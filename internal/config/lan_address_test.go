package config

// Regression: no real RFC1918 LAN address may ever be committed to this
// package's (or the wider module's) Go source again.
//
// This repo is published publicly. Every host/address the Broker talks to
// (OLLAMA_URL, UPSTREAM_URL, PLEX_URL, BROKER_ROUTE_<N>_URL, ...) is already
// env-driven with a loopback default (see Load() in config.go) — that part
// was correct. The actual defect was operational: test fixtures in this
// package's _test.go file, plus several .claude/skills/ operator docs, had
// the author's real home-desktop LAN address hardcoded as example/test
// data. That's a topology disclosure, not a code bug, but it's exactly the
// kind of thing that silently creeps back in via copy-pasted test fixtures
// or "just use my real box as the example" docs edits. This test makes that
// mechanically impossible to reintroduce in .go source without CI catching
// it, the same way ollama-client's TestHostIsEnvDriven does for that repo.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// rfc1918Address matches a literal RFC1918 private-range IPv4 address
// (the full 10-slash-8 block, the 172.16-through-172.31 slash-12 block, and
// the 192.168-slash-16 block) anywhere in a line. It deliberately does NOT
// match RFC 5737 documentation/example addresses (the 192.0.2, 198.51.100,
// and 203.0.113 slash-24 blocks) or loopback (127-slash-8) — those are the
// correct, safe-to-commit choices for test fixtures and doc examples, and
// are used throughout this package's own tests.
var rfc1918Address = regexp.MustCompile(
	`\b(?:10(?:\.\d{1,3}){3}|172\.(?:1[6-9]|2\d|3[01])(?:\.\d{1,3}){2}|192\.168(?:\.\d{1,3}){2})\b`,
)

// selfPath is this file's own path, computed once via the compiler-recorded
// caller location rather than a hardcoded name string, so the walk below
// can skip it: this file's comments and this very regex necessarily talk
// about what an RFC1918 address looks like, which the test must not flag.
var selfPath = func() string {
	_, file, _, _ := runtime.Caller(0)
	return file
}()

// TestNoRFC1918AddressInModuleSource walks every .go file in the module
// (both this package and every sibling internal/cmd package) and fails if
// any line contains a literal RFC1918 address. It is not scoped to just
// internal/config because the same class of mistake (pasting a real LAN IP
// into a test fixture or a doc comment) could happen in any package.
func TestNoRFC1918AddressInModuleSource(t *testing.T) {
	root := moduleRoot(t)

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return skipNonSourceDir(path, info.Name())
		}
		if !strings.HasSuffix(path, ".go") || isSelf(path) {
			return nil
		}
		found, scanErr := rfc1918Lines(root, path)
		offenders = append(offenders, found...)
		return scanErr
	})
	if err != nil {
		t.Fatalf("walking module source: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("hardcoded RFC1918 LAN address found in module .go source "+
			"(this repo is public — use an RFC 5737 example address like "+
			"192.0.2.10, or loopback 127.0.0.1, instead):\n%s",
			strings.Join(offenders, "\n"))
	}
}

// skipNonSourceDir returns filepath.SkipDir for directories that are not this
// module's source. .git: not source. legacy/: pre-scrubbed historical Bash-era
// docs, already audited clean, and contains no .go files. .claude/worktrees: a
// *separate* git worktree checkout (its own branch, its own history) living
// under this repo's working directory.
func skipNonSourceDir(path, name string) error {
	switch name {
	case ".git", "legacy", "vendor":
		return filepath.SkipDir
	}
	if strings.HasSuffix(path, filepath.Join(".claude", "worktrees")) {
		return filepath.SkipDir
	}
	return nil
}

// isSelf reports whether path is this file, which is exempt: see selfPath's
// doc comment above.
func isSelf(path string) bool {
	abs, err := filepath.Abs(path)
	return err == nil && abs == selfPath
}

// rfc1918Lines returns "file:line: text" for each line of path that holds an
// RFC1918 address, with file relative to root.
func rfc1918Lines(root, path string) ([]string, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("rfc1918Lines: read file: %w", err)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	var found []string
	for i, line := range strings.Split(string(data), "\n") {
		if rfc1918Address.MatchString(line) {
			found = append(found, rel+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
		}
	}
	return found, nil
}

// moduleRoot locates the repo root (the directory containing go.mod) by
// walking up from the current package directory, so the test works
// regardless of the working directory `go test` is invoked from.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate go.mod above %s", dir)
		}
		dir = parent
	}
}
