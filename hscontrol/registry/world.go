package registry

import (
	"encoding/json"
	"strconv"
)

// NodeID renders a head node id the way this channel names one: the decimal
// form of the id the head allocated for the node.
//
// It is the key of every call here — the World is fetched by it, and the
// registration callback reports it — because it is the one name for a node
// that does not move. A node key rotates; a document keyed by one would move
// when nothing about the node did.
//
// The id therefore has to EXIST before the head talks about the node at all,
// which is why the registration callback belongs inside the write that
// allocates it and after the insert, not before and not after the commit.
func NodeID(id uint64) string {
	return strconv.FormatUint(id, 10)
}

// World is the document Gentility renders for one head node and the head
// rewrites its netmaps from. v1; see
// docs/plans/2026-08-28-overlay-phase1-master.md §2.4 in the Gentility repo,
// rendered by Gentility.Overlay.Render.world_for/6.
//
// A consumer refuses a RendererVersion it does not know: the version is the
// shape of this document, not of a row, so a head that has not been taught the
// next one has nothing it can safely rewrite from.
//
// There is no Peers array, by design. The visible peer set is derived from
// Filters — every address appearing as a filter's Src or Dst, minus this
// node's own Addresses — and each peer's addresses and names come from that
// peer's own World, which the head fetches by head node id and caches.
type World struct {
	RendererVersion int      `json:"renderer_version"`
	Revision        int64    `json:"revision"`
	HeadNodeID      string   `json:"head_node_id"`
	Addresses       []string `json:"addresses"`
	Names           []Name   `json:"names"`
	DNS             []Record `json:"dns"`
	Filters         []Filter `json:"filters"`

	// DERPMap is configuration, passed through verbatim rather than
	// re-derived here.
	DERPMap json.RawMessage `json:"derp_map"`
}

// RendererVersion1 is the only World shape this head understands.
const RendererVersion1 = 1

// Name is a node's label inside one organization. A node holding grants in
// several organizations is named separately in each, and Names is in the same
// organization order as Addresses: the name at index i is the name the node
// answers to at the address at index i. That pairing is what lets the head
// show a peer only under the organization it shares with the viewer.
//
// Fqdn is the fully-qualified name Gentility composed. Prefer it where it is
// present: the renderer is what knows what a name is made of. It is omitted by
// a Gentility that does not send one yet, and the head then composes
// "<label>.<org>.<base_domain>." for itself.
type Name struct {
	Org   string `json:"org"`
	Label string `json:"label"`
	Fqdn  string `json:"fqdn,omitempty"`
}

// Record is a DNS record for the platform hostname. Several records may carry
// the SAME name with different addresses — one per organization the node holds
// a live grant in — and that is the point rather than a duplication: the
// client picks its source address by the destination's /64, so each answer is
// dialled out of the organization whose gateway it names.
//
// Type is empty by design (design §13): the pinned client ignores a record
// whose type is non-empty, which is why the v0 rewrite's "AAAA" was wrong.
type Record struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
	Type string `json:"type,omitempty"`
}

// Filter is one rendered path: a /128 source, a /128 destination, and the
// ports it may reach. Every edge comes from a reach grant and from nothing
// else, so an empty Ports list is a path to no port rather than a path to any.
type Filter struct {
	Src   string `json:"src"`
	Dst   string `json:"dst"`
	Ports []int  `json:"ports"`
}

// Change is one event on the change stream: which change, whose World, and
// which revision of it. The World itself is fetched back by head node id — a
// payload here would be a second, unordered copy of a stream whose only value
// is that it is ordered.
type Change struct {
	Seq           int64  `json:"seq"`
	HeadNodeID    string `json:"head_node_id"`
	WorldRevision int64  `json:"world_revision"`
}

// Registration is the callback the head makes from inside its own write
// transaction, before the node row is committed (design §4 channel 3).
// HeadNodeID is the id the head has just allocated, which is why the call
// belongs after the insert and inside the transaction.
type Registration struct {
	PreauthKeyID string `json:"preauth_key_id"`
	NodeKey      string `json:"node_key"`
	MachineKey   string `json:"machine_key"`
	HeadNodeID   string `json:"head_node_id"`
}

// RegistrationResult is what Gentility records for a registration.
//
// World is nil whenever Gentility had none to give — a first registration
// whose render is queued in the transaction that has just committed, or a
// rebind whose stored World still names the head node id the head has just
// abandoned. Neither is a failure: the change stream delivers the real
// document when the render lands.
type RegistrationResult struct {
	NodeID string `json:"node_id"`
	World  *World `json:"world"`
}

// Rotation is the callback for a node key rotation, made inside the same
// transaction as the update it records.
type Rotation struct {
	HeadNodeID string `json:"head_node_id"`
	OldKey     string `json:"old_key"`
	NewKey     string `json:"new_key"`
}
