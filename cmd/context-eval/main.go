// Command context-eval runs the deterministic synthetic evaluation harness for
// the context-building pipeline. It ingests fixtures/eval/corpus.json, drives
// extract and publish with the synthetic engines (no network, no model
// weights), applies the curated links, evaluates fixtures/eval/questions.json
// against contextbuild.Build and writes a JSON report.
//
// The database file is recreated on every run so the result is deterministic.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"recordingmcp/internal/eval"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "context-eval:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dbPath        = flag.String("db", "", "SQLite database path (default: a fresh temp file). The file is removed and recreated on every run.")
		outPath       = flag.String("out", "", "JSON report path (default: stdout)")
		corpusPath    = flag.String("corpus", filepath.Join("fixtures", "eval", "corpus.json"), "corpus fixture path")
		questionsPath = flag.String("questions", filepath.Join("fixtures", "eval", "questions.json"), "questions fixture path")
		linksPath     = flag.String("links", filepath.Join("fixtures", "eval", "links.json"), "curated links fixture path")
		maxItems      = flag.Int("max-items", 8, "contextbuild request MaxItems")
		maxBytes      = flag.Int("max-bytes", 16000, "contextbuild request MaxBytes")
	)
	flag.Parse()

	db := *dbPath
	cleanup := func() {}
	if db == "" {
		dir, err := os.MkdirTemp("", "context-eval-*")
		if err != nil {
			return err
		}
		db = filepath.Join(dir, "eval.db")
		cleanup = func() { os.RemoveAll(dir) }
	} else if dir := filepath.Dir(db); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	defer cleanup()

	report, err := eval.Run(context.Background(), eval.Options{
		DBPath:        db,
		CorpusPath:    *corpusPath,
		QuestionsPath: *questionsPath,
		LinksPath:     *linksPath,
		MaxItems:      *maxItems,
		MaxBytes:      *maxBytes,
	})
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if *outPath == "" {
		_, err = os.Stdout.Write(encoded)
		return err
	}
	return os.WriteFile(*outPath, encoded, 0o644)
}
