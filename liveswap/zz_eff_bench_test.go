package liveswap

import (
	"context"
	"strings"
	"testing"
)

func BenchmarkEffBoundRefusal(b *testing.B) {
	s := "local:/etc/hotserve/alice.pub: go-jose/go-jose: error in cryptographic primitive " + strings.Repeat("x", 220)
	for i := 0; i < b.N; i++ {
		_ = boundRefusal(s)
	}
}

func BenchmarkEffAuthorizeGarbage(b *testing.B) {
	_, pub := mustGenTestKey()
	local := resolveVerifiers([]trustSource{localTrust(pub, "aud1")}, nil)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = authorize(ctx, local, "not-a-jwt")
	}
}

func BenchmarkEffAuthorizeWrongAud(b *testing.B) {
	priv, pub := mustGenTestKey()
	local := resolveVerifiers([]trustSource{localTrust(pub, "aud1")}, nil)
	tok := mintTestToken(&testing.T{}, priv, "other", nil)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = authorize(ctx, local, tok)
	}
}

func BenchmarkEffAuthorizeClaimMismatch(b *testing.B) {
	priv, pub := mustGenTestKey()
	ts := localTrust(pub, "aud1")
	ts.claims = map[string]string{"repository": "org/blog"}
	ts.attribution = attributionClaims["github"]
	local := resolveVerifiers([]trustSource{ts}, nil)
	tok := mintTestToken(&testing.T{}, priv, "aud1", map[string]string{"repository": "org/other", "ref": "refs/heads/main", "actor": "alice"})
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = authorize(ctx, local, tok)
	}
}
