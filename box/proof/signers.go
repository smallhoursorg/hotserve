package proof

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"regexp"

	"golang.org/x/crypto/ssh"
)

// Signer is one `signer <principal> <key-type> <base64>` line of the
// box block: one person's SSH public key under the name the result and
// the journal carry.
type Signer struct {
	Principal string
	Type      string
	Key       []byte // the wire-format public key the base64 decoded to
	B64       string // the base64 as written, for the allowed_signers file
}

// principalRE is what a principal may be (DESIGN-box.md, "The shape"):
// allowed_signers principals are patterns with `*`, `?`, `!` and `,`,
// and none of those may appear, nor whitespace or a quote.
var principalRE = regexp.MustCompile(`^[A-Za-z0-9._@+-]+$`)

// keyTypes are the key types a signer line may declare.
var keyTypes = map[string]bool{
	"ssh-ed25519":                        true,
	"ecdsa-sha2-nistp256":                true,
	"ecdsa-sha2-nistp384":                true,
	"ecdsa-sha2-nistp521":                true,
	"sk-ssh-ed25519@openssh.com":         true,
	"sk-ecdsa-sha2-nistp256@openssh.com": true,
	"ssh-rsa":                            true,
}

// ParseSigner checks one signer line's three arguments: the principal's
// charset, the key type against the list, and that the base64 decodes
// to a public key whose wire type is the declared one.
func ParseSigner(principal, keyType, b64 string) (Signer, error) {
	if !principalRE.MatchString(principal) {
		return Signer{}, fmt.Errorf("signer principal %q may only contain letters, digits and . _ @ + -", Bound(principal))
	}
	if !keyTypes[keyType] {
		return Signer{}, fmt.Errorf("signer %s: key type %q is not one of ssh-ed25519, ecdsa-sha2-nistp256/384/521, sk-ssh-ed25519@openssh.com, sk-ecdsa-sha2-nistp256@openssh.com, ssh-rsa", principal, Bound(keyType))
	}
	key, err := base64.StdEncoding.Strict().DecodeString(b64)
	if err != nil {
		return Signer{}, fmt.Errorf("signer %s: the key is not base64", principal)
	}
	pub, err := ssh.ParsePublicKey(key)
	if err != nil {
		return Signer{}, fmt.Errorf("signer %s: the key does not parse as an SSH public key", principal)
	}
	if pub.Type() != keyType {
		return Signer{}, fmt.Errorf("signer %s: the key is %s, not %s", principal, pub.Type(), keyType)
	}
	return Signer{Principal: principal, Type: keyType, Key: key, B64: b64}, nil
}

// Signers is a box block's signer lines, in file order.
type Signers []Signer

// AllowedSigners is the file ssh-keygen -Y reads: one line per signer,
// `<principal> namespaces="git" <type> <base64>`. The same key listed
// twice is refused here: find-principals would then name more than one
// principal for one signature, and the box names exactly one.
func (s Signers) AllowedSigners() ([]byte, error) {
	var out bytes.Buffer
	for i, a := range s {
		for _, b := range s[:i] {
			if bytes.Equal(a.Key, b.Key) {
				return nil, fmt.Errorf("signer %s and signer %s are the same key", b.Principal, a.Principal)
			}
		}
		fmt.Fprintf(&out, "%s namespaces=\"git\" %s %s\n", a.Principal, a.Type, a.B64)
	}
	return out.Bytes(), nil
}

// Has reports whether a signer carries that principal.
func (s Signers) Has(principal string) bool {
	for _, a := range s {
		if a.Principal == principal {
			return true
		}
	}
	return false
}

// HasKey reports whether a signer carries that wire-format key.
func (s Signers) HasKey(key []byte) bool {
	for _, a := range s {
		if bytes.Equal(a.Key, key) {
			return true
		}
	}
	return false
}
