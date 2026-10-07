// Package semantic implements the pluggable semantic-recall backend behind the
// hybrid retrieval layer: vector encoding, embedding providers, a SQLite-backed
// index and a cosine-similarity retriever. The package never decides what is
// current — the query path applies the same stale/ambient filters the lexical
// layer uses, so a semantic hit can never resurrect superseded or ambient
// content.
package semantic

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Vector is one embedding vector. Vectors from a single provider always share
// Dims(); comparing vectors of different lengths is an error, never a silent
// truncation.
type Vector []float32

// Cosine returns the cosine similarity of a and b, clamped to [-1, 1]. It
// errors on a length mismatch. A zero-norm vector carries no direction, so its
// similarity is reported as 0 rather than as an error.
func Cosine(a, b Vector) (float64, error) {
	if len(a) != len(b) {
		return 0, fmt.Errorf("semantic: cosine length mismatch: %d vs %d", len(a), len(b))
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0, nil
	}
	v := dot / (math.Sqrt(na) * math.Sqrt(nb))
	if v > 1 {
		return 1, nil
	}
	if v < -1 {
		return -1, nil
	}
	return v, nil
}

// Encode serializes a vector as little-endian float32 bytes, matching Decode.
func Encode(v Vector) []byte {
	out := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(x))
	}
	return out
}

// Decode parses a vector encoded by Encode. dims must be positive and the byte
// length must be exactly dims*4; anything else is an error, never a partial
// read.
func Decode(b []byte, dims int) (Vector, error) {
	if dims <= 0 {
		return nil, errors.New("semantic: dims must be positive")
	}
	if len(b) != dims*4 {
		return nil, fmt.Errorf("semantic: vector is %d bytes, want %d", len(b), dims*4)
	}
	out := make(Vector, dims)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out, nil
}
