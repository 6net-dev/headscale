# Durable node removal (T-501 A3)

Base: pinned registry head 426e5024. The server writes an ordered change with
kind `world` (legacy default) or `removed`; removal has a newer revision but
no fabricated World. Architecture Manager cleared the contract.

Change `hscontrol/registry/world.go` and its reader to retain kind. Change
`registry_listener.go` to apply removal before advancing its cursor, and to
full-refresh on an unknown kind. A failed deletion must remain replayable.
Wire this in `app.go`. Refactor the deletion action used by `grpcv1.go` into
one shared function: delete durable/state identity, invalidate the cached
World, and publish the existing node-deleted change that closes its session
and announces peer removal. Unknown/already-deleted IDs succeed idempotently.

Reconnect and below-floor refresh must reconcile registered nodes, not merely
cached Worlds: authoritative registry 404 means removal; timeouts and other
transient failures preserve identity and cached data. Unknown-kind refresh
must not silently consume the event without doing that reconciliation.

Regressions first: removed event after a disconnected head resumes; actual
node deregistration and session closure; duplicate removal; deletion failure
does not advance the cursor; unknown-kind full refresh; legacy missing-kind
events remain world invalidations. The server suite covers old readers
ignoring kind, transactional append, compaction and post-revoke World 404.

Use local in-process Go tests before integration machinery. No dependencies,
protobuf or head database schema change is intended. Sign commits; final AM
review covers the server and head commits. No deploy or production writes.
