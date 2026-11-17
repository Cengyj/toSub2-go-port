package main

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestProductionProcessObservationLifecycle(t *testing.T) {
	const binary = "/fixture/tosub2"
	const foreignBinary = "/fixture/external-runtime"
	const parentArgs = binary + "\x00--debug-auth\x00--fixture-only\x00"
	const workerArgs = binary + "\x00--auth-js-worker\x00sentinel\x00/tmp/synthetic-input\x00"
	const running = "State:\tR (running)\nPPid:\t101\n"
	const zombie = "State:\tZ (zombie)\nPPid:\t101\n"
	const gone = "<gone>"
	for _, tc := range []struct {
		name        string
		statuses    []string
		executables []string
		arguments   []string
		observed    int32
		foreign     bool
		passes      int
	}{
		{"worker", []string{running}, []string{binary}, []string{workerArgs}, 1, false, 1},
		{"foreign", []string{running}, []string{foreignBinary}, []string{workerArgs}, 0, true, 1},
		{"foreign-empty-argv", []string{running}, []string{foreignBinary}, []string{""}, 0, true, 1},
		{"foreign-inherited-argv", []string{running}, []string{foreignBinary}, []string{parentArgs}, 0, true, 1},
		{"same-executable-wrong-role", []string{running}, []string{binary}, []string{binary + "\x00--unexpected\x00"}, 0, true, 1},
		{"exit-after-exe-read", []string{running}, []string{binary, gone}, []string{""}, 0, false, 1},
		{"exit-before-status-recheck", []string{running, gone}, []string{binary}, []string{""}, 0, false, 1},
		{"zombie-between-reads", []string{running, zombie}, []string{binary}, []string{""}, 0, false, 1},
		{"already-zombie", []string{zombie}, []string{binary}, []string{""}, 0, false, 1},
		{"empty-argv-transition", []string{running}, []string{binary}, []string{""}, 0, false, 1},
		{"fork-before-exec", []string{running}, []string{binary}, []string{parentArgs}, 0, false, 1},
		{"fork-then-worker", []string{running}, []string{binary}, []string{parentArgs, workerArgs}, 1, false, 2},
		{"exec-between-reads", []string{running}, []string{binary, foreignBinary}, []string{workerArgs}, 0, false, 1},
		{"foreign-after-exec-transition", []string{running}, []string{binary, foreignBinary}, []string{workerArgs}, 0, true, 2},
		{"reparented-between-reads", []string{running, "State:\tR (running)\nPPid:\t103\n"}, []string{binary}, []string{workerArgs}, 0, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "102"), 0700); err != nil {
				t.Fatal(err)
			}
			// Readers advance independently, modeling the actual non-atomic
			// status -> exe -> cmdline calls without timing-dependent processes.
			sequence := func(values []string) func() (string, error) {
				index := 0
				return func() (string, error) {
					value := values[min(index, len(values)-1)]
					index++
					if value == gone {
						return "", os.ErrNotExist
					}
					return value, nil
				}
			}
			status, executable, arguments := sequence(tc.statuses), sequence(tc.executables), sequence(tc.arguments)
			readFile := func(path string) ([]byte, error) {
				if path == filepath.Join(root, "101", "cmdline") {
					return []byte(parentArgs), nil
				}
				var value string
				var err error
				switch filepath.Base(path) {
				case "status":
					value, err = status()
				case "cmdline":
					value, err = arguments()
				default:
					return nil, os.ErrNotExist
				}
				return []byte(value), err
			}
			var observed atomic.Int32
			var foreign atomic.Bool
			for range tc.passes {
				inspectProductionChildrenAt(root, binary, 101, &observed, &foreign, readFile, func(string) (string, error) { return executable() })
			}
			if observed.Load() != tc.observed || foreign.Load() != tc.foreign {
				t.Fatalf("process classification observed=%d foreign=%t; want observed=%d foreign=%t", observed.Load(), foreign.Load(), tc.observed, tc.foreign)
			}
		})
	}
}
