# 005 — What a company needs

*2026-07-19*

Design dialogue complete. The last stretch was a discipline exercise:
we inventoried every mechanism in Tailscale's control protocol — the
actual `tailcfg` surface our pinned client speaks, cross-checked
against what headscale implements — and gave each one an explicit
verdict. The rule came from a simple observation: Tailscale's feature
list is a map of production problems they really hit. Anything they
think they need, we should think carefully about whether we need too.

The pattern that fell out fits in one sentence: **everything Tailscale
needs because it's a *network*, we keep; most of what it needs because
it's a *company*, we don't.**

The network parts — Noise transport, netmap streaming, DERP relaying,
subnet routing with failover, packet filters, MagicDNS — we inherit,
working, from headscale. The company parts sort into three piles:

- **SaaS-operator machinery** (ControlDialPlan, client-update nudges):
  reaching and managing a global anycast service. A self-hosted
  registry with a DNS name doesn't have the problem.
- **Enterprise compliance products** (device posture, flow logs, SSH
  session recording, audit streams): we defer most, and one we
  *reject* — per-connection flow logging is a permanent non-goal.
  Written down as policy, so future-us has to argue with past-us in
  public to change it.
- **Trust patches** (TKA / tailnet lock): quorum-signing so a
  compromised control plane can't forge nodes — a cryptographic patch
  for exactly the composition-point problem from
  [002](002-where-the-union-lives.md). Our answer is architectural
  (move composition on-device, eventually), not ceremonial. Until
  then we carry the same risk Tailscale-without-TKA does, and we say
  so instead of performing security.

One drop deserves its own line: Tailscale's control plane can order a
client process to sleep or exit (`Debug.Exit`). We deleted that power
on principle. **The registry composes worlds; it does not command
machines.** In a multi-org system, minimizing what the control plane
can do *to* a device is a feature you can sell.

A late but happy twist: the registry will be Elixir/Phoenix/Postgres,
with a thin Go "wire head" carved from headscale keeping the exotic
protocol parts — Noise, netmap rendering, DERP — so Elixir never
reimplements them. Go speaks Tailscale so Elixir doesn't have to. The
contract between them, `World(device)`, is the org-seam made concrete,
and it happens to be shaped exactly like what a future composing
client would consume. The stack choice made the long-term architecture
*more* honest, not less.

The design doc is committed
(`docs/plans/2026-07-19-org-grant-overlay-design.md`). Next: three
spikes in the integration harness — multi-address self nodes above all,
since the whole addressing model stands on it — then the heart
transplant: orgs, grants, and the composition engine.
