package main

import "testing"

func TestValidateListenAddress(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8788", "[::1]:8788", "localhost:8788"} {
		if err := validateListenAddress(addr, false); err != nil {
			t.Errorf("%q rejected: %v", addr, err)
		}
	}
	for _, addr := range []string{":8788", "0.0.0.0:8788", "[::]:8788", "192.0.2.1:8788", "example.com:8788"} {
		if err := validateListenAddress(addr, false); err == nil {
			t.Errorf("%q accepted without --container-bind", addr)
		}
	}
	for _, addr := range []string{"0.0.0.0:8788", "[::]:8788"} {
		if err := validateListenAddress(addr, true); err != nil {
			t.Errorf("%q rejected with --container-bind: %v", addr, err)
		}
	}
	// Container mode only permits the unspecified address, never an arbitrary
	// external interface.
	for _, addr := range []string{":8788", "192.0.2.1:8788", "example.com:8788"} {
		if err := validateListenAddress(addr, true); err == nil {
			t.Errorf("%q accepted with --container-bind", addr)
		}
	}
}
