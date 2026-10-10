package proof

import (
	"crypto/sha1" //nolint:gosec // git's object ids are SHA-1; plain crypto/sha1 over sha1dc is an accepted residual (DESIGN-box.md, step 13)
	"encoding/hex"
	"strconv"
)

// ObjectID is git's id of an object: sha1("<kind> <size>\0" + raw),
// lowercase hex. Computed, never read (DESIGN-box.md, step 13).
func ObjectID(kind string, raw []byte) string {
	h := sha1.New() //nolint:gosec // see the import
	h.Write([]byte(kind + " " + strconv.Itoa(len(raw)) + "\x00"))
	h.Write(raw)
	return hex.EncodeToString(h.Sum(nil))
}

// idForm is what a 40- or 64-character hex string looks like.
type idForm int

const (
	idBad    idForm = iota
	idSHA1          // 40 lowercase hex
	idSHA256        // 64 lowercase hex: a SHA-256 repository, which the box does not read
)

// formOfID classifies an object id as git writes it: lowercase hex of
// one of the two lengths. Uppercase is not an id git wrote.
func formOfID(s string) idForm {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return idBad
		}
	}
	switch len(s) {
	case 40:
		return idSHA1
	case 64:
		return idSHA256
	}
	return idBad
}

// IsID reports whether s is a 40-character lowercase hex SHA-1 id.
func IsID(s string) bool { return formOfID(s) == idSHA1 }

func sha256RepoRefusal(id string) error {
	return refuse("%s is in a SHA-256 repository, which the box does not read", id)
}
