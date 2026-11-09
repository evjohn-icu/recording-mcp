package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateVerifyList(t *testing.T) {
	root := filepath.Join(t.TempDir(), "profiles")
	env := map[string]string{"MEMORY_PROFILE_PASSWORD": "correct-horse-battery-staple"}
	getenv := func(key string) string { return env[key] }
	var stdout, stderr bytes.Buffer

	if err := run([]string{"create", "--profiles-root", root, "--name", "alpha"}, getenv, &stdout, &stderr); err != nil {
		t.Fatalf("create: %v (%s)", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `Created profile "alpha"`) {
		t.Fatalf("create output: %s", stdout.String())
	}

	stdout.Reset()
	if err := run([]string{"verify", "--profiles-root", root, "--name", "alpha"}, getenv, &stdout, &stderr); err != nil {
		t.Fatalf("verify with correct password: %v", err)
	}
	if !strings.Contains(stdout.String(), `Profile "alpha" verified`) {
		t.Fatalf("verify output: %s", stdout.String())
	}

	env["MEMORY_PROFILE_PASSWORD"] = "wrong-password"
	if err := run([]string{"verify", "--profiles-root", root, "--name", "alpha"}, getenv, &stdout, &stderr); err == nil {
		t.Fatal("verify accepted a wrong password")
	}

	env["MEMORY_PROFILE_PASSWORD"] = "correct-horse-battery-staple"
	if err := run([]string{"create", "--profiles-root", root, "--name", "alpha"}, getenv, &stdout, &stderr); err == nil {
		t.Fatal("create replaced an existing profile")
	}

	stdout.Reset()
	if err := run([]string{"list", "--profiles-root", root}, getenv, &stdout, &stderr); err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "alpha" {
		t.Fatalf("list output: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "alpha", "profile.json")); err != nil {
		t.Fatalf("credentials missing: %v", err)
	}
}
