package hscontrol

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juanfont/headscale/hscontrol/registry"
	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/juanfont/headscale/hscontrol/types/change"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeWorldCache records what the runner did to the head's World cache and in
// what order, since the order is the contract: every cached World is refreshed
// before a single event is served.
type fakeWorldCache struct {
	mu     sync.Mutex
	steps  []string
	loaded map[types.NodeID]*registry.World
	worlds map[types.NodeID]*registry.World
}

func newFakeWorldCache() *fakeWorldCache {
	return &fakeWorldCache{worlds: map[types.NodeID]*registry.World{}}
}

func (f *fakeWorldCache) RefreshWorlds(context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.steps = append(f.steps, "refresh")
}

func (f *fakeWorldCache) InvalidateWorld(nodeID types.NodeID) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.steps = append(f.steps, fmt.Sprintf("invalidate %d", nodeID))
}

func (f *fakeWorldCache) SnapshotWorlds() map[types.NodeID]*registry.World {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.worlds
}

func (f *fakeWorldCache) LoadWorlds(worlds map[types.NodeID]*registry.World) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.loaded = worlds
	f.steps = append(f.steps, "load")
}

func (f *fakeWorldCache) taken() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.steps...)
}

// fakeGentility is the change stream as Gentility serves it: SSE frames in the
// shape GentilityWeb.Overlay.Overlay.HeadController writes, one connection at
// a time, scripted per connection.
type fakeGentility struct {
	mu sync.Mutex

	// since is the raw ?since= of every connection, "" when the parameter
	// was omitted — which is what a head with no cursor sends and is not
	// the same request as since=0.
	since []string

	// script answers connection n. A nil entry serves nothing and holds
	// the connection open until the test's context ends.
	script []func(w http.ResponseWriter, flush func())

	connected chan struct{}
}

func newFakeGentility(script ...func(w http.ResponseWriter, flush func())) *fakeGentility {
	return &fakeGentility{script: script, connected: make(chan struct{}, len(script)+8)}
}

func (f *fakeGentility) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	n := len(f.since)
	f.since = append(f.since, r.URL.Query().Get("since"))

	var act func(http.ResponseWriter, func())
	if n < len(f.script) {
		act = f.script[n]
	}
	f.mu.Unlock()

	f.connected <- struct{}{}

	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	if act == nil {
		// An open stream with nothing on it: the 200 arrives, and then
		// no line ever does. That is what the idle watchdog exists for,
		// and it is not the same as a peer that never answers at all.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flush()

		<-r.Context().Done()

		return
	}

	act(w, flush)
}

func (f *fakeGentility) sinces() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.since...)
}

// events writes SSE frames the way the controller does — "id: <seq>", one
// "data:" line, blank line — and then ends the response, which is a clean end
// of stream the head reconnects from.
func events(changes ...registry.Change) func(http.ResponseWriter, func()) {
	return func(w http.ResponseWriter, flush func()) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		for _, c := range changes {
			fmt.Fprintf(w, "id: %d\ndata: {\"seq\":%d,\"head_node_id\":%q,\"world_revision\":%d}\n\n",
				c.Seq, c.Seq, c.HeadNodeID, c.WorldRevision)
			flush()
		}
	}
}

func gone(horizon int64) func(http.ResponseWriter, func()) {
	return func(w http.ResponseWriter, _ func()) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		fmt.Fprintf(w, `{"error":"behind_horizon","compaction_horizon":%d}`, horizon)
	}
}

// serve starts one fake Gentility. Its Close is registered BEFORE the runner's
// is, so cleanup's LIFO order stops the runner first: closing a server a head
// is still streaming from blocks for five seconds and says so.
func serve(t *testing.T, gentility *fakeGentility) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(gentility)
	t.Cleanup(server.Close)

	return server
}

// testRunner is a runner wired to one fake Gentility, with the delays wound
// down to what a test can wait for.
func testRunner(
	t *testing.T,
	server *httptest.Server,
	cache *fakeWorldCache,
	path string,
) (*registryRunner, chan change.Change) {
	t.Helper()

	changes := make(chan change.Change, 32)

	runner := newRegistryRunner(
		registry.New(server.URL, "secret", registry.WithIdleTimeout(150*time.Millisecond)),
		cache,
		func(cs ...change.Change) {
			for _, c := range cs {
				changes <- c
			}
		},
		path,
	)
	runner.backoff = 5 * time.Millisecond
	runner.backoffMax = 20 * time.Millisecond
	runner.saveEvery = 0

	return runner, changes
}

// run starts the runner and stops it when the test ends.
func run(t *testing.T, runner *registryRunner) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)
		runner.run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(2 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}

// The ordinary life of the stream: connect, refresh, apply what arrives, and
// reconnect from the cursor the last event set.
func TestRunnerFollowsTheStreamAndResumesFromItsCursor(t *testing.T) {
	gentility := newFakeGentility(
		events(
			registry.Change{Seq: 11, HeadNodeID: "3", WorldRevision: 1},
			registry.Change{Seq: 12, HeadNodeID: "4", WorldRevision: 2},
		),
	)
	server := serve(t, gentility)

	cache := newFakeWorldCache()
	runner, changes := testRunner(t, server, cache, filepath.Join(t.TempDir(), "cache.json"))
	run(t, runner)

	first := <-changes
	second := <-changes
	assert.Equal(t, types.NodeID(3), first.TargetNode)
	assert.Equal(t, types.NodeID(4), second.TargetNode)
	assert.True(t, first.IncludeSelf, "the node whose World moved gets a full self update")

	waitFor(t, "a reconnect", func() bool { return len(gentility.sinces()) > 1 })

	sinces := gentility.sinces()
	assert.Equal(t, "", sinces[0], "a head with no cursor asks for the tail, not since=0")
	assert.Equal(t, "12", sinces[1], "the reconnect resumes from the last applied seq")
}

// The ready gate, from the runner's side: the refresh is not merely done, it
// is done BEFORE the first event is applied. A head that served an event
// against a cache it had not refreshed would be answering out of a connection
// that had already dropped.
func TestRunnerRefreshesEveryCachedWorldBeforeItServesAnEvent(t *testing.T) {
	gentility := newFakeGentility(
		events(registry.Change{Seq: 5, HeadNodeID: "3", WorldRevision: 1}),
	)
	server := serve(t, gentility)

	cache := newFakeWorldCache()
	runner, changes := testRunner(t, server, cache, filepath.Join(t.TempDir(), "cache.json"))
	run(t, runner)

	<-changes

	assert.Equal(t, []string{"load", "refresh", "invalidate 3"}, cache.taken()[:3],
		"the disk cache is loaded, then refreshed, and only then is an event served")
}

// A cursor below the compaction horizon is answered 410 before a single event
// is delivered. The cursor is spent — every seq below the horizon is gone — so
// the head clears it and reconnects asking for the TAIL. Not since=0: zero is
// a point Gentility has already compacted away, and asking for it would earn
// the same 410 forever.
func TestRunnerClearsItsCursorOnAGone(t *testing.T) {
	gentility := newFakeGentility(
		gone(900),
		events(registry.Change{Seq: 901, HeadNodeID: "3", WorldRevision: 1}),
	)
	server := serve(t, gentility)

	path := filepath.Join(t.TempDir(), "cache.json")
	stale := int64(7)
	require.NoError(t, saveRegistryCache(path, registryCache{Cursor: &stale}))

	cache := newFakeWorldCache()
	runner, changes := testRunner(t, server, cache, path)
	run(t, runner)

	<-changes

	sinces := gentility.sinces()
	require.GreaterOrEqual(t, len(sinces), 2)
	assert.Equal(t, "7", sinces[0], "the persisted cursor is what is tried first")
	assert.Equal(t, "", sinces[1], "a spent cursor is cleared, and the head asks for the tail")

	waitFor(t, "the new cursor to reach the disk", func() bool {
		loaded, err := loadRegistryCache(path)

		return err == nil && loaded.Cursor != nil && *loaded.Cursor == 901
	})
}

// The cursor survives the process. A head that restarts picks the stream up
// where it left off rather than replaying it or skipping it.
func TestRunnerPersistsItsCursorAcrossARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")

	first := newFakeGentility(events(registry.Change{Seq: 12, HeadNodeID: "3", WorldRevision: 1}))
	firstServer := httptest.NewServer(first)

	firstRunner, changes := testRunner(t, firstServer, newFakeWorldCache(), path)
	firstCtx, stop := context.WithCancel(t.Context())
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)
		firstRunner.run(firstCtx)
	}()

	<-changes
	waitFor(t, "the cursor to reach the disk", func() bool {
		loaded, err := loadRegistryCache(path)

		return err == nil && loaded.Cursor != nil
	})

	stop()
	<-stopped
	firstServer.Close()

	// A new process, the same state directory.
	second := newFakeGentility(nil)
	secondServer := serve(t, second)

	secondRunner, _ := testRunner(t, secondServer, newFakeWorldCache(), path)
	run(t, secondRunner)

	waitFor(t, "the restarted head to subscribe", func() bool { return len(second.sinces()) > 0 })
	assert.Equal(t, "12", second.sinces()[0])
}

// Fail-static across a restart: the head boots to a Gentility it cannot reach,
// and the netmaps it last rendered are back in the mapper's cache before the
// first client asks for one. It keeps retrying, and does not spin.
func TestRunnerServesAPersistedCacheWhenGentilityIsUnreachable(t *testing.T) {
	server := httptest.NewServer(newFakeGentility())
	server.Close() // nothing is listening; every connection is refused

	path := filepath.Join(t.TempDir(), "cache.json")
	require.NoError(t, saveRegistryCache(path, registryCache{
		Worlds: map[string]*registry.World{"3": cachedWorld("3")},
	}))

	cache := newFakeWorldCache()
	runner, _ := testRunner(t, server, cache, path)
	run(t, runner)

	waitFor(t, "the persisted worlds to be restored", func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()

		return cache.loaded != nil
	})

	cache.mu.Lock()
	assert.Equal(t, map[types.NodeID]*registry.World{types.NodeID(3): cachedWorld("3")}, cache.loaded)
	cache.mu.Unlock()
}

// An open connection is not a working stream, and nothing in TCP tells the two
// apart. Gentility heartbeats every 15s, so silence past the idle window is a
// stream that has stopped: the head tears it down and reconnects with the
// cursor it has.
func TestRunnerReconnectsAfterAnIdleStream(t *testing.T) {
	gentility := newFakeGentility(nil, nil)
	server := serve(t, gentility)

	cache := newFakeWorldCache()
	runner, _ := testRunner(t, server, cache, filepath.Join(t.TempDir(), "cache.json"))
	run(t, runner)

	waitFor(t, "the idle stream to be replaced", func() bool { return len(gentility.sinces()) > 1 })
}

// A head that cannot read its cache file starts cold rather than not starting:
// everything in it can be asked for again, so a corrupt cache costs a full
// refresh and never a boot.
func TestRunnerStartsColdOnAnUnreadableCache(t *testing.T) {
	gentility := newFakeGentility(nil)
	server := serve(t, gentility)

	path := filepath.Join(t.TempDir(), "cache.json")
	require.NoError(t, writeUnreadable(path))

	cache := newFakeWorldCache()
	runner, _ := testRunner(t, server, cache, path)
	run(t, runner)

	waitFor(t, "the head to subscribe anyway", func() bool { return len(gentility.sinces()) > 0 })
	assert.Equal(t, "", gentility.sinces()[0], "a cache it could not read leaves it with no cursor")
}

// # A cache the head cannot write is the fault that looks like nothing
//
// Nothing stops: the head serves, follows the stream and refreshes exactly as
// it did. What it loses is the whole of fail-static across a restart — it
// comes back with no cursor and no documents, and full-refreshes every node
// before it can serve any of them. The usual cause is a bind-mounted volume
// still owned by root, and the only symptom is a log line.
//
// So the first one is an Error and the rest are Warns, throttled: an alarm
// that fires once is one an operator can act on, and an alarm that fires every
// second is one they filter.
func TestRunnerReportsTheFirstCacheSaveFailureAtError(t *testing.T) {
	logs := captureLogs(t)

	cache := newFakeWorldCache()
	runner := newRegistryRunner(nil, cache, nil, unwritablePath(t))
	runner.saveEvery = 0
	runner.reportEvery = 0

	for range 5 {
		runner.dirty = true
		runner.save()
	}

	assert.Equal(t, 1, strings.Count(logs.String(), `"level":"error"`),
		"the first failure is the news")
	assert.Equal(t, 4, strings.Count(logs.String(), `"level":"warn"`),
		"the rest are warnings")
	assert.Contains(t, logs.String(), "could not persist the registry cache")
}

// The throttle: the cause of an unwritable volume never changes and the file
// is written once a second, so a head that says so every second is a head
// nobody reads.
func TestRunnerThrottlesRepeatedCacheSaveFailures(t *testing.T) {
	logs := captureLogs(t)

	cache := newFakeWorldCache()
	runner := newRegistryRunner(nil, cache, nil, unwritablePath(t))
	runner.saveEvery = 0

	for range 5 {
		runner.dirty = true
		runner.save()
	}

	assert.Equal(t, 1, strings.Count(logs.String(), `"level":"error"`))
	assert.Equal(t, 0, strings.Count(logs.String(), `"level":"warn"`),
		"nothing repeats inside the report interval")
}

// A save that works after a run of failures is the fault clearing, and the
// next failure is a new fault rather than a continuation of the old one.
func TestRunnerReportsAFreshCacheFailureAgain(t *testing.T) {
	logs := captureLogs(t)

	dir := t.TempDir()

	cache := newFakeWorldCache()
	runner := newRegistryRunner(nil, cache, nil, filepath.Join(dir, "cache.json"))
	runner.saveEvery = 0
	runner.reportEvery = time.Hour

	runner.path = unwritablePath(t)
	runner.dirty = true
	runner.save()

	runner.path = filepath.Join(dir, "cache.json")
	runner.dirty = true
	runner.save()

	runner.path = unwritablePath(t)
	runner.dirty = true
	runner.save()

	assert.Equal(t, 2, strings.Count(logs.String(), `"level":"error"`),
		"a failure after a working save is a new fault, not a continuation")
}

// A cache that saves says nothing at all.
func TestRunnerIsSilentWhenTheCacheSaves(t *testing.T) {
	logs := captureLogs(t)

	cache := newFakeWorldCache()
	runner := newRegistryRunner(nil, cache, nil, filepath.Join(t.TempDir(), "cache.json"))
	runner.saveEvery = 0

	runner.dirty = true
	runner.save()

	assert.NotContains(t, logs.String(), `"level":"error"`)
	assert.NotContains(t, logs.String(), `"level":"warn"`)
}

// unwritablePath names a cache file under a path that is a regular file, so
// the directory the save has to make cannot be made. It stands in for the
// real fault — a volume the head's uid does not own — without needing one.
func unwritablePath(t *testing.T) string {
	t.Helper()

	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	return filepath.Join(blocker, "cache.json")
}

// captureLogs points the global zerolog logger at a buffer for one test. The
// runner reports through the package-level logger, which is where an operator
// reads this and where a test has to.
//
// The global LEVEL is lowered with it: something in this package's test set
// raises it, and a Warn the filter swallowed reads exactly like a Warn that
// was never made.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer

	previous := log.Logger
	previousLevel := zerolog.GlobalLevel()

	log.Logger = zerolog.New(&buf)
	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	t.Cleanup(func() {
		log.Logger = previous
		zerolog.SetGlobalLevel(previousLevel)
	})

	return &buf
}
