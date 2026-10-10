package hscontrol

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/tailcfg"
)

func TestSecurityHeaders(t *testing.T) {
	handler := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	handler.ServeHTTP(rec, req)

	h := rec.Result().Header
	assert.Equal(t, "DENY", h.Get("X-Frame-Options"))
	assert.Equal(t, "frame-ancestors 'none'", h.Get("Content-Security-Policy"))
	assert.Equal(t, "nosniff", h.Get("X-Content-Type-Options"))
	assert.Equal(t, "no-referrer", h.Get("Referrer-Policy"))
}

func TestAddMagicDNSRoutesRoutesTheBaseDomain(t *testing.T) {
	v6 := netip.MustParsePrefix("fd7a:115c:a1e0::/48")

	newCfg := func(baseDomain string, magicDNS bool) *types.Config {
		return &types.Config{
			BaseDomain:       baseDomain,
			PrefixV6:         &v6,
			TailcfgDNSConfig: &tailcfg.DNSConfig{Proxied: magicDNS},
		}
	}

	t.Run("magic DNS routes the base domain to quad-100", func(t *testing.T) {
		cfg := newCfg("6net.io", true)
		addMagicDNSRoutes(cfg)

		// The node's own copy is a Clone; an entry must survive it, and only
		// an empty non-nil slice does.
		routes := cfg.CloneTailcfgDNSConfig().Routes

		require.Contains(t, routes, "6net.io")
		assert.NotNil(t, routes["6net.io"])
		assert.Empty(t, routes["6net.io"])
		assert.Contains(t, routes, "0.e.1.a.c.5.1.1.a.7.d.f.ip6.arpa")
	})

	t.Run("no base domain routes no empty suffix", func(t *testing.T) {
		cfg := newCfg("", true)
		addMagicDNSRoutes(cfg)

		assert.NotContains(t, cfg.TailcfgDNSConfig.Routes, "")
	})

	t.Run("without magic DNS nothing is routed", func(t *testing.T) {
		cfg := newCfg("6net.io", false)
		addMagicDNSRoutes(cfg)

		assert.Empty(t, cfg.TailcfgDNSConfig.Routes)
	})
}
