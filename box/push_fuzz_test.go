package box

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/smallhoursorg/hotserve/box/proof"
	"github.com/smallhoursorg/hotserve/liveswap/deploytrust"
	"github.com/smallhoursorg/hotserve/liveswap/deploytrust/trusttest"
)

// FuzzPushAdmission drives an authenticated `POST /` with any request
// id, media type, body and `sha` claim, over a tree that is empty or
// holds another push. The oracle is the drop's contract: a bundle
// reaches in/ only as `<id>.tar`, byte for byte the body posted, with
// its marker beside it, and only for an answer that waited on root
// (504 here: no applier runs); every other answer leaves in/ and the
// push's marker as they were. Every answer is JSON with a status the
// Handler contract lists. One box serves every iteration, its exchange
// tree emptied between them: a box per iteration costs more than the
// push.
func FuzzPushAdmission(f *testing.F) {
	p := newPushRig(f)
	_, good := p.change()
	id := "0123456789abcdef0123456789abcdef"
	f.Add(id, "application/gzip", good, true, "", false)
	f.Add(id, "application/gzip", good, false, p.base, false)
	f.Add(id, "application/gzip", good, true, "", true)
	f.Add(id, "application/x-gzip; q=1", good, true, "", false)
	f.Add(id, "text/plain", good, true, "", false)
	f.Add("0123", "application/gzip", good, true, "", false)
	f.Add(id, "application/gzip", []byte("not a bundle"), true, "", false)
	f.Add(id, "application/gzip", tgz(f, p.repo.bundleFiles(p.base, p.base)), true, "", false)

	allowed := map[int]bool{
		http.StatusOK: true, http.StatusBadRequest: true, http.StatusConflict: true, http.StatusRequestEntityTooLarge: true,
		http.StatusUnsupportedMediaType: true, http.StatusUnprocessableEntity: true, http.StatusGatewayTimeout: true,
	}
	f.Fuzz(func(t *testing.T, id, ctype string, body []byte, shaFromBundle bool, sha string, pending bool) {
		p.t, p.repo.t = t, t // the harness reports through the iteration's t, never f
		for _, dir := range []string{"in", "stage", "out"} {
			for _, n := range p.names(p.x(dir)) {
				if err := os.RemoveAll(filepath.Join(p.x(dir), n)); err != nil {
					t.Fatal(err)
				}
			}
		}
		p.h.limiter = deploytrust.NewLimiter(trusttest.NewClock())
		p.logs.TakeAll()
		if shaFromBundle {
			if b, err := proof.ReadBundle(body); err == nil {
				sha = b.Commit.ID
			}
		}
		if pending {
			p.push(p.repo.bundleFiles(p.base, p.base))
		}
		inBefore := p.names(p.x("in"))
		w := p.post(t, p.h, push{id: id, ctype: ctype, token: p.token(t, sha), body: bytes.NewReader(body),
			header: map[string][]string{"Content-Type": {ctype}}})
		if !allowed[w.Code] || !json.Valid(w.Body.Bytes()) {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		in := p.names(p.x("in"))
		if w.Code != http.StatusGatewayTimeout {
			if len(in) != len(inBefore) || (isRequestID(id) && exists(p.x("stage/"+id+".auth"))) {
				t.Fatalf("%d, but in/ %v (was %v)", w.Code, in, inBefore)
			}
			return
		}
		if len(inBefore) != 0 || len(in) != 1 || in[0] != id+".tar" {
			t.Fatalf("504 with in/ %v (was %v)", in, inBefore)
		}
		got, err := os.ReadFile(filepath.Join(p.x("in"), in[0]))
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("in/ holds other bytes than were posted: %v", err)
		}
		if _, err := readMarker(p.x("stage/" + id + ".auth")); err != nil {
			t.Fatalf("no marker beside the bundle: %v", err)
		}
	})
}
