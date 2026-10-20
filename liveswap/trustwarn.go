package liveswap

import (
	"maps"
	"slices"

	"go.uber.org/zap"

	"github.com/smallhoursorg/hotserve/liveswap/deploytrust"
)

// warnUnboundTrust logs, once per configured deploy_trust block
// (deploytrust.WarnUnboundSource), the
// two shapes that load and are wider than they read: an OIDC block
// whose claims pin an identity but no branch (any ref of that identity
// deploys), and a local block without an audience (a token minted for
// any box that trusts the same key is accepted here). Warnings, not
// refusals — both are the documented minimum, and refusing them would
// fail every box on it. Once per block, not per app: an app without
// blocks of its own inherits the global ones, whose warning it would
// only repeat. The global blocks are read only when some app inherits
// them: a global block that every app overrides backs nothing but the
// unknown-app path (verify, then 404), and with no app at all nothing
// deploys. Runs after Validate, like the other load-time warnings, so
// a rejected config warns about nothing, and after Provision's
// defaults pass, so no app's config is nil.
func warnUnboundTrust(logger *zap.Logger, global []deploytrust.TrustConfig, apps map[string]*AppConfig) {
	if logger == nil {
		return
	}
	inherited := false
	for _, cfg := range apps {
		if len(cfg.DeployTrust) == 0 {
			inherited = true
		}
	}
	if inherited {
		for i, tc := range global {
			deploytrust.WarnUnboundSource(logger, tc, zap.Int("deploy_trust", i))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(apps)) {
		for i, tc := range apps[name].DeployTrust {
			deploytrust.WarnUnboundSource(logger, tc, zap.String("app", name), zap.Int("deploy_trust", i))
		}
	}
}
