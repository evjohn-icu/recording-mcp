package profile

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestProfilePathsCredentialsAndPasswordChange(t *testing.T) {
	manager, err := Open(filepath.Join(t.TempDir(), "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	paths, err := manager.Create("alice", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if paths.Database != filepath.Join(paths.Root, "db", "context-memory.db") || paths.Audio != filepath.Join(paths.Root, "audio") || paths.Tasks != filepath.Join(paths.Root, "tasks") {
		t.Fatalf("unexpected profile paths: %+v", paths)
	}
	if !manager.Verify("alice", "correct horse battery") || manager.Verify("alice", "wrong password") {
		t.Fatal("profile password verification did not distinguish valid and invalid passwords")
	}
	if mode := fileMode(t, filepath.Join(paths.Root, "profile.json")); mode.Perm() != 0600 {
		t.Fatalf("credential permissions = %o, want 600", mode.Perm())
	}
	names, err := manager.List()
	if err != nil || !reflect.DeepEqual(names, []string{"alice"}) {
		t.Fatalf("List() = %v, %v", names, err)
	}
	if err := manager.ChangePassword("alice", "incorrect password", "a much newer password"); err == nil {
		t.Fatal("password change accepted a wrong current password")
	}
	if err := manager.ChangePassword("alice", "correct horse battery", "a much newer password"); err != nil {
		t.Fatal(err)
	}
	if manager.Verify("alice", "correct horse battery") || !manager.Verify("alice", "a much newer password") {
		t.Fatal("password change did not replace the verifier")
	}
}

func TestProfileRejectsTraversalAndSymlinkedDataDirectory(t *testing.T) {
	manager, err := Open(filepath.Join(t.TempDir(), "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create("../escape", "correct horse battery"); err == nil {
		t.Fatal("accepted profile traversal")
	}
	paths, err := manager.Create("bob", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(paths.Audio); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, paths.Audio); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := manager.Resolve("bob"); err == nil {
		t.Fatal("accepted a symlinked profile data directory")
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}

func TestRetiredProfileCannotBeResolvedOrRecreated(t *testing.T) {
	root := filepath.Join(t.TempDir(), "profiles")
	manager, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := manager.Create("retired", "long secure password")
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.Retire("retired"); err != nil {
		t.Fatal(err)
	}
	ret := manager.IsRetired("retired")
	normal := manager.Verify("retired", "long secure password")
	purge := manager.VerifyForRetirement("retired", "long secure password")
	if !ret || normal || !purge {
		t.Fatalf("retirement auth state retired=%v normal=%v purge=%v", ret, normal, purge)
	}
	if _, err = manager.Resolve("retired"); err == nil {
		t.Fatal("retired profile resolved for ordinary use")
	}
	names, err := manager.List()
	if err != nil || len(names) != 0 {
		t.Fatalf("retired profile listed: %v %v", names, err)
	}
	if err = os.RemoveAll(paths.Root); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Create("retired", "long secure password"); err == nil {
		t.Fatal("retired profile name was recreated")
	}
	next, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = next.OpenProfile("retired", "long secure password"); err == nil {
		t.Fatal("retired profile was recreated after manager restart")
	}
}
