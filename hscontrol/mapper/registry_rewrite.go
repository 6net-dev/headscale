package mapper

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/6net-dev/6net/registryclient"
	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/rs/zerolog/log"
	"tailscale.com/tailcfg"
)

const registryWorldTimeout = 3 * time.Second

// rewriteFromRegistry makes the registry authoritative for topology,
// addressing, naming, DNS, and filters: the World(device) contract is
// applied to every outbound MapResponse. The wire head stays
// authoritative for transport (endpoints, disco keys, online).
//
// Fail-closed: on registry errors the last known world for the node is
// used; with no cached world the response is withheld entirely — a
// stale-but-sealed netmap beats a fresh one that violates the org
// seam.
func (m *mapper) rewriteFromRegistry(nodeID types.NodeID, resp *tailcfg.MapResponse) error {
	if m.regClient == nil || resp == nil {
		return nil
	}

	nv, ok := m.state.GetNodeByID(nodeID)
	if !ok {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), registryWorldTimeout)
	defer cancel()

	world, err := m.regClient.World(ctx, nv.NodeKey().String())
	if err != nil {
		world = m.cachedWorld(nodeID)
		if world == nil {
			return fmt.Errorf("registry unreachable and no cached world for node %d: %w", nodeID, err)
		}

		log.Warn().
			Err(err).
			Uint64("node.id", nodeID.Uint64()).
			Msg("registry world fetch failed; serving cached world")
	} else {
		m.cacheWorld(nodeID, world)
	}

	byKey := make(map[string]registryclient.Peer, len(world.Peers))
	for _, peer := range world.Peers {
		byKey[peer.NodeKey] = peer
	}

	if resp.Node != nil {
		applyWorldAddresses(resp.Node, world.Self.Addresses)
		applyWorldName(resp.Node, world.Self.Entries)
	}

	// Peers dropped by the rewrite must be announced in PeersRemoved:
	// an empty Peers list is omitted on the wire and means "no
	// change", so silence would leave stale peers on the client.
	var removed []tailcfg.NodeID

	resp.Peers, removed = rewriteWorldPeers(resp.Peers, byKey, removed)
	resp.PeersChanged, removed = rewriteWorldPeers(resp.PeersChanged, byKey, removed)
	resp.PeersRemoved = append(resp.PeersRemoved, removed...)

	if len(resp.PeersChangedPatch) > 0 {
		kept := resp.PeersChangedPatch[:0]

		for _, patch := range resp.PeersChangedPatch {
			pnv, ok := m.state.GetNodeByID(types.NodeID(patch.NodeID)) //nolint:gosec // IDs are bounded
			if !ok {
				continue
			}

			if _, inWorld := byKey[pnv.NodeKey().String()]; inWorld {
				kept = append(kept, patch)
			}
		}

		resp.PeersChangedPatch = kept
	}

	applyWorldDNS(resp, world)
	applyWorldFilters(resp, world)

	return nil
}

func (m *mapper) cachedWorld(nodeID types.NodeID) *registryclient.World {
	m.worldCacheMu.Lock()
	defer m.worldCacheMu.Unlock()

	return m.worldCache[nodeID]
}

func (m *mapper) cacheWorld(nodeID types.NodeID, world *registryclient.World) {
	m.worldCacheMu.Lock()
	defer m.worldCacheMu.Unlock()

	m.worldCache[nodeID] = world
}

// applyWorldDNS installs the world's org search domains and adds AAAA
// records for every org-scoped name beyond a node's primary one, so
// `host.org.base` resolves per org even when a device is named in
// several orgs at once.
func applyWorldDNS(resp *tailcfg.MapResponse, world *registryclient.World) {
	if resp.DNSConfig == nil {
		return
	}

	resp.DNSConfig.Domains = world.DNS.SearchDomains

	var extra []tailcfg.DNSRecord

	extra = appendSecondaryRecords(extra, world.Self.Entries)
	for _, peer := range world.Peers {
		extra = appendSecondaryRecords(extra, peer.Entries)
	}

	resp.DNSConfig.ExtraRecords = extra
}

func appendSecondaryRecords(records []tailcfg.DNSRecord, entries []registryclient.WorldEntry) []tailcfg.DNSRecord {
	for i, entry := range entries {
		if i == 0 {
			continue // primary name is carried by Node.Name
		}

		records = append(records, tailcfg.DNSRecord{
			Name:  entry.FQDN,
			Type:  "AAAA",
			Value: entry.Address,
		})
	}

	return records
}

// applyWorldFilters replaces the head's packet filter with the
// world's org-scoped rules: traffic flows inside an org's prefix and
// nowhere else.
func applyWorldFilters(resp *tailcfg.MapResponse, world *registryclient.World) {
	if resp.PacketFilters == nil {
		return
	}

	rules := make([]tailcfg.FilterRule, 0, len(world.Filters))
	for _, f := range world.Filters {
		rules = append(rules, tailcfg.FilterRule{
			SrcIPs: []string{f.Src},
			DstPorts: []tailcfg.NetPortRange{
				{IP: f.Dst, Ports: tailcfg.PortRangeAny},
			},
		})
	}

	resp.PacketFilters = map[string][]tailcfg.FilterRule{"base": rules}
}

func applyWorldName(node *tailcfg.Node, entries []registryclient.WorldEntry) {
	if len(entries) == 0 {
		return
	}

	node.Name = strings.TrimSuffix(entries[0].FQDN, ".") + "."
}

// rewriteWorldPeers drops peers the world does not contain (recording
// them in removed) and rewrites the addresses and names of those it
// does. A nil slice stays nil — deltas rely on the nil/empty
// distinction.
func rewriteWorldPeers(
	peers []*tailcfg.Node,
	byKey map[string]registryclient.Peer,
	removed []tailcfg.NodeID,
) ([]*tailcfg.Node, []tailcfg.NodeID) {
	if peers == nil {
		return nil, removed
	}

	kept := peers[:0]

	for _, peer := range peers {
		worldPeer, ok := byKey[peer.Key.String()]
		if !ok {
			removed = append(removed, peer.ID)
			continue
		}

		applyWorldAddresses(peer, worldPeer.Addresses)
		applyWorldName(peer, worldPeer.Entries)
		kept = append(kept, peer)
	}

	return kept, removed
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
