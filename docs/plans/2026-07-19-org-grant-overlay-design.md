# Org-Grant Overlay — Design

*2026-07-19 — validated in design dialogue; see `docs/book/001–005` for the
narrative trail.*

## 1. What this is

A Tailscale-ergonomics overlay network that is IPv6-only, multi-org by
construction, and additive: authenticating to an organization *adds* its
subnet to a device's world; logging out removes it; nothing else moves.
Built on the validated foundation that stock Tailscale clients run
v6-only end-to-end against headscale (`TestPingAllByIPv6Only`, plus live
two-machine validation on 2026-07-19).

**Non-goals for v1:** corporate-grade isolation from the registry
operator (path C, below), federation, mobile clients, OIDC admission,
Funnel/Serve, device posture, network flow logs (the last is a permanent
non-goal, as policy).

## 2. Core model

Components: a **registry** (control plane), **stock Tailscale clients**
(unmodified), a **TUI** (device-holder UX), and DERP relay(s).

- One global IPv6 overlay inside `fd7a:115c:a1e0::/48` (the range stock
  clients are defined on). The registry allocates each **org** a `/56`
  at creation.
- An **org** owns: an immutable ID, a unique DNS-safe slug, a `/56`
  prefix, a policy, admission rules. Nothing exists outside an org.
- A **device** registers once (one node key, one control connection).
- The **grant** is the atom:

  ```
  grant = (device, org, identity, scope, admission-expiry, active?)
  ```

- The **netmap builder** (the only code allowed to see across orgs)
  composes each device's world as the union of its *active* grants:
  per-org self address, peers, routes, filters, DNS names.

**Two-layer login.** *Admission* (org ceremony: invite link, admin
approval; later OIDC) creates a grant with an org-policy expiry.
*Activation* is the device-holder's instant, IdP-free toggle on a valid
admission. Revocation at either layer shrinks the netmap within a poll
cycle.

**Sharing = a narrow grant.** An org sharing a host/subnet admits the
recipient device with scope limited to the shared resources: guest
address in the sharer's prefix, routes and names for exactly the shared
things, toggleable and revocable like any grant. Full membership and a
single-service share are one mechanism at different scope.

## 3. Addressing

- Per-org addresses: a device holds one address inside each org it is
  admitted to; activation adds the address (stock clients already apply
  multi-address netmaps — dual-stack uses the same machinery).
- Org sub-prefix `/56`; device addresses allocated within it at
  admission time; freed on revocation.
- Consequences embraced: no single canonical "my address"; org worlds
  aggregate and filter by prefix; orgs cannot correlate devices by
  address.

## 4. Naming and DNS

- Names mirror addresses: `<device>.<org-slug>.<base-domain>`; device
  names unique per org (RFC 1123 labels, auto-deduped from hostname).
- **Org slugs**: registry-wide unique, RFC 1123 label syntax
  (`[a-z0-9]([a-z0-9-]*[a-z0-9])?`, 2–63 chars, ASCII only), screened
  against a reserved list (`www`, `api`, `admin`, `derp`, `dns`,
  `registry`, `internal`, …). Slug is a mutable unique pointer to the
  immutable org ID; only DNS and presentation reference slugs.
- Stock MagicDNS: resolver `fd7a:115c:a1e0::53`, AAAA-only, per-org
  search domains pushed in the netmap, reverse DNS for the /48. One
  daemon ⇒ one resolver ⇒ no split-DNS conflicts.
- Per-org custom domains: later; seam is a per-org domain column.

## 5. Routing and the IPv4 rule

- Overlay routing is collision-free by construction: disjoint org
  prefixes, /128 peers, org worlds aggregating under their /56.
- **The overlay carries no IPv4 routes, ever.** Legacy v4 LANs are
  reachable only via 4via6 translation (each site's v4 range maps into
  a distinct v6 prefix — the collision class is abolished, not
  managed). 4via6 requires no bespoke registry code: a via-route is an
  ordinary v6 prefix through normal org-scoped approval.
- Native v6 subnet routes: allowed, org-scoped, HA election inherited.
- Exit nodes: mechanism inherited; offering org-scoped; at most one
  active per device (there is one default route), default none.

## 6. Trust model and seams

Whoever computes the netmap union sees the union. V1 places composition
at the registry (operator-composed) — the same trust shape as Tailscale
SaaS, acceptable for personal/community networks, unacceptable for
corporate orgs when the operator is a private individual.

**Path C** (future): a forked client holds N per-org control sessions
and composes locally; each org server sees only its slice. V1's job is
to keep that road open. Seam rules, enforced from day one:

1. Org ID on every record; no recordless-org data.
2. Per-org prefixes, policies, keys, domains.
3. The union is computed in exactly one place (the composition engine)
   and appears in exactly one artifact (`World(device)`).
4. No cross-org joins outside the composition engine.

TKA/tailnet lock is *not* adopted: it is Tailscale's cryptographic
patch for the same threat, and our answer is architectural (move the
composition point). V1 carries the same unmitigated
registry-compromise risk as Tailscale-without-TKA; stated honestly.

## 7. Tailscale machinery verdicts

From the full inventory of the `tailcfg` v1.101 control surface vs
headscale's implementation:

**Keep untouched (inherited working):** Noise/ts2021 transport,
MapResponse streaming/batching, DERP map + embedded DERP + STUN,
endpoint/DERP-home plumbing, presence/keepalive, capver gating, DNS
config distribution, ACL packet filters, subnet routers + HA election,
route auto-approval, ephemeral nodes, SSH policy, generic capability
passthrough (CapGrant/PeerCapMap — Taildrop, WoL et al. work because
the server forwards opaque strings; keep it not-understood).

**Keep with new meaning:** registration/preauth → per-org admission;
key expiry split (node-key ↔ registry, rare) vs (admission expiry ↔
per-grant, daily); 4via6 promoted to doctrine; exit nodes org-scoped;
UserProfiles/Domain presented per-org.

**Drop deliberately:** `Debug.Exit`/`SleepSeconds` (registry must not
command devices); network flow logs (permanent non-goal, policy);
ControlDialPlan (SaaS-reachability machinery); TKA (see §6).

**Defer with revival triggers:** Funnel/Serve/VIP (cert story), ACME
dns-01 certs (overlay HTTPS), C2N (posture/remote-update era), device
posture (path C era), Health/DisplayMessages (post-v1 grant-expiry UX;
TUI covers v1), PopBrowserURL (TUI covers re-admission), SSH session
recording, audit logs, app connectors, `OldNodeKey` rotation
(headscale gap; fix opportunistically).

## 8. Implementation architecture

**Elixir owns truth; Go owns the wire.**

- **Registry — Elixir / Phoenix / Postgres.** Source of truth and all
  business logic: Ecto schemas for orgs, devices, identities, grants,
  addresses, policies, invites; admission + activation APIs (JSON, for
  TUI and the eventual SaaS UI); the composition engine resolving
  grants → `World(device)`; Phoenix PubSub publishing world updates.
  High-frequency ephemeral state (endpoints, hostinfo, online) flows
  through the registry's process state/PubSub without a Postgres write
  per update; durable state is Ecto-managed. Postgres has one writer:
  Elixir.
- **Wire head — Go, carved from headscale.** Speaks ts2021/Noise to
  clients; renders `World(device)` into `tailcfg` MapResponses with the
  existing mapper/batcher machinery; runs embedded DERP/STUN; enforces
  capver. Translates client-born events (registration, endpoint and
  Hostinfo updates, rotation, logout) into registry API calls. Holds no
  database and no policy logic. Reused packages: `noise`, `mapper`
  (adapted), `derp`, `capver`, `tailcfg` rendering from `types`;
  dropped: `db`, `policy` evaluation, `state` (replaced by the
  registry).
- **The `World(device)` contract** — versioned JSON: self addresses,
  peers (keys, addresses, endpoints, names, org), filter rules
  (abstract src/dst/port; the head renders `tailcfg.FilterRule`), DNS
  config, routes. This contract is the seam made concrete — and it is
  deliberately shaped like the per-org slice a path-C composing client
  would consume from single-org registries later.
- **Testing.** The existing Docker integration harness runs against
  the composite (registry + wire head), keeping stock-client
  compatibility continuously proven. `hscontrol/servertest` covers
  head-only tests.
- **Wire head, option B (declared v2 path):** upstream `tailscale-rs`
  (`ts_control` crate) via Rustler could move the protocol machinery
  *inside* the BEAM — one deployable instead of two. Deferred until
  the `World(device)` contract is stable and tailscale-rs matures
  (today the harness builds its example node with a patched feature
  flag); long-lived Noise streams belong on Rustler-supervised native
  threads, not naive NIFs. `tsric` stays in the test matrix so
  tailscale-rs compatibility is continuously measured. The contract
  makes Go-service → Rust-in-BEAM a swap, not a rewrite.

## 9. V1 scope and order

1. **Spikes (gating, ~an afternoon in the harness):**
   a. Stock client applies *multiple IPv6 self-addresses* from one
      netmap (gates the addressing design). **PROVEN 2026-07-19** —
      `TestMultiIPv6AddressSpike` on branch `spike/multi-v6-address`:
      every node took two addresses in different /56s and answered
      pings on both.
   b. Per-org search domains compose past 2–3 orgs.
   c. Toggle churn: full netmap replace on activation feels instant.
2. **M1 — the heart:** org + grant schema in Postgres; composition
   engine; wire head speaking for one org, then two; invite-link
   admission; activation API. Done = two orgs, Mac + core7, toggling
   via `curl`.
3. **M2 — TUI:** grants list (org, address, expiry, active), toggle,
   invite redemption, per-org peer status. No protocol logic.
4. **M3 — shares + polish:** scoped share invites, org-scoped DNS
   hardening, 4via6 doctrine documented, exit-node pick-one UX.

Out: OIDC (v1.1), SaaS UI, federation, path C, mobile.

## 10. Open questions (parked, tracked)

- Project/product name (repo is still "headscale" fork).
- TUI ↔ registry auth (device-scoped API key issued at registration is
  the placeholder).
- Org `/56` sizing and the eventual >256-org story (registry-side
  allocation policy; stock-client /48 constraint is the boundary).
- Prefix strategy for path C era (per-org true RFC 4193 /48s once a
  composing client lifts the stock-range constraint).
