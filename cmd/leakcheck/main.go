// Command leakcheck fails when a tracked (or staged) file contains
// something that should not be published: a private IPv4 address, a
// home-directory path, a credential-shaped string, or a local working
// note under docs/. See internal/leakcheck for the rules.
//
//	go run ./cmd/leakcheck            # every tracked file (CI)
//	go run ./cmd/leakcheck --staged   # the staged version of staged files (pre-commit)
//
// scripts/pre-commit wires the --staged form into a git hook.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/gordonwei/victoria-gateway/internal/leakcheck"
)

func main() {
	staged := flag.Bool("staged", false, "scan the staged contents of staged files instead of every tracked file")
	flag.Parse()
	findings, err := run(*staged, gitFiles, gitRead)
	if err != nil {
		fmt.Fprintln(os.Stderr, "leakcheck:", err)
		os.Exit(2)
	}
	os.Exit(report(os.Stdout, findings))
}

// fileLister and fileReader are the git touchpoints, swapped out in tests.
type fileLister func(staged bool) ([]string, error)
type fileReader func(staged bool, path string) ([]byte, error)

func run(staged bool, list fileLister, read fileReader) ([]leakcheck.Finding, error) {
	paths, err := list(staged)
	if err != nil {
		return nil, err
	}
	var out []leakcheck.Finding
	for _, p := range paths {
		if f := leakcheck.CheckPath(p); f != nil {
			out = append(out, *f)
		}
		b, err := read(staged, p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		out = append(out, leakcheck.Scan(p, b)...)
	}
	return out, nil
}

// report prints findings and returns the process exit code.
func report(w io.Writer, findings []leakcheck.Finding) int {
	if len(findings) == 0 {
		_, _ = fmt.Fprintln(w, "leakcheck: no findings")
		return 0
	}
	for _, f := range findings {
		if f.Line > 0 {
			_, _ = fmt.Fprintf(w, "%s:%d: %s: %s\n", f.Path, f.Line, f.Rule, f.Match)
		} else {
			_, _ = fmt.Fprintf(w, "%s: %s\n", f.Path, f.Rule)
		}
	}
	_, _ = fmt.Fprintf(w, "leakcheck: %d finding(s). Replace with documentation values (192.0.2.0/24, example.com, REPLACE-...),\n"+
		"or add %q on the line if it is a deliberate fixture.\n", len(findings), leakcheck.AllowMarker)
	return 1
}

func gitFiles(staged bool) ([]string, error) {
	args := []string{"ls-files", "-z"}
	if staged {
		args = []string{"diff", "--cached", "--name-only", "-z", "--diff-filter=ACMR"}
	}
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	var paths []string
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) > 0 {
			paths = append(paths, string(p))
		}
	}
	return paths, nil
}

func gitRead(staged bool, p string) ([]byte, error) {
	if staged {
		return exec.Command("git", "show", ":"+p).Output()
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		// Tracked but deleted in the working tree: nothing to publish.
		return nil, nil
	}
	return b, err
}
