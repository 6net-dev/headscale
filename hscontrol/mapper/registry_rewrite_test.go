package mapper

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"

	"github.com/juanfont/headscale/hscontrol/registry"
	"github.com/juanfont/headscale/hscontrol/types"
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
}

func (f *fakeWorlds) World(_ context.Context, headNodeID string) (*registry.World, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls[headNodeID]++

	if err, ok := f.errs[headNodeID]; ok {
		return nil, err
	}

	world, ok := f.worlds[headNodeID]
	if !ok {
		return nil, registry.ErrUnknownNode
	}

	return world, nil
}

func (f *fakeWorlds) callsFor(headNodeID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls[headNodeID]
}

func newFakeWorlds(worlds map[string]*registry.World) *fakeWorlds {
	return &fakeWorlds{
		worlds: worlds,
		errs:   map[string]error{},
		calls:  map[string]int{},
	}
}

func newTestMapper(store worldSource) *mapper {
	return &mapper{
		cfg:        &types.Config{BaseDomain: baseDomain},
		regClient:  store,
		worldCache: map[types.NodeID]*registry.World{},
	}
}

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

// A peer this head cannot place is a peer it cannot prove is reachable, so it
// is dropped rather than passed through — the same fail-closed rule the node's
// own World follows.
func TestRewriteDropsAPeerWhoseWorldCannotBeRead(t *testing.T) {
	store := newFakeWorlds(map[string]*registry.World{
		"1": world("1", "acme", "n1", []string{selfAddr},
			registry.Filter{Src: selfAddr + "/128", Dst: reachedAddr + "/128", Ports: []int{443}}),
	})
	store.errs["2"] = errors.New("registry down")

	resp := &tailcfg.MapResponse{Peers: []*tailcfg.Node{worldNode(2, "100.64.0.2")}}
	require.NoError(t, newTestMapper(store).rewriteFromRegistry(types.NodeID(1), resp))

	assert.Empty(t, resp.Peers)
	assert.Equal(t, []tailcfg.NodeID{2}, resp.PeersRemoved)
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
