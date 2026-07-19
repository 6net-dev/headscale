# 010 — Six o'clock is a keypress

*2026-07-19*

The curl commands have graduated. There is now a TUI:

```
  overlay · proof-node

  ▶ ● personal   fd7a:115c:a1e0::1        1 peer
    ● work       fd7a:115c:a1e0:100::2    1 peer

  synced 15:00:52 · space toggle · j/k move · q quit
```

That is the multi-org laptop from [009](009-personal-and-work.md):
two green dots, two addresses, per-org peer counts. Cursor onto
`work`, press space, and the six o'clock toggle from
[008](008-the-six-oclock-toggle.md) fires — dot hollows out, address
dims, the work overlay dissolves, personal never blinks. Space again
in the morning.

The work-only node tells the quieter half of the story:

```
  overlay · proof-node-3

  ▶ ○ personal   fd7a:115c:a1e0::3        —
    ● work       fd7a:115c:a1e0:100::1    1 peer
```

The hollow dot is an *inactive grant still holding its address* — you
can see the design's promise (deactivate ≠ revoke; your slot and your
address survive the night) rendered as a single dim line.

Implementation notes, brief because the whole point is how little
there is: ~300 lines of Go and bubbletea in `cmd/tui`. It reads the
device's identity from the local tailscaled socket, speaks only the
registry's HTTP API (grants list — one new endpoint — world, and the
toggle), refreshes every three seconds, and contains zero protocol
logic. A `--once` flag renders a single frame without a TTY, which
makes it a status command (`tui --once`) and lets CI and build logs
capture real frames — both screenshots above are actual output
against the live rig.

The stack, end to end, is now: Postgres grant → Elixir composition →
SSE → Go rewrite → stock Tailscale client → WireGuard — with a
keypress on top. Every layer built today, every layer tested, and the
whole thing small enough to hold in your head.

Next: invites (so admission stops being an admin curl), DNS and
filters from the world, and the fail-closed hardening — then the rig
graduates from proof to daily driver.
