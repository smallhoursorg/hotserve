package backup

import (
	"context"
	"os"
	"strings"
	"testing"
)

// init asks only for what it was not given, and only for the providers
// the docs set up: typed secrets for an S3-compatible store or B2, and
// nothing for anything else.
func TestMissingCredentials(t *testing.T) {
	keys := func(cs []credential) string {
		var out []string
		for _, c := range cs {
			out = append(out, c.key)
		}
		return strings.Join(out, " ")
	}
	for _, tc := range []struct {
		repo  string
		given []string
		want  string
	}{
		{"s3:s3.us-west-004.backblazeb2.com/b", nil, "AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY"},
		{"s3:s3.example.com/b", []string{"AWS_ACCESS_KEY_ID=k"}, "AWS_SECRET_ACCESS_KEY"},
		{"s3:s3.example.com/b", []string{"AWS_ACCESS_KEY_ID=k", "AWS_SECRET_ACCESS_KEY=s"}, ""},
		{"b2:bucket:path", nil, "B2_ACCOUNT_ID B2_ACCOUNT_KEY"},
		{"/srv/backups", nil, ""},
		{"rest:https://backups.example.com/box", nil, ""},
	} {
		if got := keys(missingCredentials(tc.repo, tc.given)); got != tc.want {
			t.Errorf("%s with %v: asks for %q, want %q", tc.repo, tc.given, got, tc.want)
		}
	}
	// The secret half is read without echo; the ID is not a secret.
	for _, c := range missingCredentials("s3:s3.example.com/b", nil) {
		if c.secret != strings.Contains(c.key, "SECRET") {
			t.Errorf("%s: secret = %v", c.key, c.secret)
		}
	}
}

// Nobody is asked anything when nobody can answer: a script, a pipe,
// a CI job. They keep --credentials-file and --password-file.
func TestNoPromptsWithoutATerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	if terminalPrompter(context.Background(), r, os.Stderr) != nil {
		t.Error("a pipe is not a terminal; init must not wait on it for an answer")
	}
}

// One line at a time, and not a byte more: the next question reads the
// terminal itself, and anything a buffered read took would be lost.
func TestReadLineStopsAtTheNewline(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if _, err := w.WriteString("key-id\r\nsecret\n"); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	first, err := readLine(r)
	if err != nil || first != "key-id" {
		t.Fatalf("first line = %q, %v", first, err)
	}
	second, err := readLine(r)
	if err != nil || second != "secret" {
		t.Fatalf("the second answer must still be there for the next question: %q, %v", second, err)
	}
}
