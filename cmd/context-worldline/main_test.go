package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"recordingmcp/internal/profile"
)

func TestRunRequiresPrepareAndProfilePassword(t *testing.T) {
	var out, errOut bytes.Buffer
	getenv := func(string) string { return "correct horse battery staple" }
	if err := run([]string{"--profile=alice"}, getenv, &out, &errOut); err == nil || !strings.Contains(err.Error(), "--prepare") {
		t.Fatalf("missing --prepare not rejected: %v", err)
	}
	root := filepath.Join(t.TempDir(), "profiles")
	manager, err := profile.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Create("alice", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	library := filepath.Join(t.TempDir(), "library")
	if err = os.Mkdir(library, 0700); err != nil {
		t.Fatal(err)
	}
	err = run([]string{"--prepare", "--profile=alice", "--profiles-root=" + root, "--library-dir=" + library}, func(string) string { return "wrong password" }, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "invalid profile or password") {
		t.Fatalf("invalid profile password accepted: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("failed authentication produced output: %s", out.String())
	}
}
