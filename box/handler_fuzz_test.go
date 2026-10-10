package box

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig"
	"go.uber.org/zap"

	"github.com/smallhoursorg/hotserve/liveswap/deploytrust"
)

// FuzzResultPoll drives `GET /?result=` with any query, any
// Authorization value and any marker bytes (or none), filed under the
// id the value's secret would derive. An oracle written from the
// Handler contract says what each request gets. A poll-shaped request —
// one `result` parameter, the Box-Poll scheme in any case, a canonical
// 32-byte standard base64 secret whose digest's first 32 hex are that
// parameter — is 500 when a marker stands at its id and cannot be
// read; admitted (202, never charged) when the marker holds the whole
// digest, whatever its age; otherwise an unauthenticated request like
// any other: the flat 401, charged. With a bearer (in place of the
// poll's Authorization), a malformed query is 400, and a well-formed
// one is 202 for a readable marker at its id, 500 for an unreadable
// one, 404 for none. No line is written before authentication.
func FuzzResultPoll(f *testing.F) {
	h, id, digest := secret(7)
	posted := func(age time.Duration) []byte {
		b, _ := json.Marshal(marker{SHA256: digest, Posted: time.Unix(1_700_000_000, 0).Add(-age)})
		return b
	}
	f.Add("result="+id, "Box-Poll "+h, posted(0), true, false)
	f.Add("result="+id, "box-poll "+h, posted(0), true, false)
	f.Add("result="+id, "Bearer "+h, posted(0), true, false)
	f.Add("result="+id, "Box-Poll "+h, posted(24*time.Hour), true, false)
	f.Add("result="+id, "Box-Poll "+h, posted(-time.Hour), true, false)
	f.Add("result="+id, "Box-Poll "+h, posted(0), false, false)
	f.Add("result="+id+"&result="+id, "Box-Poll "+h, posted(0), true, false)
	f.Add("result="+id, "Box-Poll "+h[:43], posted(0), true, true)
	f.Add("result="+id, "", posted(0), true, true)
	f.Add("result="+id, "", posted(0), false, true)
	f.Add("result="+id, "", []byte("{"), true, true)
	f.Add("result=%ZZ", "Box-Poll "+h, posted(0), true, true)
	f.Add("x=1", "", []byte("{"), true, true)
	f.Add("result="+id, "Box-Poll "+h, []byte(`{"sha256":"`+id+`","posted":"2023-11-14T22:13:20Z"}`), true, false)
	f.Add("result="+id, "Box-Poll "+h, []byte("{"), true, false)

	idRE := regexp.MustCompile(`^[0-9a-f]{32}$`)
	r := newRig(f)
	token := r.token(f)
	f.Fuzz(func(t *testing.T, query, auth string, markerBytes []byte, present, bearer bool) {
		if query == "" {
			return // `GET /`, the status: not this target's
		}
		r.h.limiter = deploytrust.NewLimiter(r.clock)
		r.logs.TakeAll() // else every iteration's lines stay for the whole run
		for _, d := range []string{"out", "stage"} {
			if err := os.RemoveAll(filepath.Join(r.dir, d)); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(r.dir, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		secretText := auth
		if len(auth) >= len("Box-Poll ") {
			secretText = auth[len("Box-Poll "):]
		}
		raw, err := base64.StdEncoding.DecodeString(secretText)
		if err != nil {
			raw = []byte(secretText)
		}
		sum := sha256.Sum256(raw)
		derived := hex.EncodeToString(sum[:])
		if present {
			if err := os.WriteFile(filepath.Join(r.dir, "stage", derived[:32]+".auth"), markerBytes, 0o644); err != nil {
				t.Fatal(err)
			}
		}

		// The oracle.
		q, qerr := url.ParseQuery(query)
		single := qerr == nil && len(q) == 1 && len(q["result"]) == 1
		wellFormed := single && idRE.MatchString(q["result"][0])
		shaped := !bearer && single && len(auth) == len("Box-Poll ")+44 && strings.EqualFold(auth[:len("Box-Poll ")], "Box-Poll ") &&
			len(raw) == 32 && base64.StdEncoding.EncodeToString(raw) == secretText && q["result"][0] == derived[:32]
		var m marker
		readable := len(markerBytes) <= maxMarker && json.Unmarshal(markerBytes, &m) == nil
		if readable {
			_, hexErr := hex.DecodeString(m.SHA256)
			readable = hexErr == nil && len(m.SHA256) == 64 && m.SHA256 == strings.ToLower(m.SHA256) && m.SHA256[:32] == derived[:32]
		}
		atQuery := present && wellFormed && q["result"][0] == derived[:32] // a marker stands at the query's id

		hr := httptest.NewRequest(http.MethodGet, "/", nil)
		hr.URL.RawQuery = query
		hr.Header["Authorization"] = []string{auth}
		if bearer {
			hr.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		if err := r.h.ServeHTTP(w, hr, nextFunc(func(http.ResponseWriter, *http.Request) error {
			t.Fatal("passed on")
			return nil
		})); err != nil {
			t.Fatal(err)
		}
		if !json.Valid(w.Body.Bytes()) {
			t.Fatalf("not JSON: %s", w.Body)
		}
		want, charged := http.StatusUnauthorized, 1
		switch {
		case shaped && present && !readable:
			want, charged = http.StatusInternalServerError, 0
		case shaped && present && m.SHA256 == derived:
			want, charged = http.StatusAccepted, 0
		case !bearer:
		case !wellFormed:
			want, charged = http.StatusBadRequest, 0
		case atQuery && readable:
			want, charged = http.StatusAccepted, 0
		case atQuery:
			want, charged = http.StatusInternalServerError, 0
		default:
			want, charged = http.StatusNotFound, 0
		}
		if w.Code != want || r.h.limiter.Size() != charged {
			t.Fatalf("got %d (%d charged), want %d (%d): %s", w.Code, r.h.limiter.Size(), want, charged, w.Body)
		}
		if !bearer && r.logs.FilterLevelExact(zap.ErrorLevel).Len() != 0 {
			t.Fatalf("a line before authentication: %v", r.logs.All())
		}
	})
}

// FuzzBoxOption: the `box` option's parser (what `hotserve validate`
// and the running box read) and the walk (what root reads) agree on
// the signers of any box block both accept — so the list the box
// trusts is never other than the one the running config was checked
// against. The walk goes first: a file it refuses never reaches the
// adapter, so no fuzzed `import` is ever followed.
func FuzzBoxOption(f *testing.F) {
	trust := "\t\tdeploy_trust github {\n\t\t\taudience hotserve\n\t\t\tclaim repository o/r\n\t\t}\n"
	for _, body := range []string{
		trust + "\t\tsigner alice@example.com ssh-ed25519 K1\n\t\tsigner bob ssh-ed25519 K2\n",
		trust + "\t\tsigner \"alice\" ssh-ed25519 \"K1\"\n",
		trust + "\t\tsigner alice ssh-ed25519 <<EOF\n\t\tK1\n\t\tEOF\n",
		trust + "\t\tsigner {$P:alice} ssh-ed25519 K1\n",
		trust + "\t\tsigner alice ssh-ed25519 K1 # bob\n",
		trust + "\t\tsigner alice ssh-ed25519 K1 {\n\t\t}\n\t\tsigner bob ssh-ed25519 K2\n",
		trust + "\t\tsigner alice ssh-ed25519 K1\n\t}\n\tbox {\n" + trust + "\t\tsigner bob ssh-ed25519 K2\n",
	} {
		f.Add(string(file(body)))
	}
	f.Fuzz(func(t *testing.T, body string) {
		in := []byte("{\n\tbox {\n" + body + "\n\t}\n}\n\ndeploy.example.com {\n\tbox_webhook\n}\n")
		shape, err := Walk(in)
		if err != nil {
			return
		}
		cfg, _, err := caddyconfig.GetAdapter("caddyfile").Adapt(in, map[string]any{"filename": "Caddyfile"})
		if err != nil {
			return
		}
		var c struct {
			Apps struct {
				Box *App `json:"box"`
			} `json:"apps"`
		}
		if err := json.Unmarshal(cfg, &c); err != nil {
			t.Fatal(err)
		}
		if c.Apps.Box == nil {
			t.Fatalf("the walk read a box block the adapter did not: %s", cfg)
		}
		got := c.Apps.Box.Signers
		if len(got) != len(shape.Signers) {
			t.Fatalf("the option has %d signers, the walk %d: %+v vs %+v", len(got), len(shape.Signers), got, shape.Signers)
		}
		for i, s := range shape.Signers {
			if got[i] != (SignerConfig{Principal: s.Principal, Type: s.Type, Key: s.B64}) {
				t.Fatalf("signer %d: option %+v, walk %+v", i, got[i], s)
			}
		}
	})
}
