# 003 — Everything is a grant

*2026-07-19*

Three design questions, one answer. After settling where the netmap
union lives ([002](002-where-the-union-lives.md)), we worked through
addressing, login semantics, and cross-org sharing — and watched them
collapse into a single primitive.

## Addresses belong to grants, not devices

The tension: "one global network" suggests a device has one address
everywhere; "organizational subnets" only mean something if addresses
genuinely live inside them. We chose the subnets. **A device holds one
address per org it currently participates in** — logging into acme adds
`acme-prefix::you` to your interface; revocation removes it. Stock
clients already handle multi-address netmaps (dual-stack is exactly that
machinery), so this costs nothing at the client.

What it buys: an org's entire world is contained in its sub-prefix, so
filters, routes, and revocation collapse to prefix operations — and two
orgs can't correlate that they serve the same device by comparing
addresses. What it costs: "what's my address?" has no single answer,
and naming must be org-scoped. We think that trade is obviously right;
the address that appears when you activate a grant *is* the feature.

## Login is two acts, not one

Tailscale fuses "the org lets this device in" with "this network is on
right now," which is why its logins are heavyweight enough that they
replace your world. We split them:

- **Admission** — the org's ceremony: OIDC, admin approval, or a preauth
  key; expensive, rare, expiring on the org's schedule.
- **Activation** — your instant, IdP-free toggle over a still-valid
  admission. The 6pm "work off" flips a bit and shrinks the netmap.

## Sharing is a small membership

The headline feature — cross-org sharing on a flexible basis — needed no
new mechanism at all. A friend sharing a media server is just their org
*admitting* your device with policy scoped to one host: you get a guest
address in their prefix, a route to what's shared, and a toggle like
any other grant. Full membership and a single shared service are the
same object at different scope.

## The resulting atom

```
grant = (device, org, identity, scope, admission-expiry, active?)
```

The registry's netmap builder unions a device's *active* grants —
addresses, peers, routes, DNS — and nothing else. Every feature in the
brief is now a verb on this one noun: login activates a grant, logout
deactivates it, sharing creates a narrow one, offboarding revokes it,
and "organizational subnets with flexible cross-org sharing" is just
what a pile of grants looks like from above.

Next: naming, prefix layout, and the v1 cut.
