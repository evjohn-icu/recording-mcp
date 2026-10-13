package main

import "testing"

func TestValidateRealMode(t *testing.T) {
	cases := []struct {
		name           string
		executor       string
		generator      string
		allowSynthetic bool
		wantErr        string
	}{
		{
			name:      "pipeline with synthetic generator is rejected without the flag",
			executor:  "pipeline",
			generator: "synthetic",
			wantErr:   "--generator synthetic produces placeholder candidates; pass --allow-synthetic for demos and tests only",
		},
		{
			name:           "pipeline with synthetic generator is allowed with the flag",
			executor:       "pipeline",
			generator:      "synthetic",
			allowSynthetic: true,
		},
		{
			name:      "pipeline with off generator needs no flag",
			executor:  "pipeline",
			generator: "off",
		},
		{
			name:      "pipeline with openai generator needs no flag",
			executor:  "pipeline",
			generator: "openai",
		},
		{
			name:      "synthetic executor stays demo-only but permitted",
			executor:  "synthetic",
			generator: "synthetic",
		},
		{
			name:      "other executor and generator names do not trip the guard",
			executor:  "pipeline",
			generator: "unknown-name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRealMode(tc.executor, tc.generator, tc.allowSynthetic)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateRealMode(%q, %q, %v) = %v, want nil", tc.executor, tc.generator, tc.allowSynthetic, err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("validateRealMode(%q, %q, %v) = %v, want %q", tc.executor, tc.generator, tc.allowSynthetic, err, tc.wantErr)
			}
		})
	}
}
