// Command context-backup takes consistent stop-write backups of a profiles
// root, restores them into an empty destination and verifies a restored root.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"recordingmcp/internal/backup"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		writeFailure(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: context-backup backup|restore|verify [flags]")
	}
	command := args[0]
	fs := flag.NewFlagSet("context-backup "+command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	profilesRoot := fs.String("profiles-root", "", "trusted local profiles root")
	out := fs.String("out", "", "new archive path (backup)")
	archive := fs.String("archive", "", "archive to restore (restore)")
	dest := fs.String("dest", "", "empty destination root (restore)")
	barrierSource := fs.String("barrier-source", "", "trusted, stopped live profiles root used to verify the forget barrier (restore)")
	confirm := fs.Bool("confirm-writers-stopped", false, "operator assertion that every writer is stopped")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	switch command {
	case "backup":
		if strings.TrimSpace(*profilesRoot) == "" || strings.TrimSpace(*out) == "" {
			return errors.New("--profiles-root and --out are required")
		}
		manifest, err := backup.Create(ctx, backup.CreateOptions{
			ProfilesRoot:          *profilesRoot,
			Out:                   *out,
			ConfirmWritersStopped: *confirm,
		})
		if err != nil {
			return err
		}
		return writeJSON(stdout, manifest)
	case "restore":
		if strings.TrimSpace(*archive) == "" || strings.TrimSpace(*dest) == "" || strings.TrimSpace(*barrierSource) == "" {
			return errors.New("--archive, --dest and --barrier-source are required")
		}
		manifest, err := backup.Restore(ctx, backup.RestoreOptions{
			Archive:               *archive,
			Dest:                  *dest,
			BarrierSource:         *barrierSource,
			ConfirmWritersStopped: *confirm,
		})
		if err != nil {
			return err
		}
		return writeJSON(stdout, manifest)
	case "verify":
		if strings.TrimSpace(*profilesRoot) == "" {
			return errors.New("--profiles-root is required")
		}
		report, err := backup.Verify(ctx, backup.VerifyOptions{ProfilesRoot: *profilesRoot})
		if err != nil {
			return err
		}
		return writeJSON(stdout, report)
	default:
		return fmt.Errorf("unknown command %q; want backup, restore or verify", command)
	}
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", " ")
	return encoder.Encode(value)
}

// writeFailure emits a structured error so a script can branch on the code
// without parsing prose.
func writeFailure(w io.Writer, err error) {
	payload := struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}{Code: backup.ErrorCode(err), Detail: err.Error()}
	if payload.Code == "" {
		payload.Code = "error"
	}
	_ = writeJSON(w, struct {
		Error any `json:"error"`
	}{Error: payload})
}
