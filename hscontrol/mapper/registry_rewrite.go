package mapper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"time"

	"github.com/juanfont/headscale/hscontrol/registry"
	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
	"tailscale.com/tailcfg"
)

const (
	// registrySelfTimeout bounds the fetch of the node's OWN World, the
	// document this map is being rewritten from. Nothing can be sent
	// without it.
	registrySelfTimeout = 3 * time.Second

	// registryPeerTimeout is what each peer gets, on a context of its own.
	// One deadline shared across a build would hand the last peer in a
	// large netmap whatever the peers before it left over, so a single slow
	// answer would silently drop the tail of the list — and which peers
	// those were would depend on the order they happened to be in.
	registryPeerTimeout = 2 * time.Second

	// defaultWorldTTL is how long a cached World is believed without asking
	// again. The change stream is what SHOULD invalidate an entry (B6, via
	// [Batcher.InvalidateWorld]); the TTL is the floor under a stream that
	// is down, so a cache cannot quietly serve a document that moved an
	// hour ago.
	defaultWorldTTL = 60 * time.Second
)

// registryRewriteWithheld counts the map responses the rewrite refused to let
// out. See the fail-closed note on [mapper.rewriteFromRegistry]: the node
// keeps its last netmap and is told nothing, so this counter is the whole of
// the signal.
var registryRewriteWithheld = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: "headscale",
	Name:      "registry_rewrite_withheld_total",
	Help:      "total count of map responses withheld because no sealed world could be applied",
})

// cachedWorld is one remembered document and when it was read, since a copy of
// something that moves is only as good as its age.
type cachedWorld struct {
	world *registry.World
	at    time.Time
}

// peerFate is what one round of the rewrite can say about one peer.
type peerFate int

const (
	// peerKnown: the World was read, or a cached one stands in for it.
	peerKnown peerFate = iota

	// peerRefused: Gentility answered, and the answer was that there is no
	// World here — unknown, revoked, nothing rendered, or a shape this head
	// cannot enforce. The peer is dropped and the drop is announced.
	peerRefused

	// peerUnreached: no answer arrived, and nothing is cached. The round
	// says NOTHING about the peer: it is left out of this response and out
	// of PeersRemoved, because announcing a removal would tell the client a
	// revocation happened on the evidence of a timeout.
	peerUnreached
)

// rewriteFromRegistry makes Gentility authoritative for topology,
// addressing, naming, DNS, and filters: the World contract is applied to
// every outbound MapResponse. The wire head stays authoritative for
// transport (endpoints, disco keys, online).
//
// Fail-closed: on registry errors the last known world for the node is
// used; with no cached world the response is withheld entirely — a
// stale-but-sealed netmap beats a fresh one that violates the org
// seam.
//
// # What "withheld" means to the node
//
// An error here fails the whole map response. On the ASYNC path — every
// change after the first map — [generateMapResponse] returns that error,
// [Batcher] logs it, and the change is DROPPED. Nothing is retried and nothing
// is sent, so the node simply keeps the netmap it already has until some later
// change is rewritten successfully. That is the fail-closed choice made
// visible: a node holding a slightly old sealed netmap is a node that is still
// only talking to peers a reach grant allowed, whereas sending the head's own
// unrewritten response would publish every device on the head to every other.
//
// It is not free, and it is silent by construction: no client sees an error
// and no reconnect is triggered, so a registry that stays unreachable leaves
// every node frozen at whatever it last held. [registryRewriteWithheld] is how
// an operator sees it happening — a rising counter is the only symptom, and
// the alert belongs on it rather than on the logs.
//
// The SYNCHRONOUS path is different: the initial map for a node that has just
// connected has no last netmap to keep, so the error reaches the caller and
// the session fails, which is the honest answer to "I cannot seal your first
// map".
//
// # A World is keyed by the head's own node id
//
// Not by the node key, which the v0 rewrite used: a key rotates, and a
// document keyed by one would move when nothing about the node did. The id
// the head allocates is the node's identity on this channel from the moment
// it exists, which is also why the registration callback belongs after the
// insert that allocates it — see [registry.NodeID].
//
// # Peers are derived from the filters
//
// The World carries no peer list (master §2.4). The visible peer set is
// every address named as a filter's source or destination, less this node's
// own; a peer is kept only when its OWN World holds one of those addresses,
// and its addresses and names are read from that World. So a peer nothing
// can reach is a peer this node is never told about, and there is no second
// place where a peer's identity is written down.
func (m *mapper) rewriteFromRegistry(nodeID types.NodeID, resp *tailcfg.MapResponse) error {
	if m.regClient == nil || resp == nil {
		return nil
	}

	world, err := m.fetchWorld(nodeID)
	if err != nil {
		registryRewriteWithheld.Inc()

		return err
	}

	visible := visibleAddresses(world)

	if resp.Node != nil {
		applyWorldAddresses(resp.Node, world.Addresses)
		applyWorldName(resp.Node, world.Names, 0, m.baseDomain())
	}

	// Peers dropped by the rewrite must be announced in PeersRemoved:
	// an empty Peers list is omitted on the wire and means "no
	// change", so silence would leave stale peers on the client.
	var removed []tailcfg.NodeID

	resp.Peers, removed = m.rewriteWorldPeers(resp.Peers, visible, removed)
	resp.PeersChanged, removed = m.rewriteWorldPeers(resp.PeersChanged, visible, removed)
	resp.PeersRemoved = append(resp.PeersRemoved, removed...)
	resp.PeersChangedPatch = m.keepVisiblePatches(resp.PeersChangedPatch, visible)

	applyWorldDNS(resp, world, m.baseDomain())
	applyWorldFilters(resp, world)
	applyWorldDERPMap(resp, world)

	return nil
}

func (m *mapper) baseDomain() string {
	if m.cfg == nil {
		return ""
	}

	return m.cfg.BaseDomain
}

// fetchWorld reads a node's own World fresh, because it is the document this
// map is being rewritten from. A registry that cannot be reached degrades to
// the last World seen for that node; a node with no cached World has no
// document to be sealed against, and the caller withholds the response.
func (m *mapper) fetchWorld(nodeID types.NodeID) (*registry.World, error) {
	ctx, cancel := context.WithTimeout(context.Background(), registrySelfTimeout)
	defer cancel()

	world, err := m.regClient.World(ctx, registry.NodeID(nodeID.Uint64()))
	if err == nil {
		if verr := supportedWorld(world); verr != nil {
			return nil, verr
		}

		m.cacheWorld(nodeID, world)

		return world, nil
	}

	cached, _ := m.cachedWorld(nodeID)
	if cached == nil {
		return nil, fmt.Errorf("registry unreachable and no cached world for node %d: %w", nodeID, err)
	}

	log.Warn().
		Err(err).
		Uint64("node.id", nodeID.Uint64()).
		Msg("registry world fetch failed; serving cached world")

	return cached, nil
}

// peerWorld reads a peer's World, cache first: a map build touches every peer
// and the change stream keeps the cache current, so a fetch per peer per map
// would be the same document asked for over and over. A cached entry past its
// TTL is re-read, and stood in for only if the read fails.
//
// # A refusal and an outage are different facts
//
// Gentility saying there is no World here ([registry.ErrUnknownNode],
// [registry.ErrNotRendered]) is an ANSWER, and the peer is dropped and the drop
// announced. A read that did not complete — a timeout, a 5xx, a 429 — is not an
// answer at all: with a cached document the peer is served as last seen, and
// with none this round says nothing about it. Treating the second as the first
// would have every registry hiccup tell every client that its peers had been
// revoked, and a netmap rebuilt from a lie is worse than one rebuilt late.
func (m *mapper) peerWorld(nodeID types.NodeID) (*registry.World, peerFate) {
	cached, fresh := m.cachedWorld(nodeID)
	if fresh {
		return cached, peerKnown
	}

	ctx, cancel := context.WithTimeout(context.Background(), registryPeerTimeout)
	defer cancel()

	world, err := m.regClient.World(ctx, registry.NodeID(nodeID.Uint64()))
	if err != nil {
		if errors.Is(err, registry.ErrUnknownNode) || errors.Is(err, registry.ErrNotRendered) {
			log.Info().
				Err(err).
				Uint64("peer.id", nodeID.Uint64()).
				Msg("registry has no world for peer; dropping it from the netmap")

			m.invalidateWorld(nodeID)

			return nil, peerRefused
		}

		if cached != nil {
			log.Warn().
				Err(err).
				Uint64("peer.id", nodeID.Uint64()).
				Msg("registry world fetch failed for peer; serving it as last seen")

			return cached, peerKnown
		}

		log.Warn().
			Err(err).
			Uint64("peer.id", nodeID.Uint64()).
			Msg("registry world fetch failed for peer and none is cached; saying nothing about it")

		return nil, peerUnreached
	}

	if err := supportedWorld(world); err != nil {
		// A shape this head cannot enforce is a refusal rather than an
		// outage: reading it again would answer the same.
		log.Warn().Err(err).Uint64("peer.id", nodeID.Uint64()).Msg("dropping peer")

		m.invalidateWorld(nodeID)

		return nil, peerRefused
	}

	m.cacheWorld(nodeID, world)

	return world, peerKnown
}

// supportedWorld refuses a renderer version this head was not built against.
// A document whose shape we do not know is one we cannot enforce, and
// enforcing it wrongly is worse than serving nothing.
func supportedWorld(world *registry.World) error {
	if world.RendererVersion != registry.RendererVersion1 {
		return fmt.Errorf("registry: unsupported renderer version %d", world.RendererVersion)
	}

	return nil
}

// cachedWorld returns the remembered World and whether it is still inside its
// TTL. A stale entry is still returned: it is what a failed re-read falls back
// on.
func (m *mapper) cachedWorld(nodeID types.NodeID) (*registry.World, bool) {
	m.worldCacheMu.Lock()
	defer m.worldCacheMu.Unlock()

	entry, ok := m.worldCache[nodeID]
	if !ok {
		return nil, false
	}

	return entry.world, m.now().Sub(entry.at) < m.worldTTL
}

func (m *mapper) cacheWorld(nodeID types.NodeID, world *registry.World) {
	m.worldCacheMu.Lock()
	defer m.worldCacheMu.Unlock()

	m.worldCache[nodeID] = cachedWorld{world: world, at: m.now()}
}

// invalidateWorld forgets one node's cached World. It is the seam the change
// stream drives: an event names a node, and the next map build reads that
// node's document afresh instead of waiting out [defaultWorldTTL].
func (m *mapper) invalidateWorld(nodeID types.NodeID) {
	m.worldCacheMu.Lock()
	defer m.worldCacheMu.Unlock()

	delete(m.worldCache, nodeID)
}

// visibleAddresses is the peer set, derived: every address a filter names as
// a source or a destination, less the node's own. A filter is the only thing
// that can put an address here, so a node sees exactly what it has a rendered
// path to or from.
func visibleAddresses(world *registry.World) map[netip.Addr]bool {
	visible := make(map[netip.Addr]bool, 2*len(world.Filters))

	for _, filter := range world.Filters {
		for _, prefix := range []string{filter.Src, filter.Dst} {
			addr, err := netip.ParsePrefix(prefix)
			if err != nil {
				log.Warn().Str("prefix", prefix).Msg("world filter carried an unparsable prefix")

				continue
			}

			visible[addr.Addr()] = true
		}
	}

	for _, own := range world.Addresses {
		addr, err := netip.ParseAddr(own)
		if err != nil {
			log.Warn().Str("addr", own).Msg("world carried an unparsable address")

			continue
		}

		delete(visible, addr)
	}

	return visible
}

// visibleThrough intersects a peer's own addresses with the set this node has
// a path to, and returns them with the index of the first — which is the index
// of the ORGANIZATION the peer is seen through, since a World lists its
// addresses and its names in the same organization order.
//
// This is the org seam, on the one surface that can leak across it. A node
// holding grants in two organizations has an address and a name in each, and a
// viewer sharing only one of them has no business learning either half of the
// other: handing the peer's whole World to the netmap would publish the other
// tenant's address, and naming it after its primary organization would publish
// that tenant's existence.
//
// An index of -1 means nothing intersected, and the caller drops the peer.
func visibleThrough(world *registry.World, visible map[netip.Addr]bool) ([]string, int) {
	var (
		seen  []string
		first = -1
	)

	for index, own := range world.Addresses {
		addr, err := netip.ParseAddr(own)
		if err != nil || !visible[addr] {
			continue
		}

		if first < 0 {
			first = index
		}

		seen = append(seen, own)
	}

	return seen, first
}

// applyWorldDNS installs the search domains for every organization the node
// is named in, and the World's own records for the platform hostname.
//
// The records carry a NAME and an ADDRESS and no type (design §13): the
// pinned client ignores an extra record whose type is non-empty, so the v0
// rewrite's "AAAA" was silently dropped. Several records may share one name —
// one per organization, each naming that organization's own gateway — and
// that is the point rather than a duplication.
func applyWorldDNS(resp *tailcfg.MapResponse, world *registry.World, baseDomain string) {
	if resp.DNSConfig == nil {
		return
	}

	resp.DNSConfig.Domains = searchDomains(world.Names, baseDomain)

	records := make([]tailcfg.DNSRecord, 0, len(world.DNS))
	for _, record := range world.DNS {
		records = append(records, tailcfg.DNSRecord{Name: record.Name, Value: record.Addr})
	}

	resp.DNSConfig.ExtraRecords = records
}

// searchDomains is one domain per organization the node is named in, in the
// order the World lists them — which is the renderer's organization order, so
// a node's primary organization is searched first.
func searchDomains(names []registry.Name, baseDomain string) []string {
	if baseDomain == "" {
		return nil
	}

	domains := make([]string, 0, len(names))

	for _, name := range names {
		domain := name.Org + "." + baseDomain
		if !slices.Contains(domains, domain) {
			domains = append(domains, domain)
		}
	}

	return domains
}

// applyWorldFilters replaces the head's packet filter with the World's rules:
// traffic flows where a reach grant says it may and nowhere else.
//
// A path names its port. Design §8 renders an edge from a reach grant and the
// service it points at, so the port is part of the path rather than a detail
// left open, and each one becomes its own single-port range. There is no
// [tailcfg.PortRangeAny] anywhere in here: a World that lost a port would then
// render an open door in place of a closed one, which is the one failure this
// filter exists to prevent.
func applyWorldFilters(resp *tailcfg.MapResponse, world *registry.World) {
	if resp.PacketFilters == nil {
		return
	}

	rules := make([]tailcfg.FilterRule, 0, len(world.Filters))

	for _, f := range world.Filters {
		ports := portRanges(f)

		// An empty ports list is a path to NO port, and renders nothing.
		if len(ports) == 0 {
			continue
		}

		rules = append(rules, tailcfg.FilterRule{SrcIPs: []string{f.Src}, DstPorts: ports})
	}

	resp.PacketFilters = map[string][]tailcfg.FilterRule{"base": rules}
}

// portRanges is one single-port range per port the path names, at the path's
// destination. A number outside the range a port number has is a rendering
// fault: it is dropped and logged, because widening or narrowing it would be
// this head inventing a rule Gentility did not write.
func portRanges(f registry.Filter) []tailcfg.NetPortRange {
	ranges := make([]tailcfg.NetPortRange, 0, len(f.Ports))

	for _, port := range f.Ports {
		if port < 0 || port > math.MaxUint16 {
			log.Warn().
				Int("port", port).
				Str("dst", f.Dst).
				Msg("world filter named a port outside the port range")

			continue
		}

		ranges = append(ranges, tailcfg.NetPortRange{
			IP:    f.Dst,
			Ports: tailcfg.PortRange{First: uint16(port), Last: uint16(port)},
		})
	}

	return ranges
}

// applyWorldDERPMap installs the relays Gentility renders from config
// (master §2.4: derp_map is configuration, passed through verbatim). A head
// left holding its own would send nodes to relays the overlay does not run.
//
// A World that carries no derp_map is not an instruction to have no relays,
// and neither is one this head cannot parse: both leave the head's own map
// standing, because a node with a stale relay list can still reach a relay and
// a node with none cannot reach anything.
func applyWorldDERPMap(resp *tailcfg.MapResponse, world *registry.World) {
	if len(world.DERPMap) == 0 {
		return
	}

	var derpMap tailcfg.DERPMap
	if err := json.Unmarshal(world.DERPMap, &derpMap); err != nil {
		log.Warn().Err(err).Msg("world carried an unreadable derp map; keeping the head's own")

		return
	}

	// "null" parses without error into the zero value; it is an absent map
	// rather than an empty one.
	if derpMap.Regions == nil && derpMap.HomeParams == nil {
		return
	}

	resp.DERPMap = &derpMap
}

// applyWorldName names a node inside one organization: index 0 for a node
// looking at itself, since the World lists organizations in the renderer's
// order and the first is the node's primary one, and for a peer the
// organization the viewer shares with it.
//
// A node with no name for that organization keeps whatever the head gave it,
// because a name assembled from nothing is not a name.
func applyWorldName(node *tailcfg.Node, names []registry.Name, index int, baseDomain string) {
	if name := nameAt(names, index, baseDomain); name != "" {
		node.Name = name
	}
}

// nameAt is the fully-qualified name at one index of a World's names.
//
// Gentility sends the composed name in Fqdn where it has one, and that is the
// answer: the renderer is what knows what a name is made of. Composing
// "<label>.<org>.<base_domain>." here is the fallback for a Gentility that
// does not send it yet, and it needs a base domain to compose from.
func nameAt(names []registry.Name, index int, baseDomain string) string {
	if index < 0 || index >= len(names) {
		return ""
	}

	name := names[index]
	if name.Fqdn != "" {
		return name.Fqdn
	}

	if baseDomain == "" {
		return ""
	}

	return name.Label + "." + name.Org + "." + baseDomain + "."
}

// rewriteWorldPeers keeps the peers the World's filters make visible, reading
// each one's addresses and names from that peer's own World, and records the
// rest in removed. A nil slice stays nil — deltas rely on the nil/empty
// distinction.
func (m *mapper) rewriteWorldPeers(
	peers []*tailcfg.Node,
	visible map[netip.Addr]bool,
	removed []tailcfg.NodeID,
) ([]*tailcfg.Node, []tailcfg.NodeID) {
	if peers == nil {
		return nil, removed
	}

	kept := peers[:0]

	for _, peer := range peers {
		world, fate := m.peerWorld(types.NodeID(peer.ID)) //nolint:gosec // IDs are bounded
		if fate == peerUnreached {
			// Neither kept nor announced: this round has nothing to
			// say about a peer it could not read.
			continue
		}

		if fate == peerRefused {
			removed = append(removed, peer.ID)

			continue
		}

		seen, org := visibleThrough(world, visible)
		if len(seen) == 0 {
			removed = append(removed, peer.ID)

			continue
		}

		applyWorldAddresses(peer, seen)
		applyWorldName(peer, world.Names, org, m.baseDomain())
		kept = append(kept, peer)
	}

	return kept, removed
}

// keepVisiblePatches drops a delta for a peer the World does not make
// visible. A patch names a peer the client already holds, so it is judged by
// the same rule the peer itself was, and never by whether it happens to
// appear in this response.
func (m *mapper) keepVisiblePatches(
	patches []*tailcfg.PeerChange,
	visible map[netip.Addr]bool,
) []*tailcfg.PeerChange {
	if len(patches) == 0 {
		return patches
	}

	kept := patches[:0]

	for _, patch := range patches {
		world, _ := m.peerWorld(types.NodeID(patch.NodeID)) //nolint:gosec // IDs are bounded
		if world == nil {
			continue
		}

		if seen, _ := visibleThrough(world, visible); len(seen) > 0 {
			kept = append(kept, patch)
		}
	}

	return kept
}

// applyWorldAddresses replaces a node's self addresses while preserving
// its non-self AllowedIPs entries (subnet and exit routes).
func applyWorldAddresses(node *tailcfg.Node, addrs []string) {
	selfPrefixes := make([]netip.Prefix, 0, len(addrs))

	for _, a := range addrs {
		addr, err := netip.ParseAddr(a)
		if err != nil {
			log.Warn().Str("addr", a).Msg("registry world contained unparsable address")

			continue
		}

		selfPrefixes = append(selfPrefixes, netip.PrefixFrom(addr, addr.BitLen()))
	}

	oldSelf := make(map[netip.Prefix]bool, len(node.Addresses))
	for _, p := range node.Addresses {
		oldSelf[p] = true
	}

	var routes []netip.Prefix

	for _, p := range node.AllowedIPs {
		if !oldSelf[p] {
			routes = append(routes, p)
		}
	}

	node.Addresses = selfPrefixes
	node.AllowedIPs = append(selfPrefixes[:len(selfPrefixes):len(selfPrefixes)], routes...)
}
