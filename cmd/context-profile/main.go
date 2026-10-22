// Command context-profile creates and lists password-protected local profiles.
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
		fmt.Fprintln(os.Stderr, "context-profile:", err)
		os.Exit(2)
	}
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: context-profile create|list|adopt [flags]")
	}
	command := args[0]
	fs := flag.NewFlagSet("context-profile "+command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("profiles-root", "data/profiles", "trusted local profile root")
	fs.StringVar(root, "root", "data/profiles", "deprecated alias for --profiles-root")
	name := fs.String("name", "", "profile name")
	legacyDB := fs.String("db", "", "legacy SQLite database to snapshot read-only")
	audioDir := fs.String("audio-dir", "", "legacy managed audio root to copy")
	eventsDB := fs.String("events-db", "", "optional legacy local event database to snapshot")
	passwordEnv := fs.String("profile-password-env", profile.DefaultPasswordEnv, "environment variable holding the profile password")
	fs.StringVar(passwordEnv, "password-env", profile.DefaultPasswordEnv, "deprecated alias for --profile-password-env")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	manager, err := profile.Open(*root)
	if err != nil {
		return err
	}
	switch command {
	case "create":
		if strings.TrimSpace(*name) == "" {
			return errors.New("--name is required")
		}
		password := getenv(*passwordEnv)
		paths, err := manager.Create(*name, password)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "Created profile %q at %s\n", paths.Name, paths.Root)
		return err
	case "adopt":
		if strings.TrimSpace(*name) == "" || strings.TrimSpace(*legacyDB) == "" {
			return errors.New("adopt requires --name and --db")
		}
		paths, err := manager.AdoptLegacy(context.Background(), *name, getenv(*passwordEnv), *legacyDB, *audioDir, *eventsDB)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "Adopted legacy database into profile %q at %s\n", paths.Name, paths.Root)
		return err
	case "list":
		names, err := manager.List()
		if err != nil {
			return err
		}
		for _, profileName := range names {
			if _, err := fmt.Fprintln(stdout, profileName); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown command %q; use create, list or adopt", command)
	}
}
