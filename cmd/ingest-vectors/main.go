// Command ingest-vectors writes the frozen contracts/ingest-v1/vectors files.
// Output is deterministic; committed files must match byte-for-byte
// (enforced by drift tests in internal/cloudsync and apps/cloud-ingest).
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"recordingmcp/internal/ingestvectors"
)

func main() {
	out := flag.String("out", "contracts/ingest-v1/vectors", "output directory for vector JSON files")
	flag.Parse()

	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatalf("create out dir: %v", err)
	}
	for _, f := range ingestvectors.Build() {
		path := filepath.Join(*out, f.Name)
		if err := os.WriteFile(path, []byte(f.Data), 0o644); err != nil {
			log.Fatalf("write %s: %v", path, err)
		}
		fmt.Printf("wrote %s (%d bytes)\n", path, len(f.Data))
	}
}
