# 001 — Two complaints, one cause

*2026-07-19*

I like Tailscale. I like it enough that I've been living inside a headscale
fork for months. But two things about it have never stopped grating:

1. It looks and feels IPv4-only. Every address you touch is a `100.x`.
2. You can run exactly one tailnet at a time. Logging into work *replaces*
   my personal network instead of adding to it.

This project started the day I realized these are not two complaints.
They are one complaint.

## The squat

Every tailnet in the world lives on the same IPv4 range: `100.64.0.0/10`,
carrier-grade NAT space that Tailscale quietly squats on. It's a fine hack
for one network — the addresses are stable, memorable, and every legacy
app on earth can dial them. But it means any two tailnets are guaranteed
to collide: same addresses, same routes, one routing table. Concurrent
multi-tailnet operation isn't a missing feature; it is *arithmetically
impossible* while v4 is the default. The best Tailscale could ship was
profile switching — swap the whole world when you change badge.

IPv6 has an answer to exactly this, and it's twenty years old. RFC 4193
Unique Local Addresses: take `fd00::/8`, generate a random 40-bit ID, and
you own a /48 that collides with anyone else's ULA prefix at odds of
about one in a trillion. Private networks that compose by construction.
Tailscale even uses it — every node gets an address inside
`fd7a:115c:a1e0::/48` — but as plumbing, never as the product.

So the thesis: drop IPv4 entirely, give every network its own slice of
v6 space, and the single-network limit should simply evaporate.

## Is the stack actually married to v4?

Before building anything new, we audited the headscale codebase for v4
coupling. The answer was startling: **there is essentially none.** IP
allocation, netmap construction, MagicDNS, and the entire policy engine
run on `netip.Prefix` with every v4 access nil-guarded. A config with no
v4 prefix at all is already legal — it just had never been tested, and
two v4-only features quietly become no-ops. The client side tells the
same story: there's an IPv6 MagicDNS resolver address
(`fd7a:115c:a1e0::53`), 4via6 subnet mapping, and an official
`disable-ipv4` node attribute. Tailscale themselves ship a mode where a
node drops its own v4 address.

The v4-ness of Tailscale is a *presentation decision*, not an
architecture. Which makes sense once you see who they sell to: the
enterprise buyer needs `100.x` to work with every crusty tool they own.
The cost of that choice — no concurrent networks, netmap-replacing
logins — lands on people like me, who were never the customer anyway.

## Proving it, twice

First in CI: a new integration test, `TestPingAllByIPv6Only`, runs a
headscale with `prefixes.v4` unset against four real Tailscale clients,
including one built from upstream `main` the same day. Every node gets
exactly one address — a ULA, no CGNAT residue anywhere — and every node
reaches every other by IPv6 address and by MagicDNS hostname. Green in
178 seconds. (Amusing speed bump: upstream had bumped its Go requirement
that very week, which had silently broken every head-client integration
test in the repo. One-line fix, unrelated to v6.)

Then on real hardware, which is where it gets fun. A v6-only headscale
on a Linux box, prefix `fd7a:115c:a1e0:ab00::/56`. Two *stock,
unmodified* open-source Tailscale daemons — one on the Linux box, one on
a Mac — each running in userspace mode with its own state and socket, no
root required. Both joined, drew `…ab00::1` and `…ab00::2`, and pinged
each other directly at 1–5ms — by address and by name, with the overlay
riding the LAN's native IPv6 as its transport.

And here is the picture that justifies the whole idea: **that Linux box
was a member of my real, personal, IPv4 tailnet the entire time.** Two
overlay networks, one machine, both live, zero interference — the thing
Tailscale cannot do, demonstrated with Tailscale's own client, because
the experimental network brought no colliding routes to the table.

## Where this goes

The validated foundation: headscale is already address-family agnostic,
stock clients handle v6-only fine, and disjoint ULA prefixes make
networks composable. On top of it we're going to build the thing I
actually want:

- **One daemon** on each device — no N-daemon circus.
- **One global IPv6 network**, with organizations as subnets inside it.
- **Logins as grants**: authenticating to an org *adds* its subnet to
  your world; logging out removes it. Nothing else moves.
- **Cross-org sharing** as a policy edge, not a hack.

The control plane composes your netmap server-side as the union of your
active grants; the client never needs to know organizations exist. All
the new machinery lands in the half of the stack we already control.

Next entry: designing the org and grant model.
