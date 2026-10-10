package proof

import (
	"bytes"
	"crypto/rsa"
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

// MaxPrincipal bounds a principal: ssh-keygen prints the matching
// principal on stdout, which the verifier caps, and a name longer than
// this would be cut there and never match its own line.
const MaxPrincipal = 256

// ParseSigner checks one signer line's three arguments: the principal's
// charset and length, the key type against the list, and that the
// base64 decodes to a public key whose wire type is the declared one.
func ParseSigner(principal, keyType, b64 string) (Signer, error) {
	if !principalRE.MatchString(principal) {
		return Signer{}, fmt.Errorf("signer principal %q may only contain letters, digits and . _ @ + -", Bound(principal))
	}
	if len(principal) > MaxPrincipal {
		return Signer{}, fmt.Errorf("signer principal %q is longer than %d bytes", Bound(principal), MaxPrincipal)
	}
	if !keyTypes[keyType] {
		return Signer{}, fmt.Errorf("signer %s: key type %q is not one of ssh-ed25519, ecdsa-sha2-nistp256/384/521, sk-ssh-ed25519@openssh.com, sk-ecdsa-sha2-nistp256@openssh.com, ssh-rsa", principal, Bound(keyType))
	}
	key, err := base64.StdEncoding.Strict().DecodeString(b64)
	if err != nil {
		return Signer{}, fmt.Errorf("signer %s: the key is not base64", principal)
	}
	// The decoder ignores CR and LF, and a quoted Caddyfile token can
	// span lines; what is written to allowed_signers is the text as
	// given, so it must be the one canonical line for these bytes.
	if base64.StdEncoding.EncodeToString(key) != b64 {
		return Signer{}, fmt.Errorf("signer %s: the key is not one line of canonical base64", principal)
	}
	pub, err := ssh.ParsePublicKey(key)
	if err != nil {
		return Signer{}, fmt.Errorf("signer %s: the key does not parse as an SSH public key", principal)
	}
	if pub.Type() != keyType {
		return Signer{}, fmt.Errorf("signer %s: the key is %s, not %s", principal, pub.Type(), keyType)
	}
	// OpenSSH refuses RSA keys under its minimum when it reads an
	// allowed_signers file; a line it would refuse must not be one the
	// box accepts, or every push would read as "does not verify".
	if keyType == "ssh-rsa" {
		if cpk, ok := pub.(ssh.CryptoPublicKey); ok {
			if r, ok := cpk.CryptoPublicKey().(*rsa.PublicKey); ok && r.N.BitLen() < MinRSABits {
				return Signer{}, fmt.Errorf("signer %s: an RSA key of %d bits; ssh-keygen takes none under %d", principal, r.N.BitLen(), MinRSABits)
			}
		}
	}
	return Signer{Principal: principal, Type: keyType, Key: key, B64: b64}, nil
}

// MinRSABits is OpenSSH's floor for an RSA key it will read
// (sshkey_check_rsa_length): a shorter one in allowed_signers makes
// every verification fail.
const MinRSABits = 1024

// Signers is a box block's signer lines, in file order.
type Signers []Signer

// AllowedSigners is the file ssh-keygen -Y reads: one line per signer,
// `<principal> namespaces="git" <type> <base64>`. The same key listed
// twice is refused here: find-principals would then name more than one
// principal for one signature, and the box names exactly one.
func (s Signers) AllowedSigners() ([]byte, error) {
	var out bytes.Buffer
	seen := make(map[string]string, len(s)) // key bytes → principal; linear, since a file may list thousands and a chain asks per commit
	for _, a := range s {
		if first, dup := seen[string(a.Key)]; dup {
			return nil, fmt.Errorf("signer %s and signer %s are the same key", first, a.Principal)
		}
		seen[string(a.Key)] = a.Principal
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

// PrincipalFor is the principal listed for that wire-format key, byte
// for byte — the box's own answer to "which signer made this
// signature", taken from the key the signature carries.
func (s Signers) PrincipalFor(key []byte) (string, bool) {
	for _, a := range s {
		if bytes.Equal(a.Key, key) {
			return a.Principal, true
		}
	}
	return "", false
}
