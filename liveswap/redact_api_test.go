package liveswap

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFilterForTheBoxWebhook pins what the box webhook relies on when
// it sends its bodies through RespondJSON with a filter from
// NewRedactor: a nil filter still loses a token-shaped string to the
// shape layer; a filter primed with an environment loses that
// environment's values and names the key, in a body and in text; an
// id a body names survives the entropy layer only on the safe list,
// and never when it equals a known value.
func TestFilterForTheBoxWebhook(t *testing.T) {
	const jwt = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJyZXBvIn0.c2lnbmF0dXJlX2hlcmVfMTIz" // gitleaks:allow
	w := httptest.NewRecorder()
	if err := RespondJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "validate: token " + jwt + " rejected"}, nil); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusUnprocessableEntity || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("RespondJSON wrote %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if strings.Contains(w.Body.String(), jwt) || !strings.Contains(w.Body.String(), "[redacted:jwt]") {
		t.Errorf("a nil filter let a token through: %s", w.Body.String())
	}

	const text = "dial postgres://app:hunter2hunter2@db/app: refused, password hunter2hunter2"
	r := NewRedactor([]string{"DATABASE_URL=postgres://app:hunter2hunter2@db/app", "PATH=/usr/bin"}, nil)
	out, keys := r.Redact(text)
	if strings.Contains(out, "hunter2hunter2") || !strings.Contains(out, "[redacted:DATABASE_URL]") || len(keys) != 1 || keys[0] != "DATABASE_URL" {
		t.Errorf("Redact = %q, %v; want the value and its password replaced and the key named", out, keys)
	}
	w = httptest.NewRecorder()
	if err := RespondJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "password hunter2hunter2"}, r); err != nil {
		t.Fatal(err)
	}
	if got := w.Body.String(); strings.Contains(got, "hunter2hunter2") || !strings.Contains(got, "redacted_env") || !strings.Contains(got, "DATABASE_URL") {
		t.Errorf("a primed filter's body = %s; want the value replaced and the key named", got)
	}
	var none *Redactor
	if out, _ := none.Redact("token " + jwt); strings.Contains(out, jwt) {
		t.Errorf("a nil Redactor is the two layers: %q", out)
	}
	if out, _ := NewRedactor(nil, nil).Redact("token " + jwt); strings.Contains(out, jwt) {
		t.Errorf("an empty filter is the two layers: %q", out)
	}

	// The safe list: the sha a box body names survives the entropy
	// layer only when the caller lists it, as liveswap lists an app's
	// versions, and never when it equals a known value (rule 1).
	const sha = "3f9a1c2b4d5e6f708192a3b4c5d6e7f8091a2b3c" // the SHA TestRedactorSafeList pins as masked
	for name, tc := range map[string]struct {
		r        *Redactor
		survives bool
	}{
		"unlisted":         {NewRedactor(nil, nil), false},
		"listed":           {NewRedactor(nil, []string{sha}), true},
		"listed but known": {NewRedactor([]string{"TOKEN=" + sha}, []string{sha}), false},
	} {
		w := httptest.NewRecorder()
		if err := RespondJSON(w, http.StatusOK, map[string]string{"commit": sha}, tc.r); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(w.Body.String(), sha) != tc.survives {
			t.Errorf("%s: body = %s; want the sha to survive = %v", name, w.Body.String(), tc.survives)
		}
	}
}
