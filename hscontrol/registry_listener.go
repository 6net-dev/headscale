package hscontrol

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/juanfont/headscale/hscontrol/registry"
	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/juanfont/headscale/hscontrol/types/change"
	"github.com/rs/zerolog/log"
)

var errRegistryRemovalUnconfigured = errors.New("registry removal handler is not configured")

const (
	// registryReconnectDelay is the first wait after a stream ends, and the
	// unit the backoff doubles.
	registryReconnectDelay = 3 * time.Second

	// registryReconnectMax caps it. A head that has been away for a minute
	// is a head with a revocation SLA to meet (design §4: "the revocation
	// SLA is therefore 'head reachable', measured and alerted on"), so the
	// ceiling is low enough that recovery is prompt and high enough that a
	// Gentility restarting is not being hammered by every head at once.
	registryReconnectMax = 60 * time.Second

	// registrySaveInterval throttles the cache file. The cursor moves once
	// per change, and a Gentility rendering a busy installation would
	// otherwise have every head writing its state volume per event.
	//
	// What a throttled write costs on a crash is a replay of at most a
	// second of the stream, and a replay is free: applying an event twice
	// invalidates a cache entry that is already invalid and sends a node an
	// update it already had.
	registrySaveInterval = time.Second

	// registryReportInterval throttles the REPORT of a cache that will not
	// save. The save itself is attempted every time; what is bounded here
	// is how often a head whose state volume is not writable says so, since
	// the cause never changes and the file is written once a second.
	registryReportInterval = time.Minute
)

// worldCache is the head's cached Worlds as the change-stream runner drives
// them. [mapper.Batcher] is the implementation; it is an interface here
// because the runner's whole job is the ORDER it does these in — refresh
// before serve — and that is worth asserting without a control plane behind
// it.
type worldCache interface {
	// RefreshWorlds opens a refresh generation and reads every cached
	// World again, so nothing cached over a connection that has since
	// dropped can be served.
	RefreshWorlds(ctx context.Context)

	// InvalidateWorld forgets one node's World, which is what an event on
	// the stream means.
	InvalidateWorld(nodeID types.NodeID)

	// SnapshotWorlds and LoadWorlds are the two halves of the disk cache.
	SnapshotWorlds() map[types.NodeID]*registry.World
	LoadWorlds(worlds map[types.NodeID]*registry.World)
}

// changeStream is Gentility as the runner uses it: one long-lived
// subscription. [registry.Client] is the implementation.
type changeStream interface {
	Changes(ctx context.Context, since *int64, h registry.ChangeHandler) error
}

// registryRunner follows Gentility's change stream for the life of the
// process, and is the head's whole answer to "how does a grant change reach a
// device".
//
// # What one event means
//
// An event names a node and a revision, and nothing else: the World itself is
// fetched back by head node id, because a payload here would be a second,
// unordered copy of a stream whose only value is that it is ordered. So
// applying one is two moves — forget that node's cached World, and wake that
// node — and it is idempotent, which is what makes a replayed cursor free.
//
// The wake is [change.FullSelf], targeted at the one node the event names. The
// v0 listener sent [change.FullUpdate] on any change, which woke every device
// on the head for a document that had moved for one of them. Targeted updates
// are a design §8 prerequisite.
//
// # What a reconnect means
//
// Every (re)connect refreshes every cached World before an event is served,
// because a reconnect is exactly the window in which the head may have missed
// one and no gap in the stream may leave a stale netmap. That refresh is the
// ready gate; [mapper.beginWorldRefresh] documents what it does and does not
// promise.
//
// # What being cut off means
//
// Fail-static, never fail-closed. A head that cannot reach Gentility keeps
// serving the netmaps it last rendered — including access Gentility has since
// revoked — for as long as the cut lasts, and that is a deliberate choice
// design §4 makes and measures rather than a gap. It survives a restart
// because the cache is on disk.
type registryRunner struct {
	stream       changeStream
	worlds       worldCache
	change       func(...change.Change)
	path         string
	remove       func(types.NodeID) error
	refreshNodes func(context.Context) error

	// backoff and backoffMax bound the reconnect wait; saveEvery throttles
	// the cache file. Fields rather than constants so a test can wind them
	// down to what it can wait for.
	backoff    time.Duration
	backoffMax time.Duration
	saveEvery  time.Duration

	// cursor is the last seq applied, nil for a head that has none. Every
	// field below here is touched only from run's own goroutine —
	// [registry.ChangeHandler] promises both callbacks are made on the
	// goroutine that called Changes — so none of them is locked.
	cursor *int64

	// dirty is a cursor the throttle has not written yet; savedAt is when
	// the file was last written.
	dirty   bool
	savedAt time.Time

	// failing says whether the last save failed, and reportedAt when that
	// was last said out loud. Together they make the FIRST failure of a
	// run an Error and the rest throttled Warns — see [registryRunner.save].
	failing    bool
	reportedAt time.Time

	// reportEvery is [registryReportInterval] as a field, so a test can
	// separate "throttled" from "never happened".
	reportEvery time.Duration
}

func newRegistryRunner(
	stream changeStream,
	worlds worldCache,
	changeFn func(...change.Change),
	path string,
) *registryRunner {
	return &registryRunner{
		stream:      stream,
		worlds:      worlds,
		change:      changeFn,
		path:        path,
		backoff:     registryReconnectDelay,
		backoffMax:  registryReconnectMax,
		saveEvery:   registrySaveInterval,
		reportEvery: registryReportInterval,
	}
}

// run follows the stream until ctx is cancelled. [registryRunner.restore] is
// the caller's move and is made BEFORE this one: see there for why it cannot
// be the first thing this goroutine does.
func (r *registryRunner) run(ctx context.Context) {
	wait := r.backoff

	for ctx.Err() == nil {
		connected := false

		err := r.stream.Changes(ctx, r.cursor, registry.ChangeHandler{
			Connected: func() error {
				connected = true

				refreshErr := r.refresh(ctx)
				if refreshErr != nil {
					return refreshErr
				}

				// The refresh just made every cached World
				// current, so the disk copy is owed one write
				// even on an installation quiet enough that no
				// event ever moves the cursor.
				r.dirty = true
				r.save()

				return nil
			},
			Changed: func(c registry.Change) error { return r.apply(ctx, c) },
		})

		// Whatever ended the stream, the cursor the throttle was
		// holding is written now: the next thing to happen may be a
		// restart.
		r.save()

		if ctx.Err() != nil {
			return
		}

		if connected {
			// The connection worked; whatever ended it, the next
			// attempt starts from the shortest wait again.
			wait = r.backoff
		}

		if errors.Is(err, registry.ErrBehindHorizon) {
			// A cursor there was is a cursor worth retrying
			// without the wait; the reconnect asks for the tail
			// and cannot earn the same refusal. A cursor there was
			// NOT is Gentility refusing the tail itself, which is
			// a fault rather than a compaction, and reconnecting
			// on it in a tight loop would be this head hammering a
			// registry that is already unhappy.
			if r.behindHorizon(err) {
				continue
			}
		}

		r.report(err)

		if !sleep(ctx, jitter(wait)) {
			return
		}

		wait = min(wait*2, r.backoffMax)
	}
}

// apply advances the cursor only after the event's effect succeeds. Unknown
// kinds require a full refresh; malformed known events and failed removals
// retain the cursor so reconnect cannot silently lose a revocation.
func (r *registryRunner) apply(ctx context.Context, c registry.Change) error {
	if c.Kind != "" && c.Kind != "world" && c.Kind != "removed" {
		err := r.refresh(ctx)
		if err != nil {
			return err
		}

		r.cursor = &c.Seq
		r.dirty = true
		r.save()

		return nil
	}
	nodeID, err := strconv.ParseUint(c.HeadNodeID, 10, 64)
	if err != nil {
		return fmt.Errorf("registry change %d has an invalid head node id: %w", c.Seq, err)
	}

	switch c.Kind {
	case "", "world":
		r.worlds.InvalidateWorld(types.NodeID(nodeID))
		r.change(change.FullSelf(types.NodeID(nodeID)))
	case "removed":
		if r.remove == nil {
			return errRegistryRemovalUnconfigured
		}

		err := r.remove(types.NodeID(nodeID))
		if err != nil {
			return err
		}
	}

	r.cursor = &c.Seq
	r.dirty = true
	r.save()

	return nil
}

func (r *registryRunner) refresh(ctx context.Context) error {
	if r.refreshNodes != nil {
		err := r.refreshNodes(ctx)
		if err != nil {
			return err
		}
	}

	r.worlds.RefreshWorlds(ctx)

	return nil
}

// deleteRegistryNode is shared by the DELETE API and durable feed. Removing
// cached state and notifying the batcher happen only after durable deletion.
func (h *Headscale) deleteRegistryNode(id types.NodeID) error {
	if node, ok := h.state.GetNodeByID(id); ok {
		removed, err := h.state.DeleteNode(node)
		if err != nil {
			return err
		}

		h.mapBatcher.InvalidateWorld(id)
		h.Change(removed)
	} else {
		h.mapBatcher.InvalidateWorld(id)
		h.Change(change.NodeRemoved(id))
	}

	return nil
}

// Refresh registered identities as well as cached Worlds. A cursor below the
// compaction floor may have missed removal while the node had no cached World.
func (h *Headscale) refreshRegistryNodes(ctx context.Context, client *registry.Client) error {
	limit := h.cfg.RegistryRefreshTimeout
	if limit <= 0 {
		limit = types.DefaultRegistryRefreshTimeout
	}

	ctx, cancelSweep := context.WithTimeout(ctx, limit)
	defer cancelSweep()

	for _, node := range h.state.ListNodes().All() {
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err := client.World(readCtx, registry.NodeID(node.ID().Uint64()))

		cancel()

		if errors.Is(err, registry.ErrUnknownNode) {
			err := h.deleteRegistryNode(node.ID())
			if err != nil {
				return err
			}
		} else if err != nil && !errors.Is(err, registry.ErrNotRendered) {
			return err
		}
	}

	return nil
}

// behindHorizon clears a cursor below what Gentility still holds: the events it
// missed no longer exist. It is cleared rather than reset to zero — every seq
// below the horizon is gone, so zero earns the same 410 forever — and the
// reconnect asks for the tail. The refresh that makes the head level again is
// the one the next Connected does, before it serves anything.
//
// It answers whether the reconnect may skip the backoff, which it may only
// when there was a cursor to spend. A 410 for a stream the head asked no
// cursor for is not a compaction and clearing nothing would not fix it.
func (r *registryRunner) behindHorizon(err error) bool {
	event := log.Warn()

	var behind *registry.BehindHorizonError
	if errors.As(err, &behind) {
		event = event.Int64("compaction.horizon", behind.CompactionHorizon)
	}

	if r.cursor == nil {
		event.Err(err).Msg("registry refused a stream the head asked no cursor for")

		return false
	}

	event.Int64("cursor", *r.cursor).
		Msg("registry cursor fell behind the compaction horizon; asking for the tail")

	r.cursor = nil
	r.dirty = true
	r.save()

	return true
}

// restore puts the last cache back before the first connection, so a head that
// boots to an unreachable Gentility has the netmaps it last rendered.
//
// It is called by the caller, synchronously, before [registryRunner.run] is
// started — not as run's own first statement. run is a goroutine, and the
// head starts listening at the same moment: a poll that arrives before the
// scheduler reaches this would be answered from an EMPTY cache. Which is a
// full round of on-demand fetches at best, and at worst — the boot into an
// unreachable Gentility this file is entirely about — a withheld map response
// for a node whose document is sitting on the disk unread.
//
// A cache it cannot read is a cold start rather than a failed one: everything
// in it can be asked for again, so the cost is one full round of on-demand
// fetches and never a boot.
func (r *registryRunner) restore() {
	cache, err := loadRegistryCache(r.path)
	if err != nil {
		log.Warn().Err(err).Str("path", r.path).
			Msg("registry cache unreadable; starting with no cursor and no cached worlds")

		return
	}

	r.cursor = cache.Cursor
	r.worlds.LoadWorlds(cache.worldsByNodeID())
}

// save writes the cursor and the cache, at most once every saveEvery.
func (r *registryRunner) save() {
	if !r.dirty || time.Since(r.savedAt) < r.saveEvery {
		return
	}

	if err := saveRegistryCache(r.path, registryCacheOf(r.worlds.SnapshotWorlds(), r.cursor)); err != nil {
		// Not fatal, and not retried here: the next event tries again,
		// and a head that never manages to write one is a head that
		// full-refreshes on its next boot.
		r.reportSaveFailure(err)

		return
	}

	r.failing = false
	r.dirty = false
	r.savedAt = time.Now()
}

// reportSaveFailure says a cache write failed, at the level the failure
// deserves.
//
// # Why the first one is an Error
//
// Nothing stops. The head serves, follows the stream and refreshes exactly as
// it did; what it silently loses is fail-static across a restart, coming back
// with no cursor and no documents and full-refreshing every node before it can
// serve any of them. On a large installation that is a slow, self-inflicted
// outage on every deploy, and its only symptom is this line. The usual cause
// is a bind-mounted state volume still owned by root — see the Dockerfile and
// docs/registry-mode.md — which is a five-second fix an operator will never
// make if it is reported at the level everything transient is reported at.
//
// # Why the rest are throttled warnings
//
// The cause does not change and the file is written once a second, so an
// unwritable volume would otherwise produce a log line per second forever:
// an alarm that fires continuously is one nobody reads. A save that then
// works clears the flag, so a LATER failure is a new fault and is an Error
// again rather than a continuation of the old one.
func (r *registryRunner) reportSaveFailure(err error) {
	if !r.failing {
		r.failing = true
		r.reportedAt = time.Now()

		log.Error().Err(err).Str("path", r.path).
			Msg("could not persist the registry cache; this head will full-refresh on every restart")

		return
	}

	if time.Since(r.reportedAt) < r.reportEvery {
		return
	}

	r.reportedAt = time.Now()

	log.Warn().Err(err).Str("path", r.path).Msg("could not persist the registry cache")
}

// report says what ended the stream, at the level the ending deserves. A clean
// end is the deadline Gentility sets on its own response (15 minutes) and is
// not news; an idle stream and an error are.
func (r *registryRunner) report(err error) {
	switch {
	case err == nil:
		log.Debug().Msg("registry change stream ended at its deadline; reconnecting")
	case errors.Is(err, registry.ErrStreamIdle):
		log.Warn().Err(err).Msg("registry change stream went idle; reconnecting")
	default:
		log.Warn().Err(err).Msg("registry change stream failed; reconnecting")
	}
}

// jitter spreads reconnects so a Gentility coming back does not meet every
// head it serves at the same instant. Half the wait is kept rather than
// jittering the whole of it: full jitter can return a delay near zero, which
// is a reconnect storm with extra steps.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}

	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

// sleep waits, and answers whether the wait finished rather than the process
// shutting down.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
