// Package fixtures provides the synthetic recording set used by the demo.
package fixtures

import _ "embed"

// Recordings contains the raw JSON array of synthetic event records.
//
//go:embed recordings.json
var Recordings []byte
