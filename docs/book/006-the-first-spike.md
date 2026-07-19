# 006 — The first spike

*2026-07-19*

The design's most load-bearing assumption was also its least examined:
per-org addressing only works if a stock Tailscale client will accept
*several* IPv6 self-addresses from one netmap and actually answer on
all of them. Dual-stack machinery said "probably." Nobody had proven
N×v6.

So before writing a line of registry code, we forced the question. A
spike branch hands every node a second address in a *different* /56 —
exactly what a second org grant will look like — and an integration
test demands that every client take both addresses and answer pings on
both, from every peer.

**Pass. 109 seconds.** Stock clients apply multiple v6 self-addresses
without complaint; peers route to all of them. The addressing model
stands on proof now, not plausibility. The spike hack is quarantined on
its branch with "never merge" written on it twice; the knowledge is
what merges.

Two smaller notes from the day:

**Rust enters the picture.** Upstream has `tailscale-rs`, a Rust
implementation of a Tailscale node — and our harness already knows how
to test against it. That matters because Rust (via Rustler) can live
*inside* the BEAM, where Go cannot: the v2 shape may collapse the
Go wire head into the Elixir registry entirely, one deployable instead
of two. We're not reaching for it yet — the Go path is what's proven,
and tailscale-rs is young — but the `World(device)` contract is
deliberately the kind of seam that makes "Go service now, Rust-in-BEAM
later" a swap rather than a rewrite. The Rust client stays in our test
matrix so we'll always know how compatible it is.

**Foundations are now on main.** The v6-only integration test and the
build fix it flushed out (upstream quietly requires Go ≥ 1.26.5; every
head-client test in the repo was silently broken) are committed. The
project's first CI-guarded promise: *v6-only operation with stock
clients is never allowed to regress.*

Next: M1 — the Phoenix registry, orgs, grants, and the composition
engine. The part where the thing becomes a thing.
