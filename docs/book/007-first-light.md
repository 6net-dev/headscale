# 007 — First light

*2026-07-19*

Today a stock Tailscale client applied an address that no part of
headscale chose. The choosing happened in Elixir, in a Postgres row,
in a grant.

## The transplant

The walking skeleton splits authority exactly where the design said it
should: the **registry** (Phoenix/Postgres) owns topology and
addressing — orgs, grants, who exists, who sees whom, who is called
what number — while the **wire head** (headscale, carved) keeps
transport: Noise sessions, endpoints, disco keys, DERP. Two hooks join
them:

- **Registration reports upward.** When a node completes registration,
  the head fires a fire-and-forget upsert to the registry, which
  enrolls the device and (v0 convenience) admits it to the default org
  with an active grant.
- **Netmaps rewrite downward.** Every outbound MapResponse — full,
  delta, or patch — passes through one funnel in the head. There, the
  registry's `World(device)` is applied: self addresses replaced,
  peers not present in the world dropped, peer addresses replaced.
  What the client sees is the union of its grants, rendered in
  Tailscale's own dialect.

## The proof

You can't prove provenance with matching numbers, so we made them
disagree: headscale's own allocator was configured to hand out
addresses in `fd7a:115c:a1e0:ee00::/56`, while the registry's
`personal` org allocates from `fd7a:115c:a1e0::/56`. One binary, one
Phoenix server, one throwaway stock tailscaled later:

- headscale's database: node is `…:ee00::1`
- the client's interface: **`fd7a:115c:a1e0::1`**

The address on the wire is the registry's. Netmap truth now originates
in the composition engine — the same code whose tests promise that
core7 can never learn mac's acme address — and headscale has become
what the design asked it to be: a protocol head.

## Honest ledger

V0 shortcuts, written down so they can't pretend to be decisions:
fail-open when the registry is unreachable (must become fail-closed
with a cached world before the org seam carries real trust); DNS
search domains and filter rewriting not yet applied from the world;
enrollment auto-admits to a default org instead of a real admission
flow; the OIDC registration path doesn't fire the upsert hook yet.

Next: activation toggling end-to-end (flip a grant in Postgres, watch
the netmap shrink), a second org, and the TUI.
