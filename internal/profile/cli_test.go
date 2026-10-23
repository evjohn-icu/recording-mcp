package profile

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIResolvesVerifiedProfileAndKeepsLegacyPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "profiles")
	m, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	want, err := m.Create("alice", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLI_PROFILE_TEST_PASSWORD", "correct horse battery staple")
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cli := RegisterCLI(fs)
	if err := fs.Parse([]string{"--profile=alice", "--profiles-root=" + root, "--profile-password-env=CLI_PROFILE_TEST_PASSWORD"}); err != nil {
		t.Fatal(err)
	}
	db, audio, err := cli.Resolve("legacy.db", "legacy-audio")
	if err != nil {
		t.Fatal(err)
	}
	if db != want.Database || audio != want.Audio {
		t.Fatalf("got (%q, %q), want (%q, %q)", db, audio, want.Database, want.Audio)
	}

	legacyFS := flag.NewFlagSet("legacy", flag.ContinueOnError)
	legacyCLI := RegisterCLI(legacyFS)
	if err := legacyFS.Parse(nil); err != nil {
		t.Fatal(err)
	}
	db, audio, err = legacyCLI.Resolve("legacy.db", "legacy-audio")
	if err != nil || db != "legacy.db" || audio != "legacy-audio" {
		t.Fatalf("legacy resolve = (%q, %q, %v)", db, audio, err)
	}
}

func TestCLIRejectsWrongPasswordAndExplicitPathOverrides(t *testing.T) {
	root := filepath.Join(t.TempDir(), "profiles")
	m, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create("alice", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLI_PROFILE_TEST_PASSWORD", "incorrect password")
	makeCLI := func(args ...string) (*CLI, error) {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.String("db", "", "database")
		fs.String("audio-dir", "", "audio directory")
		c := RegisterCLI(fs)
		args = append(args, "--profile=alice", "--profiles-root="+root, "--profile-password-env=CLI_PROFILE_TEST_PASSWORD")
		return c, fs.Parse(args)
	}
	cli, err := makeCLI()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = cli.Resolve("", "")
	if err == nil || !strings.Contains(err.Error(), "invalid password") {
		t.Fatalf("wrong password error = %v", err)
	}

	t.Setenv("CLI_PROFILE_TEST_PASSWORD", "correct horse battery staple")
	for _, test := range []struct {
		args        []string
		path, audio string
		expected    string
	}{
		{[]string{"--db=override.db"}, "override.db", "", "--db"},
		{[]string{"--audio-dir=override-audio"}, "", "override-audio", "--audio-dir"},
	} {
		cli, err := makeCLI(test.args...)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = cli.Resolve(test.path, test.audio)
		if err == nil || !strings.Contains(err.Error(), test.expected) {
			t.Errorf("args %v error = %v", test.args, err)
		}
	}
}

func TestCLIDoesNotCreateMissingProfile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "profiles")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLI_PROFILE_TEST_PASSWORD", "correct horse battery staple")
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cli := RegisterCLI(fs)
	if err := fs.Parse([]string{"--profile=missing", "--profiles-root=" + root, "--profile-password-env=CLI_PROFILE_TEST_PASSWORD"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cli.Resolve("", ""); err == nil {
		t.Fatal("expected missing profile error")
	}
	if _, err := os.Stat(filepath.Join(root, "missing")); !os.IsNotExist(err) {
		t.Fatalf("missing profile was created: %v", err)
	}
}

func TestCLIWorldlinePlaneUsesTrustedProfileOrExplicitLegacyPlane(t *testing.T) {
	root := filepath.Join(t.TempDir(), "profiles")
	m, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	want, err := m.Create("alice", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLI_PROFILE_TEST_PASSWORD", "correct horse battery staple")
	fs := flag.NewFlagSet("profile", flag.ContinueOnError)
	cli := RegisterCLI(fs)
	if err := fs.Parse([]string{"--profile=alice", "--profiles-root=" + root, "--profile-password-env=CLI_PROFILE_TEST_PASSWORD"}); err != nil {
		t.Fatal(err)
	}
	scope, path, err := cli.WorldlinePlane("", "")
	if err != nil || scope != "alice" || path != want.Plane {
		t.Fatalf("profile plane=(%q,%q,%v)", scope, path, err)
	}
	legacy := RegisterCLI(flag.NewFlagSet("legacy", flag.ContinueOnError))
	if _, _, err := legacy.WorldlinePlane("memory.db", ""); err == nil {
		t.Fatal("legacy plane should require explicit path")
	}
	scope, path, err = legacy.WorldlinePlane("memory.db", "events.db")
	if err != nil || scope != "local" {
		t.Fatalf("legacy plane=(%q,%q,%v)", scope, path, err)
	}
}
