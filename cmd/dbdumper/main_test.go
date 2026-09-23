package main

import (
	"errors"
	"flag"
	"io"
	"testing"
)

// TestParseFlagsDoesNotExit - with ExitOnError the flag package called os.Exit
// from inside Parse, skipping run's deferred console restore. The errors have
// to come back instead, told apart so run can exit as the flag package would.
func TestParseFlagsDoesNotExit(t *testing.T) {
	newSet := func() *flag.FlagSet {
		fs := flag.NewFlagSet("export", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.String("out", "", "")
		return fs
	}

	if err := parseFlags(newSet(), []string{"--out", "x"}); err != nil {
		t.Errorf("valid arguments: %v", err)
	}
	if err := parseFlags(newSet(), []string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: got %v, want flag.ErrHelp", err)
	}
	var ue usageError
	if err := parseFlags(newSet(), []string{"--bogus"}); !errors.As(err, &ue) {
		t.Errorf("unknown flag: got %T %v, want a usageError", err, err)
	}
}
