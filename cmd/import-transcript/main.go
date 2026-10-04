// import-transcript imports a provider-neutral, normalized ASR result. It never
// calls a provider or accesses audio.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/memory"
)

func decode(path string, out any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err = d.Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	var extra any
	if err = d.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s contains trailing JSON", path)
	}
	return nil
}

func run(args []string) error {
	fs := flag.NewFlagSet("import-transcript", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.String("db", "data/context-memory.db", "experimental context memory SQLite file")
	fs.String("input", "", "provider-neutral normalized transcript JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	db, input := fs.Lookup("db").Value.String(), fs.Lookup("input").Value.String()
	if input == "" {
		return errors.New("--input normalized.json is required")
	}
	var result asrinput.Result
	if err := decode(input, &result); err != nil {
		return err
	}
	store, err := memory.Open(db)
	if err != nil {
		return err
	}
	defer store.Close()
	report, err := store.IngestTranscript(context.Background(), result)
	if err != nil {
		return err
	}
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	return e.Encode(report)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "import-transcript:", err)
		os.Exit(1)
	}
}
