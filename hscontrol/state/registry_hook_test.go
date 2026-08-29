package state

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/juanfont/headscale/hscontrol/registry"
	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/juanfont/headscale/hscontrol/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// fakeRegistry is a Gentility that answers the two callbacks however the test
// tells it to, and records what it was asked. It is an httptest server rather
// than a stub of [RegistryHook] because the payload's exact fields are half of
// what these tests are for, and a stub would let a wrong field pass.
type fakeRegistry struct {
	server *httptest.Server

	// status and body are what /register answers; rotateStatus is what
	// /rotate answers.
	status       int
	body         string
	rotateStatus int

	// duringRotate runs on the server goroutine while the head is blocked
	// in /rotate — that is, inside its open write transaction and before
	// the commit. It is the only vantage point from which what the head
	// has already published can be seen.
	duringRotate func()

	mu            sync.Mutex
	registrations []registry.Registration
	rotations     []registry.Rotation
	bearers       []string
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()

	f := &fakeRegistry{
		status:       http.StatusOK,
		body:         `{"node_id":"node-1","world":null}`,
		rotateStatus: http.StatusNoContent,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/overlay/head/register", func(w http.ResponseWriter, r *http.Request) {
		var reg registry.Registration
		require.NoError(t, json.NewDecoder(r.Body).Decode(&reg))

		f.mu.Lock()
		f.registrations = append(f.registrations, reg)
		f.bearers = append(f.bearers, r.Header.Get("Authorization"))
		status, body := f.status, f.body
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/overlay/head/rotate", func(w http.ResponseWriter, r *http.Request) {
		var rot registry.Rotation
		require.NoError(t, json.NewDecoder(r.Body).Decode(&rot))

		f.mu.Lock()
		f.rotations = append(f.rotations, rot)
		status := f.rotateStatus
		observe := f.duringRotate
		f.mu.Unlock()

		if observe != nil {
			observe()
		}

		w.WriteHeader(status)
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)

	return f
}

func (f *fakeRegistry) answers(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.status, f.body = status, body
}

func (f *fakeRegistry) refusesRotation(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.rotateStatus = status
}

func (f *fakeRegistry) recorded() ([]registry.Registration, []registry.Rotation) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.registrations, f.rotations
}

// seededWorlds records what the hook handed the World cache.
type seededWorlds struct {
	mu     sync.Mutex
	worlds map[types.NodeID]*registry.World
}

func (s *seededWorlds) seed(id types.NodeID, world *registry.World) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.worlds == nil {
		s.worlds = map[types.NodeID]*registry.World{}
	}

	s.worlds[id] = world
}

func (s *seededWorlds) get(id types.NodeID) *registry.World {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.worlds[id]
}

// registryTestState is a State wired to fake, with a reusable user-owned
// pre-auth key so a test can register the same machine more than once.
func registryTestState(t *testing.T, fake *fakeRegistry) (*State, *types.PreAuthKeyNew, *seededWorlds) {
	t.Helper()

	cfg := persistTestConfig(t.TempDir() + "/headscale.db")

	s, err := NewState(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	seeded := &seededWorlds{}

	if fake != nil {
		s.SetRegistryHook(NewRegistryHook(
			registry.New(fake.server.URL, "test-bearer"),
			seeded.seed,
		))
	}

	user := s.CreateUserForTest("registry-user")

	pak, err := s.CreatePreAuthKey(user.TypedID(), true, false, nil, nil)
	require.NoError(t, err)

	return s, pak, seeded
}

func registerRequest(pak *types.PreAuthKeyNew, nodeKey key.NodePublic) tailcfg.RegisterRequest {
	return tailcfg.RegisterRequest{
		Auth:     &tailcfg.RegisterResponseAuth{AuthKey: pak.Key},
		NodeKey:  nodeKey,
		Hostinfo: &tailcfg.Hostinfo{Hostname: "registry-node"},
		Expiry:   time.Now().Add(24 * time.Hour),
	}
}

// TestRegistryRegisterCarriesTheBinding is the happy path: the node is
// persisted, and the callback named the pre-auth key id, both keys and the id
// the head had just allocated.
func TestRegistryRegisterCarriesTheBinding(t *testing.T) {
	fake := newFakeRegistry(t)
	s, pak, _ := registryTestState(t, fake)

	machineKey := key.NewMachine()
	nodeKey := key.NewNode()

	node, _, err := s.HandleNodeFromPreAuthKey(registerRequest(pak, nodeKey.Public()), machineKey.Public())
	require.NoError(t, err)
	require.True(t, node.Valid())

	stored, err := s.DB().GetNodeByID(node.ID())
	require.NoError(t, err, "an accepted registration must be committed")
	require.NotNil(t, stored)

	registrations, _ := fake.recorded()
	require.Len(t, registrations, 1)
	assert.Equal(t, registry.Registration{
		PreauthKeyID: strconv.FormatUint(pak.ID, 10),
		NodeKey:      nodeKey.Public().String(),
		MachineKey:   machineKey.Public().String(),
		HeadNodeID:   registry.NodeID(node.ID().Uint64()),
	}, registrations[0])

	assert.Equal(t, []string{"Bearer test-bearer"}, fake.bearers)
}

// TestRegistryRegisterSeedsTheWorld: a World that comes back with the
// registration is the document the head serves that node next, so it goes
// straight into the cache rather than being fetched back a moment later.
func TestRegistryRegisterSeedsTheWorld(t *testing.T) {
	fake := newFakeRegistry(t)
	fake.answers(http.StatusOK, `{"node_id":"node-1","world":{"renderer_version":1,"revision":7,"head_node_id":"1","addresses":["fd00::1"]}}`)

	s, pak, seeded := registryTestState(t, fake)

	node, _, err := s.HandleNodeFromPreAuthKey(
		registerRequest(pak, key.NewNode().Public()),
		key.NewMachine().Public(),
	)
	require.NoError(t, err)

	world := seeded.get(node.ID())
	require.NotNil(t, world, "a non-nil world must be seeded into the cache")
	assert.Equal(t, int64(7), world.Revision)
}

// TestRegistryRegisterNilWorldSeedsNothing: a first registration whose render
// is still queued answers world: null, and that is not a document.
func TestRegistryRegisterNilWorldSeedsNothing(t *testing.T) {
	fake := newFakeRegistry(t)
	s, pak, seeded := registryTestState(t, fake)

	node, _, err := s.HandleNodeFromPreAuthKey(
		registerRequest(pak, key.NewNode().Public()),
		key.NewMachine().Public(),
	)
	require.NoError(t, err)

	assert.Nil(t, seeded.get(node.ID()))
}

// TestRegistryRefusalRollsTheNodeBack covers every refusal in §2.3's register
// column. The head must not commit a node Gentility has not recorded, so the
// row never existed and the client is told why.
func TestRegistryRefusalRollsTheNodeBack(t *testing.T) {
	for name, tc := range map[string]struct {
		status  int
		message string
	}{
		"unknown key": {http.StatusNotFound, "invalid authkey"},
		"expired":     {http.StatusGone, "authkey expired"},
		"refused":     {http.StatusConflict, "registration refused by the registry"},
		"compromised": {http.StatusForbidden, "authkey is bound to a different machine"},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeRegistry(t)
			fake.answers(tc.status, `{}`)

			s, pak, _ := registryTestState(t, fake)

			_, _, err := s.HandleNodeFromPreAuthKey(
				registerRequest(pak, key.NewNode().Public()),
				key.NewMachine().Public(),
			)
			require.Error(t, err)

			// A refusal is final, so it reaches the client as the 401
			// every other bad pre-auth key earns rather than as a 500
			// it would retry forever.
			pakErr, ok := errors.AsType[types.PAKError](err)
			require.True(t, ok, "a refusal must surface as a types.PAKError, got %v", err)
			assert.Equal(t, tc.message, pakErr.Error())

			assertNoNodes(t, s)
		})
	}
}

// TestRegistryTransientErrorRollsTheNodeBack: a Gentility that is down has not
// recorded the registration either, so the write rolls back exactly as a
// refusal does. It is NOT a PAKError: the client should retry, and register is
// idempotent by content so a retry after a lost response is safe.
func TestRegistryTransientErrorRollsTheNodeBack(t *testing.T) {
	fake := newFakeRegistry(t)
	fake.answers(http.StatusInternalServerError, `{}`)

	s, pak, _ := registryTestState(t, fake)

	_, _, err := s.HandleNodeFromPreAuthKey(
		registerRequest(pak, key.NewNode().Public()),
		key.NewMachine().Public(),
	)
	require.Error(t, err)

	_, isPAK := errors.AsType[types.PAKError](err)
	assert.False(t, isPAK, "a transient failure must not be reported as a bad pre-auth key")

	assertNoNodes(t, s)
}

// TestRegistryRetryReusesTheHeadNodeID: the re-registration branch finds the
// node by machine key and never allocates a new id, so a client that registers
// twice keeps the id Gentility was told about — and is not reported twice.
func TestRegistryRetryReusesTheHeadNodeID(t *testing.T) {
	fake := newFakeRegistry(t)
	s, pak, _ := registryTestState(t, fake)

	machineKey := key.NewMachine()
	nodeKey := key.NewNode()
	req := registerRequest(pak, nodeKey.Public())

	first, _, err := s.HandleNodeFromPreAuthKey(req, machineKey.Public())
	require.NoError(t, err)

	second, _, err := s.HandleNodeFromPreAuthKey(req, machineKey.Public())
	require.NoError(t, err)

	assert.Equal(t, first.ID(), second.ID())

	registrations, _ := fake.recorded()
	assert.Len(t, registrations, 1, "an unchanged re-registration allocates no id and reports nothing")
}

// TestRegistryAbsentLeavesRegistrationUnchanged: without registry.url there is
// no hook, and the head is upstream headscale.
func TestRegistryAbsentLeavesRegistrationUnchanged(t *testing.T) {
	s, pak, _ := registryTestState(t, nil)

	node, _, err := s.HandleNodeFromPreAuthKey(
		registerRequest(pak, key.NewNode().Public()),
		key.NewMachine().Public(),
	)
	require.NoError(t, err)
	require.True(t, node.Valid())
}

// assertNoNodes fails if any node row survived — the whole point of doing the
// callback inside the transaction.
func assertNoNodes(t *testing.T, s *State) {
	t.Helper()

	nodes, err := s.db.ListNodes()
	require.NoError(t, err)
	assert.Empty(t, nodes, "a refused registration must leave no node row")

	assert.Equal(t, 0, s.nodeStore.ListNodes().Len(), "a refused registration must leave nothing in the NodeStore")
}

// TestRegistryRotationRecordedBeforeCommit: a node that comes back with a new
// node key is a rotation, and the head does not commit the new key until
// Gentility has been told which key replaced which (design §4 channel 4).
func TestRegistryRotationRecordedBeforeCommit(t *testing.T) {
	fake := newFakeRegistry(t)
	s, pak, _ := registryTestState(t, fake)

	machineKey := key.NewMachine()
	oldKey := key.NewNode()
	newKey := key.NewNode()

	node, _, err := s.HandleNodeFromPreAuthKey(registerRequest(pak, oldKey.Public()), machineKey.Public())
	require.NoError(t, err)

	rotated, _, err := s.HandleNodeFromPreAuthKey(registerRequest(pak, newKey.Public()), machineKey.Public())
	require.NoError(t, err)
	require.Equal(t, node.ID(), rotated.ID(), "a rotation moves a key, not a node")

	_, rotations := fake.recorded()
	require.Len(t, rotations, 1)
	assert.Equal(t, registry.Rotation{
		HeadNodeID: registry.NodeID(node.ID().Uint64()),
		OldKey:     oldKey.Public().String(),
		NewKey:     newKey.Public().String(),
	}, rotations[0])

	assertNodeKey(t, s, node.ID(), newKey.Public())
}

// TestRegistryRotationRefusalKeepsTheOldKey: a rotation the record
// contradicts is refused, and the head is left exactly as it was — the row,
// and the NodeStore snapshot the poll answers from.
func TestRegistryRotationRefusalKeepsTheOldKey(t *testing.T) {
	for name, tc := range map[string]struct {
		status  int
		isPAK   bool
		message string
	}{
		"refused":      {http.StatusConflict, true, "node key rotation refused by the registry"},
		"unknown node": {http.StatusNotFound, true, "the registry does not know this node"},
		"outage":       {http.StatusInternalServerError, false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeRegistry(t)
			s, pak, _ := registryTestState(t, fake)

			machineKey := key.NewMachine()
			oldKey := key.NewNode()

			node, _, err := s.HandleNodeFromPreAuthKey(registerRequest(pak, oldKey.Public()), machineKey.Public())
			require.NoError(t, err)

			fake.refusesRotation(tc.status)

			_, _, err = s.HandleNodeFromPreAuthKey(
				registerRequest(pak, key.NewNode().Public()),
				machineKey.Public(),
			)
			require.Error(t, err)

			pakErr, isPAK := errors.AsType[types.PAKError](err)
			assert.Equal(t, tc.isPAK, isPAK, "got %v", err)

			if tc.isPAK {
				assert.Equal(t, tc.message, pakErr.Error())
			}

			assertNodeKey(t, s, node.ID(), oldKey.Public())
		})
	}
}

// TestRegistryRotationIdempotentRetry: the same rotation offered twice is
// recorded twice and refused neither time — Gentility is idempotent on the new
// key, which is what makes a retry after a lost response safe.
func TestRegistryRotationIdempotentRetry(t *testing.T) {
	fake := newFakeRegistry(t)
	s, pak, _ := registryTestState(t, fake)

	machineKey := key.NewMachine()
	oldKey := key.NewNode()
	newKey := key.NewNode()

	_, _, err := s.HandleNodeFromPreAuthKey(registerRequest(pak, oldKey.Public()), machineKey.Public())
	require.NoError(t, err)

	fake.refusesRotation(http.StatusInternalServerError)

	_, _, err = s.HandleNodeFromPreAuthKey(registerRequest(pak, newKey.Public()), machineKey.Public())
	require.Error(t, err)

	fake.refusesRotation(http.StatusNoContent)

	node, _, err := s.HandleNodeFromPreAuthKey(registerRequest(pak, newKey.Public()), machineKey.Public())
	require.NoError(t, err)

	_, rotations := fake.recorded()
	require.Len(t, rotations, 2)
	assert.Equal(t, rotations[0], rotations[1], "the retry is the same rotation, old key and all")

	assertNodeKey(t, s, node.ID(), newKey.Public())
}

// assertNodeKey checks the key the head would serve and the key it would
// reload, which a rollback has to leave agreeing.
func assertNodeKey(t *testing.T, s *State, id types.NodeID, want key.NodePublic) {
	t.Helper()

	stored, err := s.DB().GetNodeByID(id)
	require.NoError(t, err)
	assert.Equal(t, want, stored.NodeKey, "the row must hold this key")

	cached, ok := s.GetNodeByID(id)
	require.True(t, ok)
	assert.Equal(t, want, cached.NodeKey(), "the NodeStore must hold this key")
}

// # Registry mode admits nodes by pre-auth key and by nothing else
//
// Design §4 channel 3 identifies an enrollment by the pre-auth key id
// Gentility minted through the head's own API. An interactive registration —
// gRPC `headscale nodes register`, or an OIDC callback — carries no such key,
// so there is nothing to report and the node would exist on the head and
// nowhere else: unreported, unrendered, and served no World for the whole of
// its life. The head refuses instead.

// TestRegistryRefusesGRPCRegistration: `headscale nodes register` reaches
// [State.HandleNodeFromAuthPath] with no pre-auth key (grpcv1.go:RegisterNode),
// and there is no enrollment for it to be part of.
func TestRegistryRefusesGRPCRegistration(t *testing.T) {
	fake := newFakeRegistry(t)
	s, _, _ := registryTestState(t, fake)

	user := s.CreateUserForTest("interactive-user")

	authID := authPathRequest(t, s, key.NewMachine().Public(), key.NewNode().Public())

	_, _, err := s.HandleNodeFromAuthPath(authID, types.UserID(user.ID), nil, util.RegisterMethodCLI)
	require.ErrorIs(t, err, ErrRegistryPreAuthKeyOnly)

	assertNoNodes(t, s)

	registrations, _ := fake.recorded()
	assert.Empty(t, registrations, "a refused enrollment reports nothing")
}

// TestRegistryRefusesOIDCRegistration is the same refusal on the other caller
// of [State.HandleNodeFromAuthPath] (oidc.go:handleRegistration). The method
// differs; the missing pre-auth key does not.
func TestRegistryRefusesOIDCRegistration(t *testing.T) {
	fake := newFakeRegistry(t)
	s, _, _ := registryTestState(t, fake)

	user := s.CreateUserForTest("oidc-user")

	authID := authPathRequest(t, s, key.NewMachine().Public(), key.NewNode().Public())

	_, _, err := s.HandleNodeFromAuthPath(authID, types.UserID(user.ID), nil, util.RegisterMethodOIDC)
	require.ErrorIs(t, err, ErrRegistryPreAuthKeyOnly)

	assertNoNodes(t, s)
}

// TestRegistryAllowsInteractiveRegistrationWithoutARegistry: the refusal is
// registry mode's and not headscale's. A head with no registry.url is upstream
// headscale, and `headscale nodes register` works exactly as it always did.
func TestRegistryAllowsInteractiveRegistrationWithoutARegistry(t *testing.T) {
	s, _, _ := registryTestState(t, nil)

	user := s.CreateUserForTest("interactive-user")

	authID := authPathRequest(t, s, key.NewMachine().Public(), key.NewNode().Public())

	node, _, err := s.HandleNodeFromAuthPath(authID, types.UserID(user.ID), nil, util.RegisterMethodCLI)
	require.NoError(t, err)
	assert.True(t, node.Valid())
}

// TestRegistryRefusesInteractiveKeyRotation: a node enrolled by pre-auth key
// can still reach [State.applyAuthNodeUpdate] through the same two callers,
// and the client-supplied node key there is a rotation. That path holds no
// pre-auth key either, so a head that let it through would commit a key
// Gentility could not attribute — the one thing channel 4 exists to stop.
func TestRegistryRefusesInteractiveKeyRotation(t *testing.T) {
	fake := newFakeRegistry(t)
	s, pak, _ := registryTestState(t, fake)

	machineKey := key.NewMachine()
	oldKey := key.NewNode()

	node, _, err := s.HandleNodeFromPreAuthKey(registerRequest(pak, oldKey.Public()), machineKey.Public())
	require.NoError(t, err)

	user, err := s.GetUserByName("registry-user")
	require.NoError(t, err)

	authID := authPathRequest(t, s, machineKey.Public(), key.NewNode().Public())

	_, _, err = s.HandleNodeFromAuthPath(authID, types.UserID(user.ID), nil, util.RegisterMethodOIDC)
	require.ErrorIs(t, err, ErrRegistryInteractiveRotation)

	assertNodeKey(t, s, node.ID(), oldKey.Public())

	_, rotations := fake.recorded()
	assert.Empty(t, rotations, "a refused rotation reports nothing")
}

// TestRegistryAllowsInteractiveReauthWithTheSameKey: only a KEY CHANGE is a
// rotation. A re-auth presenting the key the node already holds moves nothing
// Gentility records, and refusing it would break a relogin that is only
// refreshing an expiry.
func TestRegistryAllowsInteractiveReauthWithTheSameKey(t *testing.T) {
	fake := newFakeRegistry(t)
	s, pak, _ := registryTestState(t, fake)

	machineKey := key.NewMachine()
	nodeKey := key.NewNode()

	node, _, err := s.HandleNodeFromPreAuthKey(registerRequest(pak, nodeKey.Public()), machineKey.Public())
	require.NoError(t, err)

	user, err := s.GetUserByName("registry-user")
	require.NoError(t, err)

	authID := authPathRequest(t, s, machineKey.Public(), nodeKey.Public())

	again, _, err := s.HandleNodeFromAuthPath(authID, types.UserID(user.ID), nil, util.RegisterMethodOIDC)
	require.NoError(t, err)
	assert.Equal(t, node.ID(), again.ID())

	assertNodeKey(t, s, node.ID(), nodeKey.Public())
}

// authPathRequest stages the cached registration the auth callback resolves,
// which is what makes the gRPC and OIDC paths reachable from a unit test.
func authPathRequest(
	t *testing.T,
	s *State,
	machineKey key.MachinePublic,
	nodeKey key.NodePublic,
) types.AuthID {
	t.Helper()

	authID := types.MustAuthID()
	s.SetAuthCacheEntry(authID, types.NewRegisterAuthRequest(&types.RegistrationData{
		MachineKey: machineKey,
		NodeKey:    nodeKey,
		DiscoKey:   key.NewDisco().Public(),
		Hostname:   "interactive-node",
		Hostinfo:   &tailcfg.Hostinfo{Hostname: "interactive-node"},
	}))

	return authID
}

// TestRegistryRotationIsNotPublishedBeforeItIsRecorded is the ordering the
// create path has always had, on the path that did not: the database
// transaction — with the channel 4 callback inside it — commits BEFORE the
// NodeStore is told anything.
//
// The NodeStore is what a long poll is answered from, so a NodeStore updated
// first is a window in which a concurrent poll is served under a key Gentility
// has never been told about. It is short and it is real, and a rotation that
// is then refused leaves the head having published a key that never existed.
//
// The assertion runs on the registry's own goroutine while the head is blocked
// in /rotate, which is the only place the intermediate state is visible at all.
func TestRegistryRotationIsNotPublishedBeforeItIsRecorded(t *testing.T) {
	fake := newFakeRegistry(t)
	s, pak, _ := registryTestState(t, fake)

	machineKey := key.NewMachine()
	oldKey := key.NewNode()
	newKey := key.NewNode()

	node, _, err := s.HandleNodeFromPreAuthKey(registerRequest(pak, oldKey.Public()), machineKey.Public())
	require.NoError(t, err)

	var published key.NodePublic

	fake.duringRotate = func() {
		cached, ok := s.GetNodeByID(node.ID())
		require.True(t, ok)

		published = cached.NodeKey()
	}

	fake.refusesRotation(http.StatusConflict)

	_, _, err = s.HandleNodeFromPreAuthKey(registerRequest(pak, newKey.Public()), machineKey.Public())
	require.Error(t, err)

	assert.Equal(t, oldKey.Public(), published,
		"the NodeStore must still hold the old key while the rotation is being recorded")

	// And the refusal leaves the head exactly as it was, row and snapshot.
	assertNodeKey(t, s, node.ID(), oldKey.Public())
}

// TestRegistryRotationIsPublishedOnceRecorded is the other half: the commit
// happening first must not mean the NodeStore is left behind.
func TestRegistryRotationIsPublishedOnceRecorded(t *testing.T) {
	fake := newFakeRegistry(t)
	s, pak, _ := registryTestState(t, fake)

	machineKey := key.NewMachine()
	oldKey := key.NewNode()
	newKey := key.NewNode()

	node, _, err := s.HandleNodeFromPreAuthKey(registerRequest(pak, oldKey.Public()), machineKey.Public())
	require.NoError(t, err)

	rotated, _, err := s.HandleNodeFromPreAuthKey(registerRequest(pak, newKey.Public()), machineKey.Public())
	require.NoError(t, err)
	require.Equal(t, node.ID(), rotated.ID())

	assert.Equal(t, newKey.Public(), rotated.NodeKey(),
		"the view the caller is handed is the rotated one")

	assertNodeKey(t, s, node.ID(), newKey.Public())
}

// TestRegistryRetryAfterALostAcceptanceRebinds is the case the retry note
// beside [State.recordRegistration] is about, and the one it used to get
// wrong.
//
// Gentility accepts the registration and records it against the head node id
// the head has just allocated. The head's write then does NOT survive — the
// response is lost on the wire, or the commit fails after the callback
// returned — so the row is rolled back and that id never existed on the head.
//
// The retry does not resume the first attempt. There is no node to find by
// machine key, so the head allocates an id afresh and reports THAT, and
// whether it is the same number is the database's business rather than
// anything this channel promises: a rolled-back insert may or may not return
// its id to the pool, and another machine may take it in between. Both are
// covered here.
//
// Neither leaves Gentility wrong, and that is the property worth holding on
// to: register is by content, the enrollment is registered and not yet
// exchanged, so whichever node next claims an id rebinds it. Gentility's
// stale record for an id the head abandoned is corrected by the registration
// of whoever ends up holding it, and the World it answers is nil in exactly
// that case (see [registry.RegistrationResult]).
//
// A lost response produces the rollback rather than a failed commit:
// [hsdb.Write] commits after its callback returns and gorm offers no hook on
// the commit itself, so nothing reachable from a test can fail it. The two are
// the same fact from the head's side — Gentility recorded a registration the
// head did not keep — and a lost response is the likelier of them.
func TestRegistryRetryAfterALostAcceptanceRebinds(t *testing.T) {
	fake := newFakeRegistry(t)
	s, reusable, _ := registryTestState(t, fake)

	user := s.CreateUserForTest("lost-response-user")

	// One-shot, so the retry also shows the key is still spendable: the
	// rollback took [hsdb.UsePreAuthKey] with it.
	oneShot, err := s.CreatePreAuthKey(user.TypedID(), false, false, nil, nil)
	require.NoError(t, err)

	first := key.NewMachine()
	firstReq := registerRequest(oneShot, key.NewNode().Public())

	// A 200 the head cannot read: Gentility has recorded the binding, and
	// the head has nothing it can commit.
	fake.answers(http.StatusOK, `{"node_id":`)

	_, _, err = s.HandleNodeFromPreAuthKey(firstReq, first.Public())
	require.Error(t, err)

	_, isPAK := errors.AsType[types.PAKError](err)
	assert.False(t, isPAK, "a lost response is transient; the client should retry")

	assertNoNodes(t, s)

	fake.answers(http.StatusOK, `{"node_id":"node-1","world":null}`)

	// A different machine enrolls before the retry, and is free to be given
	// the very id Gentility was told about — the head abandoned it, so
	// nothing has a claim on it.
	other := key.NewMachine()

	otherNode, _, err := s.HandleNodeFromPreAuthKey(
		registerRequest(reusable, key.NewNode().Public()),
		other.Public(),
	)
	require.NoError(t, err)

	// Now the retry. The one-shot key is still spendable, and the id is
	// allocated afresh.
	retried, _, err := s.HandleNodeFromPreAuthKey(firstReq, first.Public())
	require.NoError(t, err)

	registrations, _ := fake.recorded()
	require.Len(t, registrations, 3, "the retry is a second registration, not a no-op")

	assert.NotEqual(t, registrations[0].HeadNodeID, registrations[2].HeadNodeID,
		"the retry does not resume the abandoned id")
	assert.Equal(t, registry.NodeID(retried.ID().Uint64()), registrations[2].HeadNodeID,
		"and reports the one it kept")
	assert.Equal(t, registrations[0].PreauthKeyID, registrations[2].PreauthKeyID,
		"it is the same enrollment, rebound")

	// The abandoned id went to the other machine, and that machine's own
	// registration is what corrects Gentility's record of it.
	assert.Equal(t, registrations[0].HeadNodeID, registrations[1].HeadNodeID)
	assert.Equal(t, registry.NodeID(otherNode.ID().Uint64()), registrations[1].HeadNodeID)
	assert.NotEqual(t, registrations[0].MachineKey, registrations[1].MachineKey)
}

// TestRegistryRetryAfterACommittedRegistrationReportsNothing is the other
// half of the same note: a retry after a registration the head DID keep finds
// the node by machine key, allocates nothing, and reports nothing.
func TestRegistryRetryAfterACommittedRegistrationReportsNothing(t *testing.T) {
	fake := newFakeRegistry(t)
	s, pak, _ := registryTestState(t, fake)

	machineKey := key.NewMachine()
	req := registerRequest(pak, key.NewNode().Public())

	first, _, err := s.HandleNodeFromPreAuthKey(req, machineKey.Public())
	require.NoError(t, err)

	second, _, err := s.HandleNodeFromPreAuthKey(req, machineKey.Public())
	require.NoError(t, err)

	assert.Equal(t, first.ID(), second.ID())

	registrations, _ := fake.recorded()
	assert.Len(t, registrations, 1)
}
