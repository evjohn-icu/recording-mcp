package app

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestEraseAudioPreservesSampleCoordinatesAndRetries(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "recording.pcm")
	original := bytes.Repeat([]byte{7, 9}, 100)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := eraseAudioFile(root, path, 40, 100, false); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(original) || !bytes.Equal(got[:40], original[:40]) || !bytes.Equal(got[100:], original[100:]) || !bytes.Equal(got[40:100], make([]byte, 60)) {
		t.Fatal("silence changed neighboring samples or length")
	}
	if err := eraseAudioFile(root, path, 0, 201, false); err == nil {
		t.Fatal("accepted invalid interval")
	}
	if err := eraseAudioFile(root, path, 0, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := eraseAudioFile(root, path, 0, 0, true); err != nil {
		t.Fatal(err)
	}
}

func TestEraseAudioRejectsExternalAndSymlinkTargets(t *testing.T) {
	root, external := t.TempDir(), filepath.Join(t.TempDir(), "private.pcm")
	if err := os.WriteFile(external, []byte{1, 2}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := eraseAudioFile(root, external, 0, 2, true); err == nil {
		t.Fatal("external path accepted")
	}
	link := filepath.Join(root, "link.pcm")
	if err := os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}
	if err := eraseAudioFile(root, link, 0, 2, true); err == nil {
		t.Fatal("symlink accepted")
	}
	got, _ := os.ReadFile(external)
	if !bytes.Equal(got, []byte{1, 2}) {
		t.Fatal("external bytes changed")
	}
}
