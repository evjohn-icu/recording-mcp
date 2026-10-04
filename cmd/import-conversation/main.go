package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"recordingmcp/internal/memory"
)

type runInfo struct {
	AudioSHA256   string `json:"audio_sha256"`
	AudioFileName string `json:"audio_file_name"`
}

func decode(path string, out any) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if e = d.Decode(out); e != nil {
		return fmt.Errorf("decode %s: %w", path, e)
	}
	var x any
	if e = d.Decode(&x); !errors.Is(e, io.EOF) {
		return fmt.Errorf("%s contains trailing JSON", path)
	}
	return nil
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, "import-conversation:", e)
		os.Exit(1)
	}
}
func run() error {
	db := flag.String("db", "data/context-memory.db", "experimental context memory SQLite file")
	transcriptPath := flag.String("transcript", "", "normalized transcript.json")
	annotationPath := flag.String("annotations", "", "human interpretation annotations.json")
	runPath := flag.String("run", "", "ASR run.json containing source audio SHA-256")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flag.Args())
	}
	if *transcriptPath == "" || *annotationPath == "" || *runPath == "" {
		return errors.New("--transcript, --annotations and --run are required")
	}
	var transcript memory.Transcript
	var annotations memory.Annotations
	var metadata runInfo
	if e := decode(*transcriptPath, &transcript); e != nil {
		return e
	}
	if e := decode(*annotationPath, &annotations); e != nil {
		return e
	}
	b, e := os.ReadFile(*runPath)
	if e != nil {
		return e
	}
	if e = json.Unmarshal(b, &metadata); e != nil {
		return fmt.Errorf("decode %s: %w", *runPath, e)
	}
	if metadata.AudioSHA256 == "" {
		return errors.New("run.json is missing audio_sha256")
	}
	store, e := memory.Open(*db)
	if e != nil {
		return e
	}
	defer store.Close()
	report, e := store.Import(context.Background(), transcript, annotations, metadata.AudioFileName, metadata.AudioSHA256)
	if e != nil {
		return e
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}
