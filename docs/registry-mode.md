# Registry mode

Setting `registry.url` makes an external registry — Gentility — authoritative
for topology and addressing, and this head becomes the thing that serves what
the registry renders. Four channels carry that, all authenticated in both
directions with the one shared secret (`registry.bearer`, `registry.bearer_path`
or `REGISTRY_BEARER`): the head **fetches a World per node** by head node id and
rewrites every outbound netmap from it — addresses, names, DNS and packet
filters — and **follows a change stream** with a cursor, refreshing every cached
World on each reconnect so no gap can leave a stale netmap; the registry
**calls the head's own API** for what a World cannot express, minting a
pre-auth key and deleting a node; and the head **calls back synchronously**,
from inside the write transaction, on a registration (carrying the pre-auth
key id, both keys and the node id it has just allocated) and on a node key
rotation. Those last two are the operating rule that matters when reading this
code: **the head commits nothing the registry has not recorded** — a refusal or
an outage rolls the node write back and the node never existed, and the client
retries, which is safe because the registry's register is idempotent by content
and its rotate is idempotent on the new key.

Cut off from the registry, the head is **fail-static, not fail-closed**: it goes
on serving the netmaps it last rendered — including access the registry has
since revoked — for as long as the cut lasts, which is why the revocation SLA
is "head reachable" and is measured.

## Three conditions a deployment has to meet

Each of these was once a comment asking an operator to be careful. Each is now
enforced or reported by the head itself, because the failures they cause all
look like something else.

**The base domain must match, and there must be one.** `dns.base_domain` must
equal the registry's `OVERLAY_BASE_DOMAIN`. The rendered World now carries its
own `base_domain` and that is what the head composes search domains and
fallback names from, so a disagreement no longer produces wrong names — but it
does mean everything else the head composes from its own config is wrong, and
the first World that disagrees is reported at **Error**. A `registry.url` with
an empty `dns.base_domain` **refuses to boot**: it is what the head falls back
to for a World that carries none, and empty it would serve no search domains
and no names at all.

**Enrolment is by pre-auth key only.** An enrolment is identified on channel 3
by the pre-auth key id the registry minted through the head's API, and there is
no other name for one. So `headscale nodes register` and the OIDC callback are
**refused** in registry mode rather than creating a node the registry has never
heard of — which would be unreported, never rendered, and refused as an unknown
node by every map build for the whole of its life. A node key rotation
presented on those same interactive paths is refused for the same reason: the
re-registration path, which does hold a pre-auth key, is where channel 4
records a rotation.

**The cache volume must be writable by the head.** `registry.cache_path` holds
the World cache and the change-stream cursor, and it is what makes fail-static
survive a restart. The container runs as uid 65532 with no shell, and a **bind
mount arrives with the host's ownership**, so the host directory needs
`chown 65532:65532` before it is mounted. Nothing fails visibly if it does not:
the head serves, follows the stream and refreshes exactly as before, and only
comes back from every restart with no cursor and no worlds, full-refreshing
every node before it can serve any of them. The first failed write says so at
**Error**.

## Two things that are not what they look like

**Renaming a node does nothing a peer can see.** `headscale nodes rename`
changes `GivenName`, which is what upstream headscale builds a node's netmap
name from. In registry mode the rewrite replaces that name with the World's —
the registry composes `<label>.<org>.<base_domain>.` and the head is a consumer
of it — so a rename is visible in the head's own database and CLI and nowhere
else. A node is renamed by renaming its grant in the registry.

**`change.VisibilityChange` is not the way to say a peer became invisible.**
The batcher reads `Change.PeersRemoved` as a list of nodes being deleted: it
drops each one and closes its control session. That is right for a deletion and
disastrous for a node that has merely lost sight of a peer, which would be
disconnected for someone else's policy change. Lost visibility is expressed by
computing the removed set per recipient at build time — which is what the
registry rewrite does — and putting it in `tailcfg.MapResponse.PeersRemoved`, a
different struct that nothing treats as a deletion.

## Settings

| Key | Default | What it is |
| --- | --- | --- |
| `registry.url` | unset | The registry. Unset is upstream headscale. |
| `registry.bearer` / `registry.bearer_path` / `REGISTRY_BEARER` | unset | The shared secret, in both directions. Mutually exclusive; the environment wins. A `url` with no bearer refuses to boot. |
| `registry.cache_path` | `/var/lib/headscale/registry-cache.json` | The World cache and the change cursor. Must survive a restart, and must be writable. |
| `registry.refresh_timeout` | `30s` | Caps the sweep that re-reads every cached World on each (re)connect. No change is served until the sweep is done, so this is the longest a reconnect may leave the head deaf; a World it did not reach is read again on demand. |
