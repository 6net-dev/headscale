# 002 — Where the union lives

*2026-07-19*

First real design decision, and it turned out not to be the question we
started with. We set out to choose a control-plane topology — one shared
server, or federation, or something else — and discovered the actual
decision hiding underneath: **where does the netmap union get computed?**

A device's daemon holds one stream to one control server, and receives
its personalized world: peers, keys, endpoints, names. Our whole feature
— personal + work + a friend's shared subnet, composing additively as
you log in and out — means someone assembles that union. And assembly
requires holding the pieces. Whoever computes your union, *sees* your
union. That's not a policy problem; it's arithmetic. There are exactly
three places it can live:

1. **One shared server** (orgs as tenants): the operator sees every
   org's everything. This is precisely Tailscale's own SaaS trust model,
   so it's not exotic — but it turns unacceptable the moment the
   operator is a private individual inside one of the orgs. No company
   parks its network topology on an employee's home server.
2. **A home server, federated** (Matrix-style): each org keeps its own
   server, but since the client speaks to one endpoint, org slices must
   *transit the user's home server* to be composed. Federation doesn't
   remove the leak — it relocates it, in the worst direction for the
   corporate case. Its genuine win is offboarding (an org's own server
   enforces its own boundary), but the price is a server-to-server
   protocol before anything works, and it makes cross-org sharing — our
   headline feature — the hardest part instead of the easiest.
3. **The device itself** (a forked client holding N control sessions,
   merging locally): each org server sees only its slice; the union
   exists only in the device's RAM. The only topology where nobody but
   you sees your whole world — by construction, not by promise. The
   cost is concentrated and honest: you own a tailscaled fork forever.

The decisive observation: **options 1 and 3 are the same road.** If the
shared server keeps strict org seams — org ID on every record, per-org
prefixes, keys, and policies, the union computed in exactly one place
(the netmap builder) and nowhere else — then that same server can later
serve as one single-org server among many, under a composing client.
Federation is the only road that doesn't connect to the others.

## The decision

**Ship the single multi-org server first, with seams for on-device
composition later.** V1 targets personal and community networks, where
"one trusted operator" is already the accepted norm. Logins-as-grants
and cross-org sharing become cheap core features (grant rows and policy
edges in one database). Corporate-grade isolation — where no third party
may see the union — is deferred to the composing-client fork, and every
schema decision between now and then gets tested against one question:
*would this still work if the union moved into the client?*

Deferred, not forgotten: the seams are load-bearing. The moment a
cross-org join creeps into a hot path, or a record exists without an
org, the road to composition-on-device quietly closes behind us.
