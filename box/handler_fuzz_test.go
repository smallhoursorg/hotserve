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
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig"

	"github.com/smallhoursorg/hotserve/liveswap/deploytrust"
)

// FuzzResultPoll drives `GET /?result=` with any query, any poll
// secret header and any marker bytes, filed under the id the header
// would derive. The poll secret admits a request exactly when an
// oracle written from the Handler contract says it should — one
// `result` parameter, a canonical 32-byte standard base64 secret
// whose digest's first 32 hex are that parameter and whose whole
// digest the marker holds, posted within the last fifteen minutes —
// and is never charged. Everything else is the flat 401 without a
// bearer; with one, a malformed query is 400 and the rest never 401.
func FuzzResultPoll(f *testing.F) {
	h, id, digest := secret(7)
	posted := func(age time.Duration) []byte {
		b, _ := json.Marshal(marker{SHA256: digest, Posted: time.Unix(1_700_000_000, 0).Add(-age)})
		return b
	}
	f.Add("result="+id, h, posted(0), false)
	f.Add("result="+id, h, posted(pendingLife-time.Second), false)
	f.Add("result="+id, h, posted(pendingLife), false)
	f.Add("result="+id, h, posted(-time.Second), false)
	f.Add("result="+id+"&result="+id, h, posted(0), false)
	f.Add("result="+id, h[:43], posted(0), true)
	f.Add("result=%ZZ", h, posted(0), true)
	f.Add("x=1", "", []byte("{"), true)
	f.Add("result="+id, h, []byte(`{"sha256":"`+id+`","posted":"2023-11-14T22:13:20Z"}`), false)

	r := newRig(f)
	token := r.token(f)
	f.Fuzz(func(t *testing.T, query, header string, markerBytes []byte, bearer bool) {
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
		raw, err := base64.StdEncoding.DecodeString(header)
		if err != nil {
			raw = []byte(header)
		}
		sum := sha256.Sum256(raw)
		derived := hex.EncodeToString(sum[:])
		if err := os.WriteFile(filepath.Join(r.dir, "stage", derived[:32]+".auth"), markerBytes, 0o644); err != nil {
			t.Fatal(err)
		}

		// The oracle.
		q, qerr := url.ParseQuery(query)
		single := qerr == nil && len(q) == 1 && len(q["result"]) == 1
		var m marker
		markerOK := len(markerBytes) <= maxMarker && json.Unmarshal(markerBytes, &m) == nil && m.SHA256 == derived
		age := r.clock.Now().Sub(m.Posted)
		admit := single && len(raw) == pollSecretLen && base64.StdEncoding.EncodeToString(raw) == header &&
			q["result"][0] == derived[:32] && markerOK && age >= 0 && age < pendingLife

		hr := httptest.NewRequest(http.MethodGet, "/", nil)
		hr.URL.RawQuery = query
		hr.Header[pollSecretHeader] = []string{header}
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
		switch {
		case admit:
			if w.Code != http.StatusAccepted || r.h.limiter.Size() != 0 {
				t.Fatalf("admitted, but %d %s, %d charged", w.Code, w.Body, r.h.limiter.Size())
			}
		case !bearer:
			if w.Code != http.StatusUnauthorized || r.h.limiter.Size() != 1 {
				t.Fatalf("not admitted, no bearer, but %d %s", w.Code, w.Body)
			}
		case !single || !validID(q["result"][0]):
			if w.Code != http.StatusBadRequest {
				t.Fatalf("a malformed query with a bearer: %d %s", w.Code, w.Body)
			}
		default:
			if w.Code != http.StatusAccepted && w.Code != http.StatusNotFound && w.Code != http.StatusInternalServerError {
				t.Fatalf("a well-formed query with a bearer: %d %s", w.Code, w.Body)
			}
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
