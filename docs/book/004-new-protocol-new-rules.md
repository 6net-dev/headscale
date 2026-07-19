# 004 — New protocol, new rules

*2026-07-19*

Two more pieces settled today: what things are called, and what happens
to IPv4. The second one got us our founding rule.

## Names are the grant's shadow

Naming falls straight out of the grant model. Names are org-scoped —
`<device>.<org>.<base>` — so one laptop is `mac.personal.…` and
`mac.acme.…` at once: two names, two addresses, one device, and
uniqueness only matters within an org. A share brings names with it
(`jellyfin.frites.…` appears when the grant activates, vanishes when it
doesn't). DNS composes exactly like the netmap because it is built from
the same grants by the same builder. Mechanically it's stock MagicDNS
over IPv6 — the resolver address we already proved works v6-only, AAAA
records, per-org search domains — and because there is one daemon, there
is one resolver, and the split-DNS wars of multi-daemon designs never
happen.

One consequence took a moment to appreciate: an org's slug is not
branding, it's a **DNS label in every member's resolver**. So slugs are
registry-unique, strict RFC 1123 (`[a-z0-9]` and hyphens, 2–63 chars,
ASCII only — no homograph games), screened against a reserved list. The
slug is a mutable pointer to an immutable org ID; nothing but DNS and
presentation ever references it. Renames stay cheap forever.

## The line in the sand

Routing inside the overlay is collision-free by construction — disjoint
org prefixes, /128 peers, everything aggregating under its org's /56.
The one door collisions could sneak back through is subnet routers:
sooner or later two orgs both expose a `192.168.1.0/24`, and suddenly
we'd be back in Tailscale's world, managing overlap instead of
abolishing it.

So the rule, stated once and enforced everywhere: **the overlay carries
no IPv4 routes. Ever.** Legacy v4 LANs are reachable only through
4via6 translation — each site's range maps into its own v6 prefix, so
two identical home LANs become two *different* subnets and the entire
collision class goes extinct. IPv4 is a translated legacy at the edge,
never a citizen of the core.

IPv4 just limits us. That's our line in the sand — new protocol, new
rules, new and endless possibilities.

## Discipline check

We also adopted a standing review rule: everything Tailscale's protocol
machinery thinks it needs — DERP, key expiry, 4via6, device posture,
C2N, the lot — gets an explicit *keep / simplify / defer / drop* verdict
with reasons, because their feature list is a map of production problems
they actually hit. That inventory is being compiled now; it becomes the
next entry.
