package main

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
)

// A terminal that could not be written to is a terminal that may not
// have shown the password: the next question fails, so that nothing
// typed ahead can confirm what was never seen.
func TestAFailedWriteEndsTheNextQuestion(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Nothing reads: the pipe fills, and a write into it fails once the
	// reader is gone.
	must(t, r.Close())
	term := &tty{f: w, in: bufio.NewReader(strings.NewReader("stored\n"))}
	term.Say("Repository password (new): shown-to-nobody")
	if _, err := term.Ask(context.Background(), "Type stored to go on: ", false); err == nil || !strings.Contains(err.Error(), "the terminal could not be written to") {
		t.Fatalf("the question was answered though the showing failed: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
