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
since revoked — for as long as the cut lasts, which is why `registry.cache_path`
must be on a volume that survives a restart and why the revocation SLA is "head
reachable" and is measured. Two settings have to agree with the registry or
nothing resolves: `dns.base_domain` must equal the registry's
`OVERLAY_BASE_DOMAIN`, and the bearer must be the same string on both sides. A
head with `registry.url` set and no bearer refuses to boot rather than start and
serve nothing.
