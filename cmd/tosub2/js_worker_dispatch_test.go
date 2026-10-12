package main

import (
	"os"
	"testing"

	"tosub2-go-port/internal/jshost"
)

// Runtime fixtures launch the current executable, which is a test binary here.
// Dispatch the same private worker mode before the testing package parses flags.
func TestMain(m *testing.M) {
	if handled, code := jshost.WorkerMain(os.Args[1:], os.Stdin, os.Stdout); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}
