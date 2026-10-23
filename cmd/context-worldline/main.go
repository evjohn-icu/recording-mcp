// Command context-worldline prepares local capture times and stitched PCM in
// one authenticated local profile. It never writes to the source library.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"recordingmcp/internal/profile"
)

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "context-worldline:", err)
		os.Exit(2)
	}
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("context-worldline", flag.ContinueOnError)
	fs.SetOutput(stderr)
	prepare := fs.Bool("prepare", false, "prepare recording times and local PCM in the selected profile")
	profileName := fs.String("profile", "", "authenticated local profile name")
	profilesRoot := fs.String("profiles-root", "data/profiles", "trusted profile root")
	passwordEnv := fs.String("profile-password-env", profile.DefaultPasswordEnv, "environment variable holding profile password")
	libraryDir := fs.String("library-dir", "", "local library directory containing <stem>/run.json")
	audioRoot := fs.String("audio-root", "", "optional original-audio directory for hash and mtime verification")
	timezone := fs.String("timezone", "Asia/Shanghai", "timezone used to interpret filename timestamps")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if !*prepare {
		return errors.New("--prepare is required")
	}
	if strings.TrimSpace(*profileName) == "" {
		return errors.New("--profile is required")
	}
	if strings.TrimSpace(*libraryDir) == "" {
		return errors.New("--library-dir is required")
	}
	password := getenv(*passwordEnv)
	if password == "" {
		return fmt.Errorf("environment variable %s is required", *passwordEnv)
	}
	manager, err := profile.Open(*profilesRoot)
	if err != nil {
		return err
	}
	report, err := manager.PrepareWorldline(context.Background(), *profileName, password, profile.WorldlinePrepareOptions{LibraryDir: *libraryDir, AudioRoot: *audioRoot, Timezone: *timezone})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "recordings=%d times_set=%d audio_ready=%d audio_unavailable=%d issues=%d manifest=%s\n", report.Recordings, report.TimesSet, report.AudioReady, report.Unavailable, report.Issues, report.Manifest)
	return err
}
