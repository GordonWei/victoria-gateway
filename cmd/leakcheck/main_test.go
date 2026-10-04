package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func fakeRepo(files map[string]string) (fileLister, fileReader, *bool) {
	var sawStaged bool
	list := func(staged bool) ([]string, error) {
		sawStaged = staged
		var ps []string
		for p := range files {
			ps = append(ps, p)
		}
		return ps, nil
	}
	read := func(_ bool, p string) ([]byte, error) { return []byte(files[p]), nil }
	return list, read, &sawStaged
}

func TestRun_FailsOnDeliberateViolation(t *testing.T) {
	list, read, sawStaged := fakeRepo(map[string]string{
		"README.md":          "clean text 192.0.2.6",
		"deploy/x.yaml":      "url: http://192" + ".168.1.20:8090",
		"docs/_draft_foo.md": "notes",
	})
	findings, err := run(true, list, read)
	if err != nil {
		t.Fatal(err)
	}
	if !*sawStaged {
		t.Error("--staged was not passed to the lister")
	}
	if len(findings) != 2 {
		t.Fatalf("findings = %+v, want 2 (IP + docs note)", findings)
	}
	var out bytes.Buffer
	if code := report(&out, findings); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "deploy/x.yaml:1:") || !strings.Contains(out.String(), "docs/_draft_foo.md:") {
		t.Errorf("report = %q", out.String())
	}
}

func TestRun_CleanRepo(t *testing.T) {
	list, read, _ := fakeRepo(map[string]string{"a.go": "package a // 203.0.113.5"})
	findings, err := run(false, list, read)
	if err != nil || len(findings) != 0 {
		t.Fatalf("findings = %+v, err = %v", findings, err)
	}
	var out bytes.Buffer
	if code := report(&out, nil); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestRun_PropagatesErrors(t *testing.T) {
	boom := errors.New("boom")
	if _, err := run(false, func(bool) ([]string, error) { return nil, boom }, nil); !errors.Is(err, boom) {
		t.Errorf("list error = %v", err)
	}
	list := func(bool) ([]string, error) { return []string{"x"}, nil }
	read := func(bool, string) ([]byte, error) { return nil, boom }
	if _, err := run(false, list, read); !errors.Is(err, boom) {
		t.Errorf("read error = %v", err)
	}
}
