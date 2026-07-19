# 008 — The six o'clock toggle

*2026-07-19*

This project exists because logging out of a work tailnet at six
o'clock shouldn't demolish your personal network. Today that sentence
stopped being a complaint and became a demo.

## The missing wire

After [007](007-first-light.md), truth lived in the registry but
traveled only opportunistically: a grant flipped in Postgres reached
clients whenever headscale happened to push a map for its own reasons.
The toggle needed a nervous system — three small pieces:

- **An activation API**: `PUT /api/devices/:node_key/grants/:org`
  with `{"active": bool}`. This is the endpoint the TUI will call; for
  now, curl is the TUI.
- **A change stream**: every grant mutation broadcasts the affected
  node keys over Phoenix PubSub, and an SSE endpoint pipes them out.
  The audience is computed conservatively — everyone holding a grant
  in the touched org — because over-notifying costs a map push and
  under-notifying strands a stale netmap.
- **A listener in the head**: subscribes to the stream and, v0, fans a
  full update to every connected node. Targeted pushes come later;
  at dogfood scale, correctness beats cleverness.

## The bug that taught the protocol

First run: toggling off removed the device's *address* instantly but
left ghost *peers* on both sides. The cause is a Tailscale protocol
subtlety worth writing down: an empty peer list in a MapResponse is
omitted from the wire, and an omitted peer list means "no change" —
so silently dropping peers tells the client nothing. Removal must be
said out loud, in `PeersRemoved`. One fix later, the rewrite announces
every peer it drops.

## The demo

Two stock clients on registry addresses `…::1` and `…::2`, seeing each
other, pinging. Then:

```
PUT …/grants/personal {"active": false}
```

Four seconds later: node1 has **no address and no peers**; node2's
netmap simply no longer contains node1. No re-login, no profile swap,
no flicker on anything else.

```
PUT …/grants/personal {"active": true}
```

Four seconds later: node1 is back — at the **same address**, because
the grant keeps its host slot through deactivation — both sides see
each other, and a WireGuard ping crosses the restored path in 1ms.

Activation is now what the design said it should be: an instant,
ceremony-free bit-flip on a standing admission. The expensive identity
dance happens once, at admission; the six o'clock toggle costs
nothing.

Next: a second org on the rig (the actual personal + work shape), DNS
and filters from the world, and the TUI to replace curl.
