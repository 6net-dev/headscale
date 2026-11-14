package mapper

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// # The sweep is bounded in both directions
//
// A reconnect refreshes every cached World before a single event is served,
// which on a large installation is thousands of documents. Serially, at one
// per-peer timeout each, an unreachable Gentility would hold the stream's
// Connected callback — and therefore the stream — for hours. So the sweep
// reads several at once and gives up on the whole of it at a deadline.

// TestRefreshWorldsReadsInParallel: the pool is what makes a big sweep finish.
func TestRefreshWorldsReadsInParallel(t *testing.T) {
	const nodes = 32

	store := newFakeWorlds(map[string]*registry.World{})

	m := newTestMapper(store)
	for id := range nodes {
		key := registry.NodeID(uint64(id + 1))
		store.worlds[key] = world(key, "acme", "n", []string{selfAddr})
		store.slow[key] = 20 * time.Millisecond

		m.cacheWorld(types.NodeID(id+1), store.worlds[key])
	}

	m.refreshWorlds(t.Context())

	assert.Equal(t, worldRefreshWorkers, store.peakInFlight(),
		"the sweep reads exactly the pool's worth at a time")

	for id := range nodes {
		_, fresh := m.cachedWorld(types.NodeID(id + 1))
		assert.True(t, fresh, "node %d should be refreshed", id+1)
	}
}

// TestRefreshWorldsGivesUpAtItsDeadline: a Gentility that answers, but far too
// slowly to sweep in a reasonable time, must not hold the change stream. The
// sweep returns at its deadline with whatever it managed.
func TestRefreshWorldsGivesUpAtItsDeadline(t *testing.T) {
	const nodes = 200

	store := newFakeWorlds(map[string]*registry.World{})

	m := newTestMapper(store)
	m.worldRefreshTimeout = 150 * time.Millisecond

	for id := range nodes {
		key := registry.NodeID(uint64(id + 1))
		store.worlds[key] = world(key, "acme", "n", []string{selfAddr})
		store.slow[key] = 50 * time.Millisecond

		m.cacheWorld(types.NodeID(id+1), store.worlds[key])
	}

	// Serially this is ten seconds, and eight at a time is still more than
	// a second: the deadline is what bounds it, not the pool.
	started := time.Now()
	m.refreshWorlds(t.Context())
	elapsed := time.Since(started)

	assert.Less(t, elapsed, time.Second, "the sweep must return at its deadline")
	assert.Less(t, store.totalCalls(), nodes, "the sweep is expected to run out of time")

	// A node the sweep never reached is UNREFRESHED, not refreshed-and-
	// unchanged: the next map build that names it reads it on demand. That
	// is the whole of what "not reached" may mean, and stamping it would
	// quietly hand out a document cached over a connection that dropped.
	unreached := 0

	for id := range nodes {
		if _, fresh := m.cachedWorld(types.NodeID(id + 1)); !fresh {
			unreached++
		}
	}

	assert.Positive(t, unreached, "the nodes the sweep never reached stay unrefreshed")
}

// TestRefreshWorldsStopsWhenTheStreamDoes: the sweep runs on the stream's
// context, so a head shutting down does not wait it out.
func TestRefreshWorldsStopsWhenTheStreamDoes(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{})

	m := newTestMapper(store)

	for id := range 100 {
		key := registry.NodeID(uint64(id + 1))
		store.worlds[key] = world(key, "acme", "n", []string{selfAddr})
		store.slow[key] = 50 * time.Millisecond

		m.cacheWorld(types.NodeID(id+1), store.worlds[key])
	}

	ctx, cancel := context.WithCancel(t.Context())

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	started := time.Now()
	m.refreshWorlds(ctx)

	assert.Less(t, time.Since(started), time.Second)
}

// TestRefreshWorldsDoesNotHoldTheChangeStream is the sweep in the place it
// actually runs: inside [registry.ChangeHandler.Connected], which the client
// calls before it reads a single line. A Gentility answering World after World
// too slowly to sweep must not stop the head hearing about changes — the
// unswept documents are read on demand, and the news arrives on time.
func TestRefreshWorldsDoesNotHoldTheChangeStream(t *testing.T) {
	const nodes = 200

	mux := http.NewServeMux()
	mux.HandleFunc("/overlay/head/world/", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"renderer_version":1,"revision":1,"head_node_id":%q,"base_domain":%q}`,
			strings.TrimPrefix(r.URL.Path, "/overlay/head/world/"), baseDomain)
	})
	mux.HandleFunc("/overlay/head/changes", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "id: 1\ndata: {\"seq\":1,\"head_node_id\":\"1\",\"world_revision\":2}\n\n")

		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client := registry.New(server.URL, "secret")

	m := newTestMapper(client)
	m.worldRefreshTimeout = 200 * time.Millisecond

	for id := range nodes {
		key := registry.NodeID(uint64(id + 1))
		m.cacheWorld(types.NodeID(id+1), world(key, "acme", "n", []string{selfAddr}))
	}

	var delivered []registry.Change

	// Ten seconds of Worlds, one connection, one event waiting behind them.
	started := time.Now()
	err := client.Changes(t.Context(), nil, registry.ChangeHandler{
		Connected: func() error { m.refreshWorlds(t.Context()); return nil },
		Changed:   func(c registry.Change) error { delivered = append(delivered, c); return nil },
	})
	require.NoError(t, err)

	assert.Less(t, time.Since(started), 3*time.Second,
		"the sweep must not hold the stream for as long as the sweep would take")

	require.Len(t, delivered, 1, "the stream is read once the sweep gives up")
	assert.Equal(t, int64(1), delivered[0].Seq)
}
