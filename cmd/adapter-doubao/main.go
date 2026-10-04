// adapter-doubao converts a parsed Doubao final-response log into provider-neutral ASR JSON.
// It never connects to an ASR service or opens audio.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"recordingmcp/internal/adapters/doubao"
	"recordingmcp/internal/asrinput"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "adapter-doubao:", err)
		os.Exit(1)
	}
}
func run() error {
	inPath := flag.String("input", "", "parsed response JSONL")
	metaPath := flag.String("metadata", "", "source envelope JSON")
	outPath := flag.String("output", "", "normalized JSON output")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flag.Args())
	}
	if *inPath == "" || *metaPath == "" || *outPath == "" {
		return errors.New("--input, --metadata and --output are required")
	}
	return convertAndWrite(*inPath, *metaPath, *outPath)
}

func convertAndWrite(inputPath, metadataPath, outputPath string) error {
	mb, err := os.ReadFile(metadataPath)
	if err != nil {
		return err
	}
	var meta asrinput.Metadata
	if err = json.Unmarshal(mb, &meta); err != nil {
		return fmt.Errorf("decode metadata: %w", err)
	}
	meta.RawEnvelope = append(json.RawMessage(nil), mb...)
	meta.RawEnvelope = append(json.RawMessage(nil), mb...)
	f, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	result, convertErr := doubao.Convert(meta, f)
	closeErr := f.Close()
	if convertErr != nil {
		return convertErr
	}
	if closeErr != nil {
		return closeErr
	}
	var output bytes.Buffer
	enc := json.NewEncoder(&output)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err = enc.Encode(result); err != nil {
		return err
	}
	return writeAtomic(outputPath, output.Bytes())
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".asrinput-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err = io.Copy(tmp, bytes.NewReader(data)); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	return nil
}
