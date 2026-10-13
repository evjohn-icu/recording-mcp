package cloudsync

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"recordingmcp/internal/ingestvectors"
)

// TestVectorsByteEqual detects drift between the committed contract vectors
// and the generator: re-running cmd/ingest-vectors must reproduce the exact
// bytes in contracts/ingest-v1/vectors.
func TestVectorsByteEqual(t *testing.T) {
	for _, f := range ingestvectors.Build() {
		f := f
		t.Run(f.Name, func(t *testing.T) {
			committed, err := os.ReadFile(filepath.Join("..", "..", "contracts", "ingest-v1", "vectors", f.Name))
			if err != nil {
				t.Fatalf("read committed vector %s: %v", f.Name, err)
			}
			if !bytes.Equal([]byte(f.Data), committed) {
				t.Errorf("vector %s drifted from committed file; regenerate with go run ./cmd/ingest-vectors --out contracts/ingest-v1/vectors", f.Name)
			}
		})
	}
}
