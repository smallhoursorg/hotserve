package box

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"

	"github.com/smallhoursorg/hotserve/box/proof"
	"github.com/smallhoursorg/hotserve/liveswap/deploytrust"
)

func init() {
	caddy.RegisterModule(App{})
}

// App is the `box` global option (DESIGN-box.md, "The shape"): who may
// push this box's configuration — `deploy_trust`, liveswap's grammar
// and verifier — and who may sign it — the `signer` lines.
//
// The verifiers are what box_webhook authenticates against. The
// signers are held here only so that a bad line fails config load, and
// with it `hotserve validate`; the signer list the box trusts is read
// from the installed file's raw tokens (Walk), never from the running
// configuration (step 10).
type App struct {
	DeployTrust []deploytrust.TrustConfig `json:"deploy_trust,omitempty"`
	Signers     []SignerConfig            `json:"signers,omitempty"`

	verifiers []deploytrust.Verifier
}

// SignerConfig is one `signer <principal> <key-type> <base64>` line.
type SignerConfig struct {
	Principal string `json:"principal"`
	Type      string `json:"type"`
	Key       string `json:"key"`
}

// CaddyModule returns the Caddy module information.
func (App) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "box",
		New: func() caddy.Module { return new(App) },
	}
}

// Provision checks every signer as the walk does (proof.ParseSigner on
// each line, then the set: one key, one principal) and builds the
// verifiers (deploytrust.New, which refuses an empty set at config
// load), then warns about a deploy_trust block wider than it reads.
func (a *App) Provision(ctx caddy.Context) error {
	return a.provision(ctx.Logger())
}

func (a *App) provision(logger *zap.Logger) error {
	if len(a.Signers) == 0 {
		return errors.New("box: no signer; the box applies only commits signed by a key its Caddyfile lists")
	}
	signers := make(proof.Signers, 0, len(a.Signers))
	for _, s := range a.Signers {
		sg, err := proof.ParseSigner(s.Principal, s.Type, s.Key)
		if err != nil {
			return fmt.Errorf("box: %w", err)
		}
		signers = append(signers, sg)
	}
	if _, err := signers.AllowedSigners(); err != nil {
		return fmt.Errorf("box: %w", err)
	}
	vs, err := deploytrust.New(a.DeployTrust, false)
	if err != nil {
		return fmt.Errorf("box: %w", err)
	}
	a.verifiers = vs
	// The warning reads a block as it will verify, placeholders
	// resolved; New resolved its own copy, so this is another.
	resolved := slices.Clone(a.DeployTrust)
	for i := range resolved {
		resolved[i].Claims = maps.Clone(resolved[i].Claims)
	}
	deploytrust.ResolvePlaceholders(caddy.NewReplacer(), resolved)
	for i, tc := range resolved {
		deploytrust.WarnUnboundSource(logger, tc, zap.String("option", "box"), zap.Int("deploy_trust", i))
	}
	return nil
}

// Start is a no-op: the app is configuration box_webhook reads.
func (*App) Start() error { return nil }

// Stop is a no-op.
func (*App) Stop() error { return nil }

var (
	_ caddy.App         = (*App)(nil)
	_ caddy.Provisioner = (*App)(nil)
)
