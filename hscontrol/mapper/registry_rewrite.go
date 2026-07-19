package mapper

import (
	"context"
	"net/netip"
	"time"

	"github.com/juanfont/headscale/hscontrol/registryclient"
	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/rs/zerolog/log"
	"tailscale.com/tailcfg"
)

const registryWorldTimeout = 3 * time.Second

// rewriteFromRegistry makes the registry authoritative for topology and
// addressing: self and peer addresses are replaced by the World(device)
// contract, and peers absent from the world are dropped. The wire head
// stays authoritative for transport (endpoints, disco keys, online).
//
// v0 walking skeleton: fails OPEN — on registry errors the response is
// served unrewritten, loudly. This must become fail-closed (serve a
// cached world, then drop peers) before any deployment where the org
// seam carries real trust.
func (m *mapper) rewriteFromRegistry(nodeID types.NodeID, resp *tailcfg.MapResponse) {
	if m.regClient == nil || resp == nil {
		return
	}

	nv, ok := m.state.GetNodeByID(nodeID)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), registryWorldTimeout)
	defer cancel()

	world, err := m.regClient.World(ctx, nv.NodeKey().String())
	if err != nil {
		log.Warn().
			Err(err).
			Uint64("node.id", nodeID.Uint64()).
			Msg("registry world fetch failed; serving unrewritten map (v0 fail-open)")

		return
	}

	byKey := make(map[string]registryclient.Peer, len(world.Peers))
	for _, peer := range world.Peers {
		byKey[peer.NodeKey] = peer
	}

	if resp.Node != nil {
		applyWorldAddresses(resp.Node, world.Self.Addresses)
	}

	resp.Peers = rewriteWorldPeers(resp.Peers, byKey)
	resp.PeersChanged = rewriteWorldPeers(resp.PeersChanged, byKey)

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
}

// rewriteWorldPeers drops peers the world does not contain and rewrites
// the addresses of those it does. A nil slice stays nil — deltas rely
// on the nil/empty distinction.
func rewriteWorldPeers(
	peers []*tailcfg.Node,
	byKey map[string]registryclient.Peer,
) []*tailcfg.Node {
	if peers == nil {
		return nil
	}

	kept := peers[:0]

	for _, peer := range peers {
		worldPeer, ok := byKey[peer.Key.String()]
		if !ok {
			continue
		}

		applyWorldAddresses(peer, worldPeer.Addresses)
		kept = append(kept, peer)
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
