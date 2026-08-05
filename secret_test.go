package hotswap

import "testing"

func TestSecretsEqual(t *testing.T) {
	cases := []struct {
		provided, configured string
		want                 bool
	}{
		{"s3cret", "s3cret", true},
		{"", "", true},
		{"s3cret", "S3cret", false},
		{"", "s3cret", false},
		{"s3cret", "", false},
		{"s3cret-with-more", "s3cret", false},
	}
	for _, tc := range cases {
		if got := secretsEqual(tc.provided, tc.configured); got != tc.want {
			t.Errorf("secretsEqual(%q, %q) = %v, want %v", tc.provided, tc.configured, got, tc.want)
		}
	}
}
