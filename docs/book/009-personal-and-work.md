# 009 — Personal and work

*2026-07-19*

The message that started this project said: *I want both a personal
and a work overlay running together on one machine, and I want logins
to enable or disable access to subnets — not replace the whole thing.*
Today the rig runs that sentence, literally.

## The shape

Two orgs in the registry — `personal` (…`::/56`) and `work`
(…`:100::/56`) — and three stock, unmodified Tailscale clients:

- **node1** is the "my laptop" of the story: admitted to both orgs,
  holding **two addresses at once** — `…::1` in personal, `…:100::2`
  in work. The multi-address netmap the spike proved in isolation now
  arrives through the full production path: Postgres grants, world
  composition, netmap rewrite, stock client.
- **node2** is personal-only. **node3** is work-only.

And the seam — the property our very first Elixir test pinned as
"core7 must never learn mac's acme address" — held on real WireGuard
clients, in every direction at once: node2 sees node1 at *only* its
personal address, node3 at *only* its work address, and node2 and
node3 do not exist for each other. The head's own database still
believes all three nodes are one happy mutually-visible family;
the registry's world simply outranks it.

## Six o'clock, for real this time

One request — `PUT …/grants/work {"active": false}` — and four
seconds later: node1's work address gone, node3 gone from node1's
world, node1 gone from node3's. And the part that matters most is the
part where nothing happened: node2's netmap untouched, node1's
personal address untouched, and a personal-org ping still crossing in
1ms *while* the work overlay dissolved around it. In the morning, one
more request, and work is back — same address as yesterday, because
the grant kept its slot.

Tailscale switches worlds; this composes them. That difference is the
whole product.

## Bookkeeping

The demo rode on a new admission API (`POST
/api/devices/:node_key/grants`) — orgs can now be joined by request
rather than only by enrollment default. Still an unauthenticated
admin-tool stand-in for the real invite flow, and the honest ledger
from 007 still stands otherwise: fail-open, no DNS/filter rewrite
yet, curl-as-TUI.

But the core claim of the design — one daemon, one global network,
organizational subnets, additive logins, org-scoped visibility — is
no longer an argument. It's a running system with 31 green tests and
three humming daemons. Next: the TUI, so six o'clock is a keypress.
