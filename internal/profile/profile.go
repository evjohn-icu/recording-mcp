// Package profile manages local, password-protected profiles. A profile keeps
// its database and local data directories beneath one trusted root. Password
// verification protects application access; it does not encrypt these files.
package profile

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024 // KiB
	argonThreads uint8  = 2
	argonKeyLen         = 32
	saltLen             = 16
)

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

type credentials struct {
	Version int    `json:"version"`
	Salt    string `json:"salt"`
	Hash    string `json:"hash"`
}

// Paths are derived from a profile name and never accepted from profile data.
type Paths struct {
	Name     string
	Root     string
	Database string
	Audio    string
	Tasks    string
	Backups  string
	Index    string
	Plane    string
}

// Manager resolves profile directories beneath Root.
type Manager struct {
	Root     string
	verifyMu sync.Mutex
}

const retiredCatalogName = "retired-profiles.json"

// Open creates the profile root if needed and rejects a symlink root.
func Open(root string) (*Manager, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("profile: root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("profile: resolve root: %w", err)
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return nil, fmt.Errorf("profile: create root: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("profile: root must be a real directory")
	}
	if err := os.Chmod(abs, 0700); err != nil {
		return nil, fmt.Errorf("profile: secure root: %w", err)
	}
	return &Manager{Root: abs}, nil
}

// OpenProfile creates a new profile on first use or verifies the password of
// an existing profile. Its filesystem layout is fixed by this package.
func (m *Manager) OpenProfile(name, password string) (Paths, error) {
	if m == nil || m.Root == "" {
		return Paths{}, errors.New("profile: manager is required")
	}
	if !validName.MatchString(name) {
		return Paths{}, errors.New("profile: name must use 1-64 letters, digits, '_' or '-' and start with a letter or digit")
	}
	m.verifyMu.Lock()
	defer m.verifyMu.Unlock()
	if m.IsRetired(name) {
		return Paths{}, errors.New("profile: profile is permanently retired")
	}
	if len(password) < 12 || len(password) > 1024 {
		return Paths{}, errors.New("profile: password must be between 12 and 1024 bytes")
	}
	root := filepath.Join(m.Root, name)
	if err := os.Mkdir(root, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return Paths{}, fmt.Errorf("profile: create profile: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Paths{}, errors.New("profile: profile path must be a real directory")
	}
	if err := os.Chmod(root, 0700); err != nil {
		return Paths{}, fmt.Errorf("profile: secure profile: %w", err)
	}
	credPath := filepath.Join(root, "profile.json")
	if _, err := os.Lstat(credPath); errors.Is(err, os.ErrNotExist) {
		if err := writeCredentials(credPath, password); err != nil {
			return Paths{}, err
		}
	} else if err != nil {
		return Paths{}, fmt.Errorf("profile: inspect credentials: %w", err)
	} else if ok, err := verifyCredentials(credPath, password); err != nil {
		return Paths{}, err
	} else if !ok {
		return Paths{}, errors.New("profile: invalid password")
	}

	return m.paths(name)
}

// Create adds a new profile and refuses to replace an existing profile.
func (m *Manager) Create(name, password string) (Paths, error) {
	if m == nil || m.Root == "" {
		return Paths{}, errors.New("profile: manager is required")
	}
	if !validName.MatchString(name) {
		return Paths{}, errors.New("profile: invalid name")
	}
	m.verifyMu.Lock()
	defer m.verifyMu.Unlock()
	if m.IsRetired(name) {
		return Paths{}, errors.New("profile: profile is permanently retired")
	}
	if err := validatePassword(password); err != nil {
		return Paths{}, err
	}
	root := filepath.Join(m.Root, name)
	if err := os.Mkdir(root, 0700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return Paths{}, errors.New("profile: profile already exists")
		}
		return Paths{}, fmt.Errorf("profile: create profile: %w", err)
	}
	if err := writeCredentials(filepath.Join(root, "profile.json"), password); err != nil {
		_ = os.RemoveAll(root)
		return Paths{}, err
	}
	return m.paths(name)
}

// Resolve returns the fixed paths for an existing profile after checking its
// directory and credential file. It creates missing standard data directories.
func (m *Manager) Resolve(name string) (Paths, error) {
	if m == nil || m.Root == "" || !validName.MatchString(name) {
		return Paths{}, errors.New("profile: invalid profile")
	}
	if m.IsRetired(name) {
		return Paths{}, errors.New("profile: profile is permanently retired")
	}
	root := filepath.Join(m.Root, name)
	info, err := os.Lstat(root)
	if err != nil {
		return Paths{}, fmt.Errorf("profile: resolve profile: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Paths{}, errors.New("profile: profile path must be a real directory")
	}
	credInfo, err := os.Lstat(filepath.Join(root, "profile.json"))
	if err != nil || !credInfo.Mode().IsRegular() || credInfo.Mode()&os.ModeSymlink != 0 {
		return Paths{}, errors.New("profile: profile credentials are unavailable")
	}
	return m.paths(name)
}

// List returns profile names whose directories and credential files are valid.
func (m *Manager) List() ([]string, error) {
	if m == nil || m.Root == "" {
		return nil, errors.New("profile: manager is required")
	}
	entries, err := os.ReadDir(m.Root)
	if err != nil {
		return nil, fmt.Errorf("profile: list profiles: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !validName.MatchString(entry.Name()) {
			continue
		}
		if _, err := m.Resolve(entry.Name()); err == nil {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

func (m *Manager) paths(name string) (Paths, error) {
	root := filepath.Join(m.Root, name)
	p := Paths{
		Name: name, Root: root,
		Database: filepath.Join(root, "db", "context-memory.db"),
		Audio:    filepath.Join(root, "audio"),
		Tasks:    filepath.Join(root, "tasks"),
		Backups:  filepath.Join(root, "backups"),
		Index:    filepath.Join(root, "index"),
		Plane:    filepath.Join(root, "plane", "events.db"),
	}
	for _, dir := range []string{filepath.Dir(p.Database), p.Audio, p.Tasks, p.Backups, p.Index, filepath.Dir(p.Plane)} {
		if err := ensureDirectory(dir); err != nil {
			return Paths{}, fmt.Errorf("profile: create profile data directory: %w", err)
		}
	}
	return p, nil
}

// Verify checks a profile password without opening or creating profile data.
func (m *Manager) Verify(name, password string) bool {
	if m == nil || !validName.MatchString(name) || len(password) > 1024 {
		return false
	}
	m.verifyMu.Lock()
	defer m.verifyMu.Unlock()
	if m.IsRetired(name) {
		return false
	}
	ok, err := verifyCredentials(filepath.Join(m.Root, name, "profile.json"), password)
	return err == nil && ok
}

// ChangePassword verifies the old password and atomically replaces the local
// password verifier. Callers should revoke existing browser sessions after
// success; profile files remain unencrypted.
func (m *Manager) ChangePassword(name, oldPassword, newPassword string) error {
	if m == nil || m.Root == "" || !validName.MatchString(name) {
		return errors.New("profile: invalid profile or new password length")
	}
	if err := validatePassword(newPassword); err != nil {
		return err
	}
	m.verifyMu.Lock()
	defer m.verifyMu.Unlock()
	if m.IsRetired(name) {
		return errors.New("profile: profile is permanently retired")
	}
	path := filepath.Join(m.Root, name, "profile.json")
	ok, err := verifyCredentials(path, oldPassword)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("profile: invalid password")
	}
	tmp := path + ".new"
	_ = os.Remove(tmp)
	if err := writeCredentials(tmp, newPassword); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("profile: replace credentials: %w", err)
	}
	return nil
}

func validatePassword(password string) error {
	if len(password) < 12 || len(password) > 1024 {
		return errors.New("profile: password must be between 12 and 1024 bytes")
	}
	return nil
}

func ensureDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("path must be a real directory")
	}
	return os.Chmod(path, 0700)
}

func writeCredentials(path, password string) error {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("profile: generate salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	body, err := json.Marshal(credentials{Version: 1, Salt: base64.RawStdEncoding.EncodeToString(salt), Hash: base64.RawStdEncoding.EncodeToString(hash)})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("profile: create credentials: %w", err)
	}
	if _, err := f.Write(append(body, '\n')); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("profile: write credentials: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("profile: sync credentials: %w", err)
	}
	return f.Close()
}

func verifyCredentials(path, password string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, fmt.Errorf("profile: read credentials: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("profile: credentials must be a regular file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("profile: read credentials: %w", err)
	}
	var c credentials
	if err := json.Unmarshal(b, &c); err != nil || c.Version != 1 {
		return false, errors.New("profile: invalid credentials file")
	}
	salt, err := base64.RawStdEncoding.DecodeString(c.Salt)
	if err != nil || len(salt) != saltLen {
		return false, errors.New("profile: invalid credentials file")
	}
	want, err := base64.RawStdEncoding.DecodeString(c.Hash)
	if err != nil || len(want) != argonKeyLen {
		return false, errors.New("profile: invalid credentials file")
	}
	got := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// IsRetired reports a durable local deletion decision stored outside the
// profile directory, so deleting/recreating that directory cannot resurrect it.
func (m *Manager) IsRetired(name string) bool {
	if m == nil || m.Root == "" || !validName.MatchString(name) {
		return false
	}
	path := filepath.Join(m.Root, retiredCatalogName)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	var names []string
	if json.Unmarshal(data, &names) != nil {
		return true
	} // fail closed on catalog corruption
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// Retire permanently records a profile name in the trusted root catalog.
func (m *Manager) Retire(name string) error {
	if m == nil || m.Root == "" || !validName.MatchString(name) {
		return errors.New("profile: invalid profile name")
	}
	m.verifyMu.Lock()
	defer m.verifyMu.Unlock()
	path := filepath.Join(m.Root, retiredCatalogName)
	names := []string{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &names); err != nil {
			return errors.New("profile: retired catalog is corrupt; refusing retirement update")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, n := range names {
		if n == name {
			return nil
		}
	}
	names = append(names, name)
	sort.Strings(names)
	body, err := json.Marshal(names)
	if err != nil {
		return err
	}
	tmp := path + ".new"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(body, '\n')); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	dir, err := os.Open(m.Root)
	if err != nil {
		return err
	}
	err = dir.Sync()
	_ = dir.Close()
	return err
}

// ResolveForRetirement returns the same derived paths while allowing cleanup
// retries after the profile has been removed from normal login/listing.
func (m *Manager) ResolveForRetirement(name string) (Paths, error) {
	if m == nil || m.Root == "" || !validName.MatchString(name) {
		return Paths{}, errors.New("profile: invalid profile")
	}
	root := filepath.Join(m.Root, name)
	info, err := os.Lstat(root)
	if err != nil {
		return Paths{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Paths{}, errors.New("profile: invalid profile directory")
	}
	cred, err := os.Lstat(filepath.Join(root, "profile.json"))
	if err != nil || !cred.Mode().IsRegular() || cred.Mode()&os.ModeSymlink != 0 {
		return Paths{}, errors.New("profile: credentials unavailable")
	}
	return m.paths(name)
}

// VerifyForRetirement permits only the password-gated purge/retry operation;
// it never makes a retired profile available for application use.
func (m *Manager) VerifyForRetirement(name, password string) bool {
	if m == nil || !validName.MatchString(name) || len(password) > 1024 {
		return false
	}
	m.verifyMu.Lock()
	defer m.verifyMu.Unlock()
	ok, err := verifyCredentials(filepath.Join(m.Root, name, "profile.json"), password)
	return err == nil && ok
}
