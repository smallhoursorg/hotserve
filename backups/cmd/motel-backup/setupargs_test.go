package main

import "testing"

// setup's arguments: one repository, and --existing once, before or
// after it; anything else is the usage text — a word beginning with a
// dash that is not the flag, the terminator among them, since a URL
// never begins with one.
func TestSetupArgs(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		repo     string
		existing bool
		usage    bool
	}{
		{[]string{"s3:https://s3.example.com/b"}, "s3:https://s3.example.com/b", false, false},
		{[]string{"--existing", "s3:https://s3.example.com/b"}, "s3:https://s3.example.com/b", true, false},
		{[]string{"s3:https://s3.example.com/b", "--existing"}, "s3:https://s3.example.com/b", true, false},
		{[]string{"-existing", "s3:https://s3.example.com/b"}, "s3:https://s3.example.com/b", true, false},
		{[]string{"--existing=true", "s3:https://s3.example.com/b"}, "", false, true},
		{[]string{"--existing", "s3:https://s3.example.com/b", "--existing"}, "", false, true},
		{[]string{"--", "s3:https://s3.example.com/b", "--existing"}, "", false, true},
		{[]string{"--existing"}, "", false, true},
		{[]string{"--exists", "s3:https://s3.example.com/b"}, "", false, true},
		{[]string{"s3:https://s3.example.com/b", "s3:https://s3.example.com/c"}, "", false, true},
		{[]string{"s3:https://s3.example.com/b", "--existing", "x"}, "", false, true},
		{[]string{"s3:https://s3.example.com/b", "--"}, "", false, true},
		{[]string{"--", "--existing"}, "", false, true},
		{[]string{"--", "s3:https://s3.example.com/b"}, "", false, true},
		{[]string{"-x"}, "", false, true},
		{[]string{"", "s3:https://s3.example.com/b"}, "", false, true},
		{[]string{"s3:https://s3.example.com/b", ""}, "", false, true},
		// An unset variable: one word, empty, for the engine to refuse by
		// name ("it is empty"), not the usage text.
		{[]string{""}, "", false, false},
		{[]string{"--existing", ""}, "", true, false},
		{nil, "", false, true},
	} {
		repo, existing, err := setupArgs(tc.args)
		if tc.usage {
			if err == nil || err.Error() != usage {
				t.Errorf("%q: err %v, want the usage text", tc.args, err)
			}
			continue
		}
		if err != nil || repo != tc.repo || existing != tc.existing {
			t.Errorf("%q: %q %v %v; want %q %v", tc.args, repo, existing, err, tc.repo, tc.existing)
		}
	}
	// main's check of the arguments is setupArgs's answer: the usage
	// text, unprefixed, exit 2, as for every command given the wrong
	// arguments.
	for _, tc := range []struct {
		args []string
		ok   bool
	}{{nil, false}, {[]string{"s3:https://s3.example.com/b"}, true}, {[]string{"--existing", "s3:https://s3.example.com/b"}, true}, {[]string{"--existing"}, false}, {[]string{"a", "b"}, false}, {[]string{"a", "--existing", "c"}, false}} {
		if arguments("setup", tc.args) != tc.ok {
			t.Errorf("arguments(setup, %q) = %v", tc.args, !tc.ok)
		}
	}
	if arguments("validate", []string{"f"}) != true || arguments("validate", nil) != false || arguments("run", nil) != true || arguments("run", []string{"x"}) != false || arguments("restore", []string{"a", "--yes"}) != true {
		t.Error("the other commands' counts")
	}
}
