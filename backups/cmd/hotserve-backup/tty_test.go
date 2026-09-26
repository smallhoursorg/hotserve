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
	// Nobody reads: with the reader gone, a write fails at once (EPIPE;
	// no signal, the pipe not being standard output).
	must(t, r.Close())
	term := &tty{f: w, in: bufio.NewReader(strings.NewReader("stored\n"))}
	term.Say("Repository password (new): shown-to-nobody")
	// The question ends on the showing's failure — its own words — and
	// not merely because its prompt could not be written either.
	if _, err := term.Ask(context.Background(), "Type stored to go on: ", false); err == nil || !strings.Contains(err.Error(), "so what was to be shown may not have been") {
		t.Fatalf("the question was answered though the showing failed: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
