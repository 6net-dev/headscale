package mapper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juanfont/headscale/hscontrol/registry"
	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/tailcfg"
)

const baseDomain = "overlay.example"

// Addresses in one organization's /64: the node, a peer it has a path to, and
// a peer it does not.
const (
	selfAddr     = "fd7a:115c:a1e0:1::a"
	reachedAddr  = "fd7a:115c:a1e0:1::b"
	unreachedddr = "fd7a:115c:a1e0:1::c"
	gatewayAddr  = "fd7a:115c:a1e0:1::1"
)

// fakeWorlds is the World store the rewrite reads, standing in for Gentility.
type fakeWorlds struct {
	mu     sync.Mutex
	worlds map[string]*registry.World
	errs   map[string]error
	calls  map[string]int

	// budgets is how long each call had left when it arrived, which is
	// what says whether the fetches share a deadline or hold their own.
	budgets map[string]time.Duration

	// slow is how long a call takes, so one fetch can be made to eat into
	// whatever budget the next one is given.
	slow map[string]time.Duration

	// inFlight and peak are how many calls were running at once, which is
	// what says whether a sweep is a pool or a queue of one.
	inFlight int
	peak     int
}

func (f *fakeWorlds) World(ctx context.Context, headNodeID string) (*registry.World, error) {
	f.mu.Lock()

	f.calls[headNodeID]++

	f.inFlight++
	f.peak = max(f.peak, f.inFlight)

	if deadline, ok := ctx.Deadline(); ok {
		f.budgets[headNodeID] = time.Until(deadline)
	}

	slow := f.slow[headNodeID]

	f.mu.Unlock()

	if slow > 0 {
		time.Sleep(slow)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.inFlight--

	if err, ok := f.errs[headNodeID]; ok {
		return nil, err
	}

	world, ok := f.worlds[headNodeID]
	if !ok {
		return nil, registry.ErrUnknownNode
	}

	return world, nil
}

func (f *fakeWorlds) budgetFor(headNodeID string) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.budgets[headNodeID]
}

// peakInFlight is the most calls this store ever had running at once.
func (f *fakeWorlds) peakInFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.peak
}

// totalCalls is every call the store answered, however it answered it.
func (f *fakeWorlds) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	total := 0
	for _, calls := range f.calls {
		total += calls
	}

	return total
}

func (f *fakeWorlds) callsFor(headNodeID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls[headNodeID]
}

func newFakeWorlds(worlds map[string]*registry.World) *fakeWorlds {
	return &fakeWorlds{
		worlds:  worlds,
		errs:    map[string]error{},
		calls:   map[string]int{},
		budgets: map[string]time.Duration{},
		slow:    map[string]time.Duration{},
	}
}

func newTestMapper(store worldSource) *mapper {
	return &mapper{
		cfg:        &types.Config{BaseDomain: baseDomain},
		regClient:  store,
		worldCache: map[types.NodeID]cachedWorld{},
		worldTTL:   defaultWorldTTL,
		now:        time.Now,
	}
}

// clock is a hand-wound now for the cache TTL, which is otherwise only
// testable by waiting a minute.
type clock struct{ at time.Time }

func (c *clock) now() time.Time { return c.at }

func (c *clock) advance(d time.Duration) { c.at = c.at.Add(d) }

// world is a v1 World for one node: its own addresses and names, and the
// filters that decide who it can see.
func world(headNodeID, org, label string, addrs []string, filters ...registry.Filter) *registry.World {
	return &registry.World{
		RendererVersion: registry.RendererVersion1,
		Revision:        1,
		HeadNodeID:      headNodeID,
		Addresses:       addrs,
		Names:           []registry.Name{{Org: org, Label: label}},
		Filters:         filters,
	}
}

// worldNode is a peer as the head builds it, before the rewrite: a headscale
// address and a headscale name, both of which the World replaces.
func worldNode(id int64, addr string) *tailcfg.Node {
	own := netip.MustParseAddr(addr)

	return &tailcfg.Node{
		ID:        tailcfg.NodeID(id),
		Name:      "stale.example.",
		Addresses: []netip.Prefix{netip.PrefixFrom(own, own.BitLen())},
	}
}

func prefixes(t *testing.T, node *tailcfg.Node) []string {
	t.Helper()

	out := make([]string, 0, len(node.Addresses))
	for _, prefix := range node.Addresses {
		out = append(out, prefix.String())
	}

	return out
}

func allowedIPs(t *testing.T, node *tailcfg.Node) []string {
	t.Helper()

	out := make([]string, 0, len(node.AllowedIPs))
	for _, prefix := range node.AllowedIPs {
		out = append(out, prefix.String())
	}

	return out
}

func peerIDs(peers []*tailcfg.Node) []int64 {
	out := make([]int64, 0, len(peers))
	for _, peer := range peers {
		out = append(out, int64(peer.ID))
	}

	return out
}

// The whole of Reading 2 in one test: the World lists no peers, the filters
// decide who is visible, and each surviving peer's addresses and name come
// from that peer's own World rather than from the node's.
func TestRewriteKeepsOnlyThePeersTheFiltersName(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr},
			registry.Filter{Src: selfAddr + "/128", Dst: reachedAddr + "/128", Ports: []int{443}}),
		"2": world("2", "acme", "reached", []string{reachedAddr}),
		"3": world("3", "acme", "unreached", []string{unreachedddr}),
	})

	resp := &tailcfg.MapResponse{
		Node:  worldNode(1, "100.64.0.1"),
		Peers: []*tailcfg.Node{worldNode(2, "100.64.0.2"), worldNode(3, "100.64.0.3")},
	}

	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	assert.Equal(t, []int64{2}, peerIDs(resp.Peers), "only the peer a filter names survives")
	assert.Equal(t, []tailcfg.NodeID{3}, resp.PeersRemoved,
		"a dropped peer must be announced, or the client keeps it")

	assert.Equal(t, []string{reachedAddr + "/128"}, prefixes(t, resp.Peers[0]))
	assert.Equal(t, "reached.acme."+baseDomain+".", resp.Peers[0].Name)

	assert.Equal(t, []string{selfAddr + "/128"}, prefixes(t, resp.Node))
	assert.Equal(t, "n1.acme."+baseDomain+".", resp.Node.Name)
}

// The World is asked for by the head's own node id. The node key is mutable —
// it rotates — and a document keyed by one moves when nothing about the node
// does.
func TestRewriteLooksTheWorldUpByHeadNodeID(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"17": world("17", "acme", "n1", []string{selfAddr}),
	})

	resp := &tailcfg.MapResponse{Node: worldNode(17, "100.64.0.1")}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(17), resp))

	assert.Equal(t, 1, store.callsFor("17"))
	assert.Equal(t, "17", registry.NodeID(17))
}

// A Tailscale client enforces its filter on inbound traffic, so a node's World
// carries the rules where it is the destination too — and a peer named only as
// a source is just as visible as one named as a destination.
func TestRewriteSeesAPeerNamedOnlyAsAFilterSource(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr},
			registry.Filter{Src: gatewayAddr + "/128", Dst: selfAddr + "/128", Ports: []int{443}}),
		"2": world("2", "acme", "gateway", []string{gatewayAddr}),
	})

	resp := &tailcfg.MapResponse{Peers: []*tailcfg.Node{worldNode(2, "100.64.0.2")}}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	assert.Equal(t, []int64{2}, peerIDs(resp.Peers))
	assert.Empty(t, resp.PeersRemoved)
}

// A node's own addresses are not peers of itself, however many filters name
// them.
func TestRewriteNeverKeepsTheNodeAsItsOwnPeer(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr},
			registry.Filter{Src: selfAddr + "/128", Dst: selfAddr + "/128", Ports: []int{443}}),
	})

	resp := &tailcfg.MapResponse{Peers: []*tailcfg.Node{worldNode(1, "100.64.0.1")}}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	assert.Empty(t, resp.Peers)
	assert.Equal(t, []tailcfg.NodeID{1}, resp.PeersRemoved)
}

// Gentility REFUSING a peer is an answer: there is no such World, or none has
// been rendered. The peer is dropped and the drop is announced, because the
// client would otherwise keep it forever.
func TestRewriteDropsAPeerGentilityRefuses(t *testing.T) {
	for _, refusal := range []error{registry.ErrUnknownNode, registry.ErrNotRendered} {
		t.Run(refusal.Error(), func(t *testing.T) {
			store := newFakeWorlds(map[string]*registry.World{
				"1": world("1", "acme", "n1", []string{selfAddr},
					registry.Filter{Src: selfAddr + "/128", Dst: reachedAddr + "/128", Ports: []int{443}}),
			})
			store.errs["2"] = refusal

			resp := &tailcfg.MapResponse{Peers: []*tailcfg.Node{worldNode(2, "100.64.0.2")}}
			require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

			assert.Empty(t, resp.Peers)
			assert.Equal(t, []tailcfg.NodeID{2}, resp.PeersRemoved)
		})
	}
}

// An answer that did not ARRIVE is not an answer. With nothing cached this
// round says nothing about the peer: announcing a removal would tell the
// client a revocation happened, on the evidence of a timeout.
func TestRewriteSaysNothingAboutAPeerItCouldNotReach(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr},
			registry.Filter{Src: selfAddr + "/128", Dst: reachedAddr + "/128", Ports: []int{443}}),
	})
	store.errs["2"] = errors.New("registry down")

	resp := &tailcfg.MapResponse{Peers: []*tailcfg.Node{worldNode(2, "100.64.0.2")}}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	assert.Empty(t, resp.Peers)
	assert.Empty(t, resp.PeersRemoved, "a transient failure is not a revocation")
}

// With something cached, a transient failure serves the peer as last seen —
// the same stale-but-sealed rule the node's own World follows.
func TestRewriteServesAPeerAsLastSeenThroughAnOutage(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr},
			registry.Filter{Src: selfAddr + "/128", Dst: reachedAddr + "/128", Ports: []int{443}}),
		"2": world("2", "acme", "reached", []string{reachedAddr}),
	})

	tick := &clock{at: time.Now()}
	m := newTestMapper(store)
	m.now = tick.now

	first := &tailcfg.MapResponse{Peers: []*tailcfg.Node{worldNode(2, "100.64.0.2")}}
	require.NoError(t, m.rewriteFromRegistry(types.NodeID(1), first))
	require.Len(t, first.Peers, 1)

	store.errs["2"] = errors.New("registry down")
	tick.advance(2 * defaultWorldTTL)

	again := &tailcfg.MapResponse{Peers: []*tailcfg.Node{worldNode(2, "100.64.0.2")}}
	require.NoError(t, m.rewriteFromRegistry(types.NodeID(1), again))

	require.Len(t, again.Peers, 1)
	assert.Equal(t, []string{reachedAddr + "/128"}, prefixes(t, again.Peers[0]))
	assert.Empty(t, again.PeersRemoved)
	assert.Equal(t, 2, store.callsFor("2"), "the stale entry was re-fetched, then fallen back on")
}

// A cached World is a copy of a document that moves, so it goes stale on its
// own even if no change event ever arrives.
func TestRewriteRefetchesAPeerWorldPastItsTTL(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr},
			registry.Filter{Src: selfAddr + "/128", Dst: reachedAddr + "/128", Ports: []int{443}}),
		"2": world("2", "acme", "reached", []string{reachedAddr}),
	})

	tick := &clock{at: time.Now()}
	m := newTestMapper(store)
	m.now = tick.now

	build := func() {
		resp := &tailcfg.MapResponse{Peers: []*tailcfg.Node{worldNode(2, "100.64.0.2")}}
		require.NoError(t, m.rewriteFromRegistry(types.NodeID(1), resp))
		require.Len(t, resp.Peers, 1)
	}

	build()
	tick.advance(defaultWorldTTL / 2)
	build()
	assert.Equal(t, 1, store.callsFor("2"), "inside the TTL the cache answers")

	tick.advance(defaultWorldTTL)
	build()
	assert.Equal(t, 2, store.callsFor("2"), "past the TTL the document is read again")
}

// The seam B6's change stream invalidates by: an event names a node, and the
// next map build reads that node's World afresh rather than waiting out a TTL.
func TestInvalidateWorldForcesARefetch(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr},
			registry.Filter{Src: selfAddr + "/128", Dst: reachedAddr + "/128", Ports: []int{443}}),
		"2": world("2", "acme", "reached", []string{reachedAddr}),
	})

	m := newTestMapper(store)

	build := func() {
		resp := &tailcfg.MapResponse{Peers: []*tailcfg.Node{worldNode(2, "100.64.0.2")}}
		require.NoError(t, m.rewriteFromRegistry(types.NodeID(1), resp))
		require.Len(t, resp.Peers, 1)
	}

	build()
	build()
	require.Equal(t, 1, store.callsFor("2"))

	m.invalidateWorld(types.NodeID(2))

	build()
	assert.Equal(t, 2, store.callsFor("2"))
}

// Every peer gets its own budget. One shared deadline across a map build means
// the last peer in a large netmap is fetched with whatever the peers before it
// left over, so a single slow answer silently drops the tail of the list.
func TestRewriteGivesEveryPeerItsOwnBudget(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr},
			registry.Filter{Src: selfAddr + "/128", Dst: reachedAddr + "/128", Ports: []int{443}},
			registry.Filter{Src: selfAddr + "/128", Dst: unreachedddr + "/128", Ports: []int{443}}),
		"2": world("2", "acme", "reached", []string{reachedAddr}),
		"3": world("3", "acme", "second", []string{unreachedddr}),
	})
	store.slow["2"] = 60 * time.Millisecond

	resp := &tailcfg.MapResponse{
		Peers: []*tailcfg.Node{worldNode(2, "100.64.0.2"), worldNode(3, "100.64.0.3")},
	}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))
	require.Len(t, resp.Peers, 2)

	for _, id := range []string{"2", "3"} {
		budget := store.budgetFor(id)
		assert.LessOrEqual(t, budget, registryPeerTimeout,
			"peer %s was handed more than a peer's budget, so the budget is shared", id)
		assert.Greater(t, budget, registryPeerTimeout-50*time.Millisecond,
			"peer %s paid for a slow peer before it", id)
	}
}

// A patch names a peer the client already holds, so it is judged by the same
// visibility rule and not by whether the peer appears in this response.
func TestRewriteDropsPatchesForInvisiblePeers(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr},
			registry.Filter{Src: selfAddr + "/128", Dst: reachedAddr + "/128", Ports: []int{443}}),
		"2": world("2", "acme", "reached", []string{reachedAddr}),
		"3": world("3", "acme", "unreached", []string{unreachedddr}),
	})

	resp := &tailcfg.MapResponse{
		PeersChangedPatch: []*tailcfg.PeerChange{{NodeID: 2}, {NodeID: 3}},
	}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	require.Len(t, resp.PeersChangedPatch, 1)
	assert.Equal(t, tailcfg.NodeID(2), resp.PeersChangedPatch[0].NodeID)
}

// Deltas rely on the nil/empty distinction: an empty Peers list is omitted on
// the wire and means "no change".
func TestRewriteLeavesANilPeerListNil(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr}),
	})

	resp := &tailcfg.MapResponse{Node: worldNode(1, "100.64.0.1")}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	assert.Nil(t, resp.Peers)
	assert.Nil(t, resp.PeersChanged)
}

// A client that advertises a subnet route or an exit node is advertising
// something this head has no way to authorize: the World is the only source of
// a node's addresses, and Gentility renders no routes. So the advertisement is
// ignored — not approved, not rendered — and the node's own AllowedIPs come
// back as exactly its World addresses.
func TestRewriteIgnoresAdvertisedRoutes(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr}),
	})

	node := worldNode(1, "100.64.0.1")
	node.AllowedIPs = []netip.Prefix{
		node.Addresses[0],
		netip.MustParsePrefix("10.0.0.0/24"),
		netip.MustParsePrefix("0.0.0.0/0"),
		netip.MustParsePrefix("::/0"),
	}

	resp := &tailcfg.MapResponse{Node: node}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	assert.Equal(t, []string{selfAddr + "/128"}, allowedIPs(t, resp.Node),
		"a node's AllowedIPs are its World addresses and nothing else")
	assert.Equal(t, []string{selfAddr + "/128"}, prefixes(t, resp.Node))
}

// The same rule seen from the other side: whatever a peer advertised, its
// peers are told only what the World says it holds. A subnet route surviving
// here would route a whole network through a node on the strength of that
// node's own claim.
func TestRewriteGivesPeersOnlyTheirWorldAddresses(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr},
			registry.Filter{Src: selfAddr + "/128", Dst: reachedAddr + "/128", Ports: []int{443}}),
		"2": world("2", "acme", "reached", []string{reachedAddr}),
	})

	peer := worldNode(2, "100.64.0.2")
	peer.AllowedIPs = []netip.Prefix{
		peer.Addresses[0],
		netip.MustParsePrefix("192.168.0.0/16"),
	}

	resp := &tailcfg.MapResponse{Node: worldNode(1, "100.64.0.1"), Peers: []*tailcfg.Node{peer}}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	require.Len(t, resp.Peers, 1)
	assert.Equal(t, []string{reachedAddr + "/128"}, allowedIPs(t, resp.Peers[0]),
		"a peer's advertised subnet route is not the head's to render")
}

// One name, several answers, no type: the record set is what design §13 asks
// for, and each organization the node is named in gets a search domain.
func TestRewriteAppliesWorldDNS(t *testing.T) {
	self := world("1", "acme", "n1", []string{selfAddr, "fd7a:115c:a1e0:2::a"})
	self.Names = append(self.Names, registry.Name{Org: "beta", Label: "n1"})
	self.DNS = []registry.Record{
		{Name: "gentility-overlay.internal", Addr: gatewayAddr},
		{Name: "gentility-overlay.internal", Addr: "fd7a:115c:a1e0:2::1"},
	}

	store := newFakeWorlds(map[string]*registry.World{"1": self})

	resp := &tailcfg.MapResponse{DNSConfig: &tailcfg.DNSConfig{Domains: []string{"stale.example"}}}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	assert.Equal(t,
		[]string{"acme." + baseDomain, "beta." + baseDomain},
		resp.DNSConfig.Domains)

	require.Len(t, resp.DNSConfig.ExtraRecords, 2)
	assert.Equal(t, resp.DNSConfig.ExtraRecords[0].Name, resp.DNSConfig.ExtraRecords[1].Name)
	assert.Equal(t, gatewayAddr, resp.DNSConfig.ExtraRecords[0].Value)

	for _, record := range resp.DNSConfig.ExtraRecords {
		assert.Empty(t, record.Type, "a typed record is silently dropped by the client")
	}
}

// A registry that cannot be reached serves the last World seen. Stale but
// sealed beats fresh but leaky.
func TestRewriteFallsBackToTheCachedWorld(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr}),
	})
	mapper := newTestMapper(store)

	resp := &tailcfg.MapResponse{Node: worldNode(1, "100.64.0.1")}
	require.NoError(t, mapper.rewriteFromRegistry(types.NodeID(1), resp))

	store.errs["1"] = errors.New("registry down")

	again := &tailcfg.MapResponse{Node: worldNode(1, "100.64.0.1")}
	require.NoError(t, mapper.rewriteFromRegistry(types.NodeID(1), again))
	assert.Equal(t, []string{selfAddr + "/128"}, prefixes(t, again.Node))
}

// With nothing cached there is no document to be sealed against, and the
// caller withholds the response entirely.
func TestRewriteWithholdsAResponseItCannotSeal(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{})
	store.errs["1"] = errors.New("registry down")

	resp := &tailcfg.MapResponse{Node: worldNode(1, "100.64.0.1")}
	err := newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no cached world")
}

// A peer's World is read once and remembered: a map build touches every peer,
// and the change stream is what keeps the cache current.
func TestRewriteReadsAPeerWorldOnce(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr},
			registry.Filter{Src: selfAddr + "/128", Dst: reachedAddr + "/128", Ports: []int{443}}),
		"2": world("2", "acme", "reached", []string{reachedAddr}),
	})
	mapper := newTestMapper(store)

	for range 3 {
		resp := &tailcfg.MapResponse{Peers: []*tailcfg.Node{worldNode(2, "100.64.0.2")}}
		require.NoError(t, mapper.rewriteFromRegistry(types.NodeID(1), resp))
		require.Len(t, resp.Peers, 1)
	}

	assert.Equal(t, 1, store.callsFor("2"), "the peer's World is cached")
	assert.Equal(t, 3, store.callsFor("1"), "the node's own World is read fresh")
}

// A document whose shape this head does not know is one it cannot enforce.
func TestRewriteRefusesAnUnknownRendererVersion(t *testing.T) {
	future := world("1", "acme", "n1", []string{selfAddr})
	future.RendererVersion = 99

	store := newFakeWorlds(map[string]*registry.World{"1": future})

	resp := &tailcfg.MapResponse{Node: worldNode(1, "100.64.0.1")}
	err := newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported renderer version")
}

// Without a registry configured the rewrite is not in the path at all.
func TestRewriteIsInertWithoutARegistry(t *testing.T) {
	resp := &tailcfg.MapResponse{Node: worldNode(1, "100.64.0.1"), Peers: []*tailcfg.Node{worldNode(2, "100.64.0.2")}}

	require.NoError(t, (&mapper{}).rewriteFromRegistry(types.NodeID(1), resp))
	assert.Len(t, resp.Peers, 1)
}

// filterRules runs one World's filters through the rewrite and returns the
// rules it produced, which is the only place the head decides what may reach
// what.
func filterRules(t *testing.T, filters ...registry.Filter) []tailcfg.FilterRule {
	t.Helper()

	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr}, filters...),
	})

	resp := &tailcfg.MapResponse{PacketFilters: map[string][]tailcfg.FilterRule{"base": nil}}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	return resp.PacketFilters["base"]
}

// A path names its port (design §8), so the rule it renders admits that port
// and nothing else.
func TestRewriteRendersOneRangePerPort(t *testing.T) {
	rules := filterRules(t,
		registry.Filter{Src: selfAddr + "/128", Dst: gatewayAddr + "/128", Ports: []int{443}})

	require.Len(t, rules, 1)
	assert.Equal(t, []string{selfAddr + "/128"}, rules[0].SrcIPs)
	require.Len(t, rules[0].DstPorts, 1)
	assert.Equal(t, gatewayAddr+"/128", rules[0].DstPorts[0].IP)
	assert.Equal(t, tailcfg.PortRange{First: 443, Last: 443}, rules[0].DstPorts[0].Ports)
}

// Several ports on one path are several ranges on one rule, each a single
// port wide.
func TestRewriteRendersEveryPortOfAPath(t *testing.T) {
	rules := filterRules(t,
		registry.Filter{Src: selfAddr + "/128", Dst: gatewayAddr + "/128", Ports: []int{443, 8443}})

	require.Len(t, rules, 1)
	assert.Equal(t,
		[]tailcfg.NetPortRange{
			{IP: gatewayAddr + "/128", Ports: tailcfg.PortRange{First: 443, Last: 443}},
			{IP: gatewayAddr + "/128", Ports: tailcfg.PortRange{First: 8443, Last: 8443}},
		},
		rules[0].DstPorts)
}

// An empty ports list is a path to NO port. Falling back to any-port would
// turn a renderer that lost a service's port into an open door.
func TestRewriteRendersNoRuleForAPathWithNoPorts(t *testing.T) {
	rules := filterRules(t,
		registry.Filter{Src: selfAddr + "/128", Dst: gatewayAddr + "/128", Ports: nil})

	assert.Empty(t, rules, "a path to no port admits nothing")
}

// The one rule that must never appear, whatever the World said.
func TestRewriteNeverRendersPortRangeAny(t *testing.T) {
	for _, ports := range [][]int{nil, {}, {443}, {443, 8443}, {0}, {65535}} {
		rules := filterRules(t,
			registry.Filter{Src: selfAddr + "/128", Dst: gatewayAddr + "/128", Ports: ports})

		for _, rule := range rules {
			for _, dst := range rule.DstPorts {
				assert.NotEqual(t, tailcfg.PortRangeAny, dst.Ports,
					"ports %v rendered an any-port rule", ports)
			}
		}
	}
}

// A port outside the range a port number has is a rendering fault, and a
// fault renders nothing rather than something narrower or wider than asked.
func TestRewriteDropsAnOutOfRangePort(t *testing.T) {
	rules := filterRules(t,
		registry.Filter{Src: selfAddr + "/128", Dst: gatewayAddr + "/128", Ports: []int{443, 70000}})

	require.Len(t, rules, 1)
	require.Len(t, rules[0].DstPorts, 1)
	assert.Equal(t, tailcfg.PortRange{First: 443, Last: 443}, rules[0].DstPorts[0].Ports)
}

// Addresses in a second organization's /64, for the node that holds grants in
// both.
const (
	betaSelfAddr = "fd7a:115c:a1e0:2::a"
	betaPeerAddr = "fd7a:115c:a1e0:2::b"
)

// twoOrgPeer is a peer holding a live grant in two organizations: an address
// and a name in each, in the renderer's organization order.
func twoOrgPeer() *registry.World {
	return &registry.World{
		RendererVersion: registry.RendererVersion1,
		Revision:        1,
		HeadNodeID:      "2",
		Addresses:       []string{reachedAddr, betaPeerAddr},
		Names: []registry.Name{
			{Org: "acme", Label: "shared"},
			{Org: "beta", Label: "shared"},
		},
	}
}

// The org seam, on the one surface that can leak across it: a node sees a
// peer through the organization it shares with it, and learns neither the
// address the peer holds in another tenant nor the name it answers to there.
func TestRewriteShowsAPeerOnlyInTheOrgTheFilterNames(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr},
			registry.Filter{Src: selfAddr + "/128", Dst: reachedAddr + "/128", Ports: []int{443}}),
		"2": twoOrgPeer(),
	})

	resp := &tailcfg.MapResponse{Peers: []*tailcfg.Node{worldNode(2, "100.64.0.2")}}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	require.Len(t, resp.Peers, 1)
	assert.Equal(t, []string{reachedAddr + "/128"}, prefixes(t, resp.Peers[0]),
		"the address in an organization this node holds no path into is not its business")
	assert.Equal(t, "shared.acme."+baseDomain+".", resp.Peers[0].Name,
		"the name is the one the peer answers to in the shared organization")
}

// The same peer, seen from the other side of the seam: the other tenant sees
// the other address and the other name, and neither tenant sees both.
func TestRewriteShowsTheOtherOrgTheOtherHalf(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "beta", "n1", []string{betaSelfAddr},
			registry.Filter{Src: betaSelfAddr + "/128", Dst: betaPeerAddr + "/128", Ports: []int{443}}),
		"2": twoOrgPeer(),
	})

	resp := &tailcfg.MapResponse{Peers: []*tailcfg.Node{worldNode(2, "100.64.0.2")}}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	require.Len(t, resp.Peers, 1)
	assert.Equal(t, []string{betaPeerAddr + "/128"}, prefixes(t, resp.Peers[0]))
	assert.Equal(t, "shared.beta."+baseDomain+".", resp.Peers[0].Name)
}

// Gentility composes the fully-qualified name itself. Where it sends one it is
// the answer, because it is the renderer and not this head that knows what a
// name is made of.
func TestRewritePrefersTheRenderedFqdn(t *testing.T) {
	peer := twoOrgPeer()
	peer.Names[0].Fqdn = "rendered.acme.elsewhere.example."

	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr},
			registry.Filter{Src: selfAddr + "/128", Dst: reachedAddr + "/128", Ports: []int{443}}),
		"2": peer,
	})

	resp := &tailcfg.MapResponse{Peers: []*tailcfg.Node{worldNode(2, "100.64.0.2")}}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	require.Len(t, resp.Peers, 1)
	assert.Equal(t, "rendered.acme.elsewhere.example.", resp.Peers[0].Name)
}

// A name assembled from nothing is not a name: with no name for the
// organization the peer is seen through, the head's own stays.
func TestRewriteKeepsTheHeadsNameWhenNoOrgNameMatches(t *testing.T) {
	peer := twoOrgPeer()
	peer.Names = nil

	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr},
			registry.Filter{Src: selfAddr + "/128", Dst: reachedAddr + "/128", Ports: []int{443}}),
		"2": peer,
	})

	resp := &tailcfg.MapResponse{Peers: []*tailcfg.Node{worldNode(2, "100.64.0.2")}}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	require.Len(t, resp.Peers, 1)
	assert.Equal(t, "stale.example.", resp.Peers[0].Name)
}

// Withholding is silent by construction — the async path logs and drops, and
// the node simply keeps the netmap it has — so the counter is the only way an
// operator sees it happening at all.
func TestRewriteCountsAWithheldResponse(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{})
	store.errs["1"] = errors.New("registry down")

	before := testutil.ToFloat64(registryRewriteWithheld)

	resp := &tailcfg.MapResponse{Node: worldNode(1, "100.64.0.1")}
	require.Error(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	assert.InDelta(t, before+1, testutil.ToFloat64(registryRewriteWithheld), 0.0,
		"a withheld response must be countable")
}

// A rewrite that succeeds counts nothing.
func TestRewriteCountsNothingWhenItSeals(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr}),
	})

	before := testutil.ToFloat64(registryRewriteWithheld)

	resp := &tailcfg.MapResponse{Node: worldNode(1, "100.64.0.1")}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	assert.InDelta(t, before, testutil.ToFloat64(registryRewriteWithheld), 0.0)
}

// The DERP map is configuration, and Gentility renders it from the same config
// that renders everything else. A head that kept its own would send nodes to
// relays the overlay does not run.
func TestRewriteAppliesTheWorldDERPMap(t *testing.T) {
	self := world("1", "acme", "n1", []string{selfAddr})
	self.DERPMap = json.RawMessage(
		`{"Regions":{"1":{"RegionID":1,"RegionCode":"overlay","Nodes":[` +
			`{"Name":"1a","RegionID":1,"HostName":"derp.overlay.example","DERPPort":443}]}}}`)

	store := newFakeWorlds(map[string]*registry.World{"1": self})

	resp := &tailcfg.MapResponse{
		Node:    worldNode(1, "100.64.0.1"),
		DERPMap: &tailcfg.DERPMap{Regions: map[int]*tailcfg.DERPRegion{9: {RegionCode: "stale"}}},
	}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	require.NotNil(t, resp.DERPMap)
	require.Len(t, resp.DERPMap.Regions, 1)

	region := resp.DERPMap.Regions[1]
	require.NotNil(t, region, "the head's own regions are replaced, not merged")
	assert.Equal(t, "overlay", region.RegionCode)
	require.Len(t, region.Nodes, 1)
	assert.Equal(t, "derp.overlay.example", region.Nodes[0].HostName)
}

// A World that carries no derp_map is not an instruction to have no relays.
func TestRewriteLeavesTheDERPMapAloneWhenTheWorldSendsNone(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr}),
	})

	own := &tailcfg.DERPMap{Regions: map[int]*tailcfg.DERPRegion{9: {RegionCode: "embedded"}}}

	resp := &tailcfg.MapResponse{Node: worldNode(1, "100.64.0.1"), DERPMap: own}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	assert.Same(t, own, resp.DERPMap)
}

// An explicit null is the same absence, and a document this head cannot parse
// is not a reason to strand every node with no relay at all.
func TestRewriteKeepsTheDERPMapOnAnUnreadableOne(t *testing.T) {
	for _, raw := range []string{
		`null`,
		`"not a derp map"`,

		// An EMPTY region set. It parses cleanly into a non-nil map, so
		// the nil check alone let it through and installed a relay list
		// with no relays in it — a node with a stale relay can still
		// reach one, and a node with none can reach nothing.
		`{"Regions":{}}`,
		`{}`,
	} {
		self := world("1", "acme", "n1", []string{selfAddr})
		self.DERPMap = json.RawMessage(raw)

		store := newFakeWorlds(map[string]*registry.World{"1": self})
		own := &tailcfg.DERPMap{Regions: map[int]*tailcfg.DERPRegion{9: {RegionCode: "embedded"}}}

		resp := &tailcfg.MapResponse{Node: worldNode(1, "100.64.0.1"), DERPMap: own}
		require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

		assert.Same(t, own, resp.DERPMap, "derp_map %s", raw)
	}
}

// The World names the space its names live in, and that is the answer the
// head uses. See [baseDomainOf]: `dns.base_domain` is the fallback for a
// Gentility that does not send one, not a second opinion.
func TestRewriteUsesTheWorldsBaseDomain(t *testing.T) {
	const rendered = "rendered.example"

	self := world("1", "acme", "n1", []string{selfAddr})
	self.BaseDomain = rendered
	self.Names[0].Fqdn = ""

	store := newFakeWorlds(map[string]*registry.World{"1": self})

	resp := &tailcfg.MapResponse{
		Node:      worldNode(1, "100.64.0.1"),
		DNSConfig: &tailcfg.DNSConfig{Domains: []string{"stale.example"}},
	}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	assert.Equal(t, []string{"acme." + rendered}, resp.DNSConfig.Domains,
		"the search domain is composed under the World's own base domain")
	assert.Equal(t, "n1.acme."+rendered+".", resp.Node.Name,
		"the composed name falls back under the World's own base domain")
}

// A peer is named under ITS OWN World's base domain, not the viewer's: the
// document that names a node is the document that says what space it is
// named in.
func TestRewriteNamesAPeerUnderItsOwnBaseDomain(t *testing.T) {
	const peerDomain = "peer.example"

	self := world("1", "acme", "n1", []string{selfAddr},
		registry.Filter{Src: selfAddr + "/128", Dst: reachedAddr + "/128", Ports: []int{443}})

	peerWorld := world("2", "acme", "reached", []string{reachedAddr})
	peerWorld.BaseDomain = peerDomain

	store := newFakeWorlds(map[string]*registry.World{"1": self, "2": peerWorld})

	resp := &tailcfg.MapResponse{
		Node:  worldNode(1, "100.64.0.1"),
		Peers: []*tailcfg.Node{worldNode(2, "100.64.0.2")},
	}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	require.Len(t, resp.Peers, 1)
	assert.Equal(t, "reached.acme."+peerDomain+".", resp.Peers[0].Name)
}

// A World that carries no base domain is a Gentility that does not send one
// yet, and the head's own dns.base_domain composes the name instead.
func TestRewriteFallsBackToTheConfiguredBaseDomain(t *testing.T) {
	self := world("1", "acme", "n1", []string{selfAddr})

	store := newFakeWorlds(map[string]*registry.World{"1": self})

	resp := &tailcfg.MapResponse{
		Node:      worldNode(1, "100.64.0.1"),
		DNSConfig: &tailcfg.DNSConfig{},
	}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	assert.Equal(t, []string{"acme." + baseDomain}, resp.DNSConfig.Domains)
	assert.Equal(t, "n1.acme."+baseDomain+".", resp.Node.Name)
}

// The two must be equal (config-example says so), and a head that disagrees
// with the registry composes hostnames nothing resolves. It is not fatal —
// the World's own answer is used and the netmap is still sealed — so it is
// reported once, at Error, rather than per map build.
func TestRewriteReportsABaseDomainDisagreement(t *testing.T) {
	logs := captureLogs(t)

	self := world("1", "acme", "n1", []string{selfAddr})
	self.BaseDomain = "somewhere.else"

	store := newFakeWorlds(map[string]*registry.World{"1": self})
	mapper := newTestMapper(store)

	for range 3 {
		resp := &tailcfg.MapResponse{Node: worldNode(1, "100.64.0.1")}
		require.NoError(t, mapper.rewriteFromRegistry(types.NodeID(1), resp))
	}

	assert.Equal(t, 1, strings.Count(logs.String(), `"level":"error"`),
		"the disagreement is reported once, not once per map build")
	assert.Contains(t, logs.String(), "somewhere.else")
	assert.Contains(t, logs.String(), baseDomain)
}

// Agreement is silence.
func TestRewriteIsSilentWhenTheBaseDomainsAgree(t *testing.T) {
	logs := captureLogs(t)

	self := world("1", "acme", "n1", []string{selfAddr})
	self.BaseDomain = baseDomain

	store := newFakeWorlds(map[string]*registry.World{"1": self})

	resp := &tailcfg.MapResponse{Node: worldNode(1, "100.64.0.1")}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	assert.NotContains(t, logs.String(), `"level":"error"`)
}

// captureLogs points the global zerolog logger at a buffer for one test. The
// rewrite reports through the package-level logger, which is the only place
// its warnings and this Error can be read from. The global level is lowered
// with it, since a line the filter swallowed reads exactly like a line that
// was never written.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer

	previous := log.Logger
	previousLevel := zerolog.GlobalLevel()

	log.Logger = zerolog.New(&buf)
	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	t.Cleanup(func() {
		log.Logger = previous
		zerolog.SetGlobalLevel(previousLevel)
	})

	return &buf
}

// A path this head cannot read is a path it cannot enforce.
//
// [visibleAddresses] already skips a prefix it cannot parse, so a filter with
// a malformed endpoint made nothing visible — and yet the rule was rendered
// anyway, out of the same unparsed string. That is the head guessing at what
// Gentility meant on the one surface whose whole purpose is that it does not.
// Both halves now read the World the same way.
func TestRewriteDropsAPathWithAnUnparsableEndpoint(t *testing.T) {
	for _, broken := range []registry.Filter{
		{Src: "not-an-address", Dst: gatewayAddr + "/128", Ports: []int{443}},
		{Src: selfAddr + "/128", Dst: "not-an-address", Ports: []int{443}},

		// A bare address is not a prefix, and [visibleAddresses] does
		// not accept one either.
		{Src: selfAddr, Dst: gatewayAddr + "/128", Ports: []int{443}},
		{Src: selfAddr + "/128", Dst: gatewayAddr, Ports: []int{443}},
	} {
		rules := filterRules(t, broken)
		assert.Empty(t, rules, "filter %+v", broken)
	}
}

// One malformed path does not cost the rest of them: the World is not
// discarded, only the edge nobody can read.
func TestRewriteKeepsTheReadablePathsBesideABrokenOne(t *testing.T) {
	rules := filterRules(t,
		registry.Filter{Src: "not-an-address", Dst: gatewayAddr + "/128", Ports: []int{443}},
		registry.Filter{Src: selfAddr + "/128", Dst: gatewayAddr + "/128", Ports: []int{8443}})

	require.Len(t, rules, 1)
	assert.Equal(t, []string{selfAddr + "/128"}, rules[0].SrcIPs)
	assert.Equal(t, tailcfg.PortRange{First: 8443, Last: 8443}, rules[0].DstPorts[0].Ports)
}
