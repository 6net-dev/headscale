package mapper

import (
	"context"
	"testing"
	"time"

	"github.com/juanfont/headscale/hscontrol/registry"
	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/tailcfg"
)

// A reconnect is exactly the window in which the head may have missed a
// change, so no map response may be built from a World cached before the
// current connection. RefreshWorlds is what a (re)connect calls: it marks
// every entry unrefreshed and reads them all again.
func TestRefreshWorldsReadsEveryCachedWorldAgain(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr}),
		"2": world("2", "acme", "n2", []string{reachedAddr}),
	})

	m := newTestMapper(store)
	m.cacheWorld(types.NodeID(1), store.worlds["1"])
	m.cacheWorld(types.NodeID(2), store.worlds["2"])

	m.refreshWorlds(context.Background())

	assert.Equal(t, 1, store.callsFor("1"))
	assert.Equal(t, 1, store.callsFor("2"))

	for _, id := range []types.NodeID{1, 2} {
		_, fresh := m.cachedWorld(id)
		assert.True(t, fresh, "node %d should be refreshed for this connection", id)
	}
}

// The gate itself, seen from a map build. A World cached before the current
// connection is not fresh however young it is, so the build re-reads it rather
// than handing out a document that may have moved while the head was away.
func TestAWorldCachedBeforeTheConnectionIsNotServed(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr},
			registry.Filter{Src: selfAddr + "/128", Dst: reachedAddr + "/128", Ports: []int{443}}),
		"2": world("2", "acme", "reached", []string{reachedAddr}),
	})

	m := newTestMapper(store)
	m.cacheWorld(types.NodeID(2), store.worlds["2"])

	// The stream reconnects. Nothing has been swept yet.
	m.beginWorldRefresh()

	resp := &tailcfg.MapResponse{
		Node:  worldNode(1, "100.64.0.1"),
		Peers: []*tailcfg.Node{worldNode(2, "100.64.0.2")},
	}
	require.NoError(t, m.rewriteFromRegistry(types.NodeID(1), resp))

	assert.Equal(t, 1, store.callsFor("2"),
		"a peer cached before this connection must be read again, not served from the cache")
}

// Fail-static, and the floor under it. A refresh that cannot be made does not
// stop the head serving what it last rendered — and the entry is stamped as
// read for this connection anyway, so an unreachable Gentility is asked once
// per TTL rather than once per map build.
func TestAnUnrefreshableWorldIsStillServedAndNotHammered(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr},
			registry.Filter{Src: selfAddr + "/128", Dst: reachedAddr + "/128", Ports: []int{443}}),
	})
	cached := world("2", "acme", "reached", []string{reachedAddr})

	m := newTestMapper(store)
	m.cacheWorld(types.NodeID(2), cached)
	m.beginWorldRefresh()

	store.errs["2"] = context.DeadlineExceeded

	for range 2 {
		resp := &tailcfg.MapResponse{
			Node:  worldNode(1, "100.64.0.1"),
			Peers: []*tailcfg.Node{worldNode(2, "100.64.0.2")},
		}
		require.NoError(t, m.rewriteFromRegistry(types.NodeID(1), resp))

		require.Len(t, resp.Peers, 1, "the peer is served as last seen")
		assert.Equal(t, []string{reachedAddr + "/128"}, prefixes(t, resp.Peers[0]))
	}

	assert.Equal(t, 1, store.callsFor("2"),
		"a failed read still counts as read for this connection")
}

// Gentility answering that there is no World here is an answer, not an outage:
// the entry goes, and the next build learns the peer is gone rather than
// serving a revoked node's last document out of the cache forever.
func TestRefreshWorldsForgetsAWorldGentilityRefuses(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{})

	m := newTestMapper(store)
	m.cacheWorld(types.NodeID(2), world("2", "acme", "gone", []string{reachedAddr}))

	m.refreshWorlds(context.Background())

	cached, _ := m.cachedWorld(types.NodeID(2))
	assert.Nil(t, cached, "a refused node is forgotten")
}

// The disk cache's two halves. What is snapshotted is what comes back, and
// what comes back is NOT refreshed for the connection that loads it: a head
// that has just booted has not spoken to Gentility yet, so the first
// connection's sweep is what makes these documents current.
func TestSnapshotAndLoadWorldsRoundTrip(t *testing.T) {
	m := newTestMapper(newFakeWorlds(map[string]*registry.World{}))

	first := world("1", "acme", "n1", []string{selfAddr})
	m.cacheWorld(types.NodeID(1), first)

	snapshot := m.snapshotWorlds()
	require.Len(t, snapshot, 1)
	assert.Equal(t, first, snapshot[types.NodeID(1)])

	restored := newTestMapper(newFakeWorlds(map[string]*registry.World{}))
	restored.loadWorlds(snapshot)

	cached, fresh := restored.cachedWorld(types.NodeID(1))
	assert.Equal(t, first, cached)
	assert.False(t, fresh, "a World off the disk has not been refreshed on any connection")
}

// The batcher is the seam the change-stream runner drives, so the same four
// operations have to be reachable through it.
func TestBatcherExposesTheWorldCache(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr}),
	})
	b := &Batcher{mapper: newTestMapper(store)}

	b.LoadWorlds(map[types.NodeID]*registry.World{types.NodeID(1): store.worlds["1"]})
	require.Len(t, b.SnapshotWorlds(), 1)

	b.RefreshWorlds(context.Background())
	assert.Equal(t, 1, store.callsFor("1"))

	b.InvalidateWorld(types.NodeID(1))
	assert.Empty(t, b.SnapshotWorlds())
}

// A head with no registry has no cache, and every one of these is a no-op
// rather than a panic: app.go must be able to call them without asking.
func TestTheWorldCacheIsInertWithoutARegistry(t *testing.T) {
	b := &Batcher{mapper: &mapper{worldTTL: time.Minute, now: time.Now}}

	assert.NotPanics(t, func() {
		b.LoadWorlds(map[types.NodeID]*registry.World{types.NodeID(1): {}})
		b.RefreshWorlds(context.Background())
		b.InvalidateWorld(types.NodeID(1))
	})
	assert.Nil(t, b.SnapshotWorlds())
}
