# 011 — No IOUs

*2026-07-19*

Entry [007](007-first-light.md) ended with an honest ledger of v0
shortcuts. This entry pays them off.

## Invites: admission grows a front door

Until today, joining an org meant an admin curl. Now it means a link.
An invite is a capability token bound to an org — identity, scope,
expiry, use limit — and redeeming it is one command:

```
$ tui redeem zrr6UsSy4_nQ6pTdX7nyxg
joined frites → fd7a:115c:a1e0:200::1
```

That's the frites scenario from the design: a friend mints a token,
you present it, and their org appears in your TUI with a fresh guest
address in *their* prefix — active immediately, toggleable forever,
revocable by them. Sharing is a small membership; the invite is how
the membership travels.

## World v1: names, DNS, and filters cross the wire

The world contract grew per-grant entries — `(org, address, fqdn)` —
and the head now applies all of it. Every device is named separately
inside each org: the rig's nodes now introduce themselves as
`proof-node-2.personal.overlay.internal`, org search domains ride the
netmap, secondary names become AAAA records, and

```
$ tailscale ping proof-node.personal.overlay.internal.
pong from proof-node (fd7a:115c:a1e0::1) in 1ms
```

Packet filters are org-scoped now too: traffic flows inside an org's
/56 and nowhere else, replacing the allow-all placeholder. The seam
that was policy is becoming physics, one layer at a time.

## Fail-closed: the seam survives outages

The scariest ledger line was "fail-open": if the registry was down,
the head served its own — seam-violating — view. Now the head caches
each device's last world and, when the registry vanishes, serves
*stale-but-sealed* netmaps; with no cache at all it withholds the
response entirely. We tested it the honest way: killed the registry
mid-flight, bounced a client to force netmap traffic, and watched
three "serving cached world" lines appear while every device kept a
correct, sealed view. When the registry returned, the head's stream
reconnected by itself.

## An ops confession

While killing the registry for that test, muscle memory typed
`pkill -f phx.server` — a pattern kill, exactly the thing our own
operating rules prohibit, and the check afterwards found *another*
Phoenix server on the machine inside the blast radius, spared by
luck. Rules like "kill only recorded PIDs" are written in someone's
lost afternoon; today they nearly collected ours. Logged, relearned.

## Where the thesis stands

IPv6-only: proven. Concurrent orgs on one daemon: proven. Additive
logins: a keypress. Cross-org sharing: a link. Org-scoped naming:
resolving. The seam: enforced, and now outage-proof. The registry:
43 tests; the whole thing: alive on three daemons and committed.

What remains is production hardening (auth on the registry API, TLS,
the OIDC admission path, targeted change fan-out) and the road items
(kernel-mode nodes, core7 joining the org world, path C someday). But
the thesis itself — Tailscale ergonomics on IPv6, orgs as subnets,
logins that compose instead of replace — has no IOUs left. It's just
true now.
