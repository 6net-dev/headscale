package mapper

import (
	"context"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"time"

	"github.com/juanfont/headscale/hscontrol/registry"
	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/rs/zerolog/log"
	"tailscale.com/tailcfg"
)

const registryWorldTimeout = 3 * time.Second

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

	ctx, cancel := context.WithTimeout(context.Background(), registryWorldTimeout)
	defer cancel()

	world, err := m.fetchWorld(ctx, nodeID)
	if err != nil {
		return err
	}

	visible := visibleAddresses(world)

	if resp.Node != nil {
		applyWorldAddresses(resp.Node, world.Addresses)
		applyWorldName(resp.Node, world.Names, m.baseDomain())
	}

	// Peers dropped by the rewrite must be announced in PeersRemoved:
	// an empty Peers list is omitted on the wire and means "no
	// change", so silence would leave stale peers on the client.
	var removed []tailcfg.NodeID

	resp.Peers, removed = m.rewriteWorldPeers(ctx, resp.Peers, visible, removed)
	resp.PeersChanged, removed = m.rewriteWorldPeers(ctx, resp.PeersChanged, visible, removed)
	resp.PeersRemoved = append(resp.PeersRemoved, removed...)
	resp.PeersChangedPatch = m.keepVisiblePatches(ctx, resp.PeersChangedPatch, visible)

	applyWorldDNS(resp, world, m.baseDomain())
	applyWorldFilters(resp, world)

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
func (m *mapper) fetchWorld(ctx context.Context, nodeID types.NodeID) (*registry.World, error) {
	world, err := m.regClient.World(ctx, registry.NodeID(nodeID.Uint64()))
	if err == nil {
		if verr := supportedWorld(world); verr != nil {
			return nil, verr
		}

		m.cacheWorld(nodeID, world)

		return world, nil
	}

	cached := m.cachedWorld(nodeID)
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
// would be the same document asked for over and over.
//
// A nil answer means this head cannot say where the peer lives, and the
// caller drops it rather than guessing — the same fail-closed rule the node's
// own World follows.
func (m *mapper) peerWorld(ctx context.Context, nodeID types.NodeID) *registry.World {
	if cached := m.cachedWorld(nodeID); cached != nil {
		return cached
	}

	world, err := m.regClient.World(ctx, registry.NodeID(nodeID.Uint64()))
	if err != nil {
		log.Warn().
			Err(err).
			Uint64("peer.id", nodeID.Uint64()).
			Msg("registry world fetch failed for peer; dropping it from the netmap")

		return nil
	}

	if err := supportedWorld(world); err != nil {
		log.Warn().Err(err).Uint64("peer.id", nodeID.Uint64()).Msg("dropping peer")

		return nil
	}

	m.cacheWorld(nodeID, world)

	return world
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

func (m *mapper) cachedWorld(nodeID types.NodeID) *registry.World {
	m.worldCacheMu.Lock()
	defer m.worldCacheMu.Unlock()

	return m.worldCache[nodeID]
}

func (m *mapper) cacheWorld(nodeID types.NodeID, world *registry.World) {
	m.worldCacheMu.Lock()
	defer m.worldCacheMu.Unlock()

	m.worldCache[nodeID] = world
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

// holdsVisibleAddress reports whether any of a peer's own addresses is one
// this node has a path to.
func holdsVisibleAddress(world *registry.World, visible map[netip.Addr]bool) bool {
	return slices.ContainsFunc(world.Addresses, func(own string) bool {
		addr, err := netip.ParseAddr(own)

		return err == nil && visible[addr]
	})
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

// applyWorldName names a node inside its first organization — the World lists
// them in the renderer's organization order, so the first is the node's
// primary one. A node named in no organization keeps whatever the head gave
// it, because a name assembled from nothing is not a name.
func applyWorldName(node *tailcfg.Node, names []registry.Name, baseDomain string) {
	if len(names) == 0 || baseDomain == "" {
		return
	}

	node.Name = names[0].Label + "." + names[0].Org + "." + baseDomain + "."
}

// rewriteWorldPeers keeps the peers the World's filters make visible, reading
// each one's addresses and names from that peer's own World, and records the
// rest in removed. A nil slice stays nil — deltas rely on the nil/empty
// distinction.
func (m *mapper) rewriteWorldPeers(
	ctx context.Context,
	peers []*tailcfg.Node,
	visible map[netip.Addr]bool,
	removed []tailcfg.NodeID,
) ([]*tailcfg.Node, []tailcfg.NodeID) {
	if peers == nil {
		return nil, removed
	}

	kept := peers[:0]

	for _, peer := range peers {
		world := m.peerWorld(ctx, types.NodeID(peer.ID)) //nolint:gosec // IDs are bounded
		if world == nil || !holdsVisibleAddress(world, visible) {
			removed = append(removed, peer.ID)

			continue
		}

		applyWorldAddresses(peer, world.Addresses)
		applyWorldName(peer, world.Names, m.baseDomain())
		kept = append(kept, peer)
	}

	return kept, removed
}

// keepVisiblePatches drops a delta for a peer the World does not make
// visible. A patch names a peer the client already holds, so it is judged by
// the same rule the peer itself was, and never by whether it happens to
// appear in this response.
func (m *mapper) keepVisiblePatches(
	ctx context.Context,
	patches []*tailcfg.PeerChange,
	visible map[netip.Addr]bool,
) []*tailcfg.PeerChange {
	if len(patches) == 0 {
		return patches
	}

	kept := patches[:0]

	for _, patch := range patches {
		world := m.peerWorld(ctx, types.NodeID(patch.NodeID)) //nolint:gosec // IDs are bounded
		if world != nil && holdsVisibleAddress(world, visible) {
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
