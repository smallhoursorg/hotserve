package liveswap

import (
	"net/url"
	"testing"
)

func TestParseAllowlistEntry(t *testing.T) {
	for in, wantErr := range map[string]bool{
		"artifacts.corp":                 false,
		"github.com/smallhoursorg/":      false,
		"github.com/smallhoursorg":       false, // trailing slash optional
		"GitHub.com/Org/deep/prefix":     false,
		"":                               true,
		"https://github.com/org":         true, // schemes rejected loudly
		"github.com/../escape":           true,
		"github.com//":                   true, // cleans to "/", no pin left
		"user@github.com/org":            true,
		"github.com:8443/org":            true, // ports not supported in entries
	} {
		_, err := parseAllowlistEntry(in)
		if (err != nil) != wantErr {
			t.Errorf("parseAllowlistEntry(%q) err=%v, wantErr=%v", in, err, wantErr)
		}
	}
}

func TestAllowlistMatching(t *testing.T) {
	org, err := parseAllowlistEntry("github.com/smallhoursorg/")
	if err != nil {
		t.Fatal(err)
	}
	bare, err := parseAllowlistEntry("artifacts.corp")
	if err != nil {
		t.Fatal(err)
	}
	entries := []artifactAllowEntry{org, bare}

	for rawURL, want := range map[string]bool{
		// the org pin
		"https://github.com/smallhoursorg/hotserve/releases/download/v1/a.tgz": true,
		"https://github.com/smallhoursorg":                                     true, // the prefix itself
		"https://GITHUB.COM/smallhoursorg/x":                                   true, // host case folds
		"https://github.com/SmallHoursOrg/x":                                   false, // path case fails closed
		"https://github.com/smallhoursorg-evil/x":                              false, // segment boundary
		"https://github.com/evilorg/x":                                         false,
		"https://github.com/smallhoursorg/../evilorg/x":                        false, // dot segments cleaned
		"https://github.com/smallhoursorg/%2e%2e/evilorg/x":                    false, // encoded dot segments
		"https://github.com/%73mallhoursorg/x":                                 true,  // decoding is symmetric
		"https://github.com@evil.com/smallhoursorg/x":                          false, // userinfo trick
		"https://objects.githubusercontent.com/whatever":                       false, // redirect host NOT listed
		// the bare host
		"https://artifacts.corp/anything/at/all.tgz": true,
		"https://ARTIFACTS.CORP/x":                   true,
		"https://artifacts.corp.evil.com/x":          false,
		"https://artifacts.corp:8443/x":              true, // hostname match; port not pinned
	} {
		u, err := url.Parse(rawURL)
		if err != nil {
			t.Fatalf("parse %q: %v", rawURL, err)
		}
		if _, got := matchAllowlist(entries, u); got != want {
			t.Errorf("match(%q) = %v, want %v", rawURL, got, want)
		}
	}
}
