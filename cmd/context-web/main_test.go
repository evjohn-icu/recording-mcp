package main

import "testing"

func TestValidateListenAddress(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8788", "[::1]:8788", "localhost:8788"} {
		if err := validateListenAddress(addr); err != nil {
			t.Errorf("%q rejected: %v", addr, err)
		}
	}
	for _, addr := range []string{":8788", "0.0.0.0:8788", "192.0.2.1:8788", "example.com:8788"} {
		if err := validateListenAddress(addr); err == nil {
			t.Errorf("%q accepted", addr)
		}
	}
}
