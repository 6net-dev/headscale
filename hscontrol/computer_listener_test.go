package hscontrol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/juanfont/headscale/hscontrol/mapper"
	"github.com/juanfont/headscale/hscontrol/registry"
	"github.com/juanfont/headscale/hscontrol/state"
	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// gentility answers the two calls the computer listener's check rests on.
type gentility struct {
	mu            sync.Mutex
	computers     map[string]bool
	down          bool
	notAComputer  bool
	registrations []registry.Registration
}

func (g *gentility) set(change func(*gentility)) {
	g.mu.Lock()
	defer g.mu.Unlock()

	change(g)
}

func (g *gentility) registered() []registry.Registration {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.registrations
}

func (g *gentility) world(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()

	id := strings.TrimPrefix(r.URL.Path, "/overlay/head/world/")

	computer, ok := g.computers[id]

	switch {
	case g.down:
		w.WriteHeader(http.StatusBadGateway)
	case !ok:
		w.WriteHeader(http.StatusConflict)
	default:
		_ = json.NewEncoder(w).Encode(registry.World{
			RendererVersion: registry.RendererVersion1,
			HeadNodeID:      id,
			Addresses:       []string{"fd00::" + id},
			Computer:        computer,
		})
	}
}

func (g *gentility) register(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()

	var reg registry.Registration

	err := json.NewDecoder(r.Body).Decode(&reg)
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)

		return
	}

	g.registrations = append(g.registrations, reg)

	if reg.ComputerListener && g.notAComputer {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"error":"not_a_computer"}`)

		return
	}

	fmt.Fprint(w, `{"node_id":"node-1","world":null}`)
}

func registryApp(t *testing.T) (*Headscale, *gentility) {
	t.Helper()

	fake := &gentility{computers: map[string]bool{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/overlay/head/world/", fake.world)
	mux.HandleFunc("/overlay/head/register", fake.register)
	mux.HandleFunc("/overlay/head/rotate", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	dir := t.TempDir()
	cfg := types.Config{
		ServerURL:           "http://localhost:8080",
		NoisePrivateKeyPath: dir + "/noise_private.key",
		RegistryURL:         server.URL,
		RegistryBearer:      "test-bearer",
		Database: types.DatabaseConfig{
			Type:   "sqlite3",
			Sqlite: types.SqliteConfig{Path: dir + "/headscale_test.db"},
		},
		Policy: types.PolicyConfig{Mode: types.PolicyModeDB},
		Tuning: types.Tuning{
			BatchChangeDelay:               100 * time.Millisecond,
			BatcherWorkers:                 1,
			NodeMapSessionBufferedChanSize: 30,
		},
	}

	app, err := NewHeadscale(&cfg)
	require.NoError(t, err)

	app.mapBatcher = mapper.NewBatcherAndMapper(&cfg, app.state)
	app.mapBatcher.Start()
	t.Cleanup(app.mapBatcher.Close)

	app.state.SetRegistryHook(state.NewRegistryHook(
		registry.New(server.URL, "test-bearer"), app.mapBatcher.SeedWorld,
	))

	return app, fake
}

// enrolled is a node that registered on another listener, and the keys to
// speak as it.
type enrolled struct {
	id      types.NodeID
	user    *types.User
	machine key.MachinePublic
	nodeKey key.NodePublic
}

func enrol(t *testing.T, app *Headscale, name string) enrolled {
	t.Helper()

	e := enrolled{
		user:    app.state.CreateUserForTest(name),
		machine: key.NewMachine().Public(),
		nodeKey: key.NewNode().Public(),
	}

	resp := post(t, app, e.machine, false, "/machine/register", registerWith(authKey(t, app, e.user), e.nodeKey, "enrolled"))
	require.Equal(t, http.StatusOK, resp.Code)

	node, ok := app.state.GetNodeByNodeKey(e.nodeKey)
	require.True(t, ok)

	e.id = node.ID()

	return e
}

func authKey(t *testing.T, app *Headscale, user *types.User) string {
	t.Helper()

	pak, err := app.state.CreatePreAuthKey(user.TypedID(), true, false, nil, nil)
	require.NoError(t, err)

	return pak.Key
}

func registerWith(authKey string, nodeKey key.NodePublic, hostname string) tailcfg.RegisterRequest {
	req := tailcfg.RegisterRequest{
		Version:  tailcfg.CurrentCapabilityVersion,
		NodeKey:  nodeKey,
		Hostinfo: &tailcfg.Hostinfo{Hostname: hostname},
	}
	if authKey != "" {
		req.Auth = &tailcfg.RegisterResponseAuth{AuthKey: authKey}
	}

	return req
}

func post(t *testing.T, app *Headscale, machine key.MachinePublic, marked bool, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	payload, err := json.Marshal(body)
	require.NoError(t, err)

	return serveNoise(t, app, machine, marked, http.MethodPost, path, payload)
}

func serveNoise(t *testing.T, app *Headscale, machine key.MachinePublic, marked bool, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()

	ns := &noiseServer{headscale: app, machineKey: machine, computerListener: marked}
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), method, path, bytes.NewReader(body))
	ns.router("192.0.2.1:1234").ServeHTTP(rec, req)

	return rec
}

func registerError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	require.Equal(t, http.StatusOK, rec.Code)

	var resp tailcfg.RegisterResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	return resp.Error
}

func requireRefused(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "close", rec.Header().Get("Connection"))
}

// requireUntouched says nothing a refused request carried was stored.
func requireUntouched(t *testing.T, app *Headscale, e enrolled) {
	t.Helper()

	current, ok := app.state.GetNodeByID(e.id)
	require.True(t, ok)
	assert.Equal(t, e.nodeKey, current.NodeKey())
	assert.Equal(t, "enrolled", current.Hostinfo().Hostname())
	assert.Empty(t, current.Endpoints().AsSlice())
	assert.False(t, current.IsExpired())

	stored, err := app.state.DB().GetNodeByID(e.id)
	require.NoError(t, err)
	assert.Equal(t, e.nodeKey, stored.NodeKey)
	assert.Equal(t, "enrolled", stored.Hostinfo.Hostname)
	assert.Empty(t, stored.Endpoints)
}

type route struct{ method, path string }

// everyRoute is what the Noise router serves, read from the router, so a route
// added later is in every test below.
func everyRoute(t *testing.T, app *Headscale, node types.NodeID) []route {
	t.Helper()

	var routes []route

	id := strconv.FormatUint(node.Uint64(), 10)
	params := strings.NewReplacer("{src_node_id}", id, "{dst_node_id}", id)

	err := chi.Walk((&noiseServer{headscale: app}).router(""),
		func(method, path string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			routes = append(routes, route{method, params.Replace(path)})

			return nil
		})
	require.NoError(t, err)
	require.Contains(t, routes, route{http.MethodPost, "/machine/map"})
	require.Contains(t, routes, route{http.MethodPost, "/machine/register"})
	require.Greater(t, len(routes), 10)

	// And one the router does not serve.
	return append(routes, route{http.MethodPost, "/machine/no-such-route"})
}

func TestComputerListenerRefusesEveryRouteToANodeThatIsNotAComputers(t *testing.T) {
	app, fake := registryApp(t)
	e := enrol(t, app, "by-hand")

	computers := func(answer bool) func(*gentility) {
		return func(g *gentility) { g.computers[registry.NodeID(e.id.Uint64())] = answer }
	}

	for _, tt := range []struct {
		name string
		set  func(*gentility)
	}{
		{"its World does not say computer", computers(false)},
		{"it has no World", func(g *gentility) { clear(g.computers) }},
		{"the registry is down and none is cached", func(g *gentility) { g.down = true }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake.set(tt.set)
			app.mapBatcher.InvalidateWorld(e.id)

			for _, route := range everyRoute(t, app, e.id) {
				t.Run(route.method+route.path, func(t *testing.T) {
					requireRefused(t, serveNoise(t, app, e.machine, true, route.method, route.path, []byte(`{}`)))
				})
			}

			requireUntouched(t, app, e)
		})
	}
}

// The control for the test above: what is refused when marked is refused by
// the mark and by nothing else.
func TestComputerListenerRefusesNothingItShouldNot(t *testing.T) {
	app, fake := registryApp(t)
	byHand := enrol(t, app, "by-hand")
	computer := enrol(t, app, "computer")

	fake.set(func(g *gentility) {
		g.computers[registry.NodeID(byHand.id.Uint64())] = false
		g.computers[registry.NodeID(computer.id.Uint64())] = true
	})

	for _, tt := range []struct {
		name   string
		node   enrolled
		marked bool
	}{
		{"unmarked", byHand, false},
		{"a computer's own", computer, true},
		{"a computer's own, unmarked", computer, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, route := range everyRoute(t, app, tt.node.id) {
				rec := serveNoise(t, app, tt.node.machine, tt.marked, route.method, route.path, []byte(`{}`))
				assert.NotEqual(t, http.StatusForbidden, rec.Code, route)
			}
		})
	}
}

func TestComputerListenerStoresNothingFromANodeThatIsNotAComputers(t *testing.T) {
	app, fake := registryApp(t)
	e := enrol(t, app, "by-hand")
	other := app.state.CreateUserForTest("other")

	fake.set(func(g *gentility) { g.computers[registry.NodeID(e.id.Uint64())] = false })

	rotated := key.NewNode().Public()

	loggedOut := registerWith("", e.nodeKey, "changed")
	loggedOut.Expiry = time.Now().Add(-time.Hour)

	for _, tt := range []struct {
		name string
		path string
		body any
	}{
		{"map", "/machine/map", tailcfg.MapRequest{
			Version:  tailcfg.CurrentCapabilityVersion,
			NodeKey:  e.nodeKey,
			Hostinfo: &tailcfg.Hostinfo{Hostname: "changed"},
		}},
		{"restart", "/machine/register", registerWith("", e.nodeKey, "changed")},
		{"logout", "/machine/register", loggedOut},
		{"rotation", "/machine/register", registerWith(authKey(t, app, e.user), rotated, "changed")},
		{"another user's key", "/machine/register", registerWith(authKey(t, app, other), rotated, "changed")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			registrations := len(fake.registered())

			requireRefused(t, post(t, app, e.machine, true, tt.path, tt.body))
			requireUntouched(t, app, e)

			assert.Len(t, app.state.GetNodesByMachineKeyAllUsers(e.machine), 1)
			assert.Len(t, fake.registered(), registrations, "refused before the callback")
		})
	}

	t.Run("the same rotation unmarked is recorded", func(t *testing.T) {
		rec := post(t, app, e.machine, false, "/machine/register", registerWith(authKey(t, app, e.user), rotated, "changed"))
		require.Empty(t, registerError(t, rec))

		current, ok := app.state.GetNodeByID(e.id)
		require.True(t, ok)
		assert.Equal(t, rotated, current.NodeKey())
	})
}

// A machine with one computer's node and one that is not is refused: the
// session proves the machine, and a handler may act on either node.
func TestComputerListenerRefusesAMachineWithAnyOtherNode(t *testing.T) {
	app, fake := registryApp(t)
	e := enrol(t, app, "computer")

	rec := post(t, app, e.machine, false, "/machine/register",
		registerWith(authKey(t, app, app.state.CreateUserForTest("other")), key.NewNode().Public(), "second"))
	require.Empty(t, registerError(t, rec))
	require.Len(t, app.state.GetNodesByMachineKeyAllUsers(e.machine), 2)

	fake.set(func(g *gentility) { g.computers[registry.NodeID(e.id.Uint64())] = true })

	requireRefused(t, post(t, app, e.machine, true, "/machine/register", registerWith("", e.nodeKey, "changed")))
	requireUntouched(t, app, e)
}

func TestComputerListenerWithNoNode(t *testing.T) {
	app, fake := registryApp(t)
	user := app.state.CreateUserForTest("new")

	t.Run("every route but a registration with an authkey is refused", func(t *testing.T) {
		withKey, err := json.Marshal(registerWith(authKey(t, app, user), key.NewNode().Public(), "new"))
		require.NoError(t, err)

		for _, route := range everyRoute(t, app, 1) {
			if route.path != "/machine/register" {
				requireRefused(t, serveNoise(t, app, key.NewMachine().Public(), true, route.method, route.path, withKey))
			}
		}

		for _, body := range []string{``, `{}`, `not json`} {
			requireRefused(t, serveNoise(t, app, key.NewMachine().Public(), true, http.MethodPost, "/machine/register", []byte(body)))
		}

		assert.Empty(t, fake.registered())
	})

	// Neither is answered by the callback, and each keeps the Hostinfo it
	// carries in the cache of registrations waiting to be approved.
	t.Run("an interactive registration is refused", func(t *testing.T) {
		requireRefused(t, post(t, app, key.NewMachine().Public(), true, "/machine/register",
			registerWith("", key.NewNode().Public(), "interactive")))
	})

	t.Run("a followup with an authkey is refused", func(t *testing.T) {
		req := registerWith(authKey(t, app, user), key.NewNode().Public(), "followup")
		req.Followup = "http://localhost:8080/register/" + types.MustAuthID().String()

		requireRefused(t, post(t, app, key.NewMachine().Public(), true, "/machine/register", req))
	})

	t.Run("a key that is not a computer's is rolled back and says so", func(t *testing.T) {
		fake.set(func(g *gentility) { g.notAComputer = true })
		defer fake.set(func(g *gentility) { g.notAComputer = false })

		machine, nodeKey := key.NewMachine().Public(), key.NewNode().Public()
		rec := post(t, app, machine, true, "/machine/register", registerWith(authKey(t, app, user), nodeKey, "refused"))

		assert.Equal(t, "not a computer's authkey", registerError(t, rec))
		assert.Empty(t, app.state.GetNodesByMachineKeyAllUsers(machine))

		nodes, err := app.state.DB().ListNodes()
		require.NoError(t, err)
		assert.Empty(t, nodes)
	})

	t.Run("a computer's key is registered", func(t *testing.T) {
		machine, nodeKey := key.NewMachine().Public(), key.NewNode().Public()
		rec := post(t, app, machine, true, "/machine/register", registerWith(authKey(t, app, user), nodeKey, "computer"))

		require.Empty(t, registerError(t, rec))
		assert.Len(t, app.state.GetNodesByMachineKeyAllUsers(machine), 1)

		registrations := fake.registered()
		last := registrations[len(registrations)-1]
		assert.True(t, last.ComputerListener)
		assert.Equal(t, nodeKey.String(), last.NodeKey)
	})
}

func TestFromComputerListenerReadsPresence(t *testing.T) {
	for _, tt := range []struct {
		name   string
		values []string
		want   bool
	}{
		{"absent", nil, false},
		{"as the proxy sets it", []string{"1"}, true},
		{"empty", []string{""}, true},
		{"any other value", []string{"0"}, true},
		{"repeated", []string{"", "1"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/ts2021", nil)
			for _, value := range tt.values {
				req.Header.Add("x-gentility-computer-listener", value)
			}

			assert.Equal(t, tt.want, fromComputerListener(req))
		})
	}
}
