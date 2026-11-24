package hscontrol

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juanfont/headscale/hscontrol/registry"
	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/envknob"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

const joinTimeout = 30 * time.Second

// listeners is one head behind two listeners, the way a proxy serves it: one
// marks every request and the other strips the mark.
func listeners(t *testing.T, app *Headscale) (string, string) {
	t.Helper()

	handler := app.HTTPHandler()

	serve := func(mark func(http.Header)) string {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mark(r.Header)
			handler.ServeHTTP(w, r)
		}))
		t.Cleanup(server.Close)

		return server.URL
	}

	marked := serve(func(h http.Header) { h.Set(computerListenerHeader, "1") })
	unmarked := serve(func(h http.Header) { h.Del(computerListenerHeader) })

	return marked, unmarked
}

// client is a real client on a first join, as a computer's connector makes one.
func client(t *testing.T, controlURL, authKey string) *tsnet.Server {
	t.Helper()

	// Nothing here may leave the machine.
	envknob.SetNoLogsNoSupport()

	c := &tsnet.Server{
		Dir:        t.TempDir(),
		ControlURL: controlURL,
		AuthKey:    authKey,
		Hostname:   "computer",
		Logf:       t.Logf,
		UserLogf:   t.Logf,
	}
	t.Cleanup(func() { _ = c.Close() })

	return c
}

// join waits less than the client is prepared to, so a client left retrying
// fails the test with the deadline rather than the refusal.
func join(t *testing.T, c *tsnet.Server) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), joinTimeout)
	defer cancel()

	_, err := c.Up(ctx)

	return err
}

func realClientApp(t *testing.T) (*Headscale, *gentility) {
	t.Helper()

	app, fake := registryApp(t)
	app.state.SetDERPMap(&tailcfg.DERPMap{
		Regions: map[int]*tailcfg.DERPRegion{
			900: {RegionID: 900, RegionCode: "test", Nodes: []*tailcfg.DERPNode{{
				Name: "test0", RegionID: 900, HostName: "127.0.0.1", IPv4: "127.0.0.1", DERPPort: -1,
			}}},
		},
	})

	return app, fake
}

func TestRealClientHearsNotAComputerAsAFailedRegistration(t *testing.T) {
	app, fake := realClientApp(t)
	marked, _ := listeners(t, app)

	fake.set(func(g *gentility) { g.notAComputer = true })

	err := join(t, client(t, marked, authKey(t, app, app.state.CreateUserForTest("guest"))))
	require.ErrorContains(t, err, "not a computer's authkey")
	require.NotErrorIs(t, err, context.DeadlineExceeded)

	nodes, err := app.state.DB().ListNodes()
	require.NoError(t, err)
	assert.Empty(t, nodes, "the refused registration left a node behind")

	registrations := fake.registered()
	require.NotEmpty(t, registrations)
	assert.True(t, registrations[0].ComputerListener)
}

func TestRealClientJoinsAsAComputer(t *testing.T) {
	app, fake := realClientApp(t)
	marked, _ := listeners(t, app)

	// The node's id is the first the head allocates.
	fake.set(func(g *gentility) { g.computers[registry.NodeID(1)] = true })

	require.NoError(t, join(t, client(t, marked, authKey(t, app, app.state.CreateUserForTest("guest")))))

	node, ok := app.state.GetNodeByID(types.NodeID(1))
	require.True(t, ok)
	assert.Equal(t, "computer", node.Hostinfo().Hostname())
}

// The control: a key that is not a computer's is refused by the mark alone.
func TestRealClientJoinsUnmarked(t *testing.T) {
	app, fake := realClientApp(t)
	_, unmarked := listeners(t, app)

	fake.set(func(g *gentility) {
		g.notAComputer = true
		g.computers[registry.NodeID(1)] = false
	})

	require.NoError(t, join(t, client(t, unmarked, authKey(t, app, app.state.CreateUserForTest("laptop")))))

	registrations := fake.registered()
	require.Len(t, registrations, 1)
	assert.False(t, registrations[0].ComputerListener)
}

// A node enrolled elsewhere and carried in is never mapped, and keeps the
// Hostinfo it had: the client is the same one, moved to the marked listener.
func TestRealClientCarriedInIsNotMapped(t *testing.T) {
	app, fake := realClientApp(t)
	marked, unmarked := listeners(t, app)

	fake.set(func(g *gentility) { g.computers[registry.NodeID(1)] = false })

	outside := client(t, unmarked, authKey(t, app, app.state.CreateUserForTest("laptop")))
	require.NoError(t, join(t, outside))
	require.NoError(t, outside.Close())

	before, ok := app.state.GetNodeByID(types.NodeID(1))
	require.True(t, ok)

	carried := client(t, marked, "")
	carried.Dir = outside.Dir
	carried.Hostname = "carried"

	refused := make(chan struct{}, 1)
	carried.Logf = func(format string, args ...any) {
		if strings.Contains(fmt.Sprintf(format, args...), "http 403: not a computer's node") {
			select {
			case refused <- struct{}{}:
			default:
			}
		}
	}

	// It retries for as long as it is let: what it is told is not an answer
	// about a registration.
	require.NoError(t, carried.Start())

	for range 2 {
		select {
		case <-refused:
		case <-time.After(joinTimeout):
			t.Fatal("the carried-in client was not refused")
		}
	}

	after, ok := app.state.GetNodeByID(types.NodeID(1))
	require.True(t, ok)
	assert.Equal(t, before.Hostinfo().Hostname(), after.Hostinfo().Hostname())
	assert.Equal(t, before.NodeKey(), after.NodeKey())
	assert.Len(t, app.state.ListNodes().AsSlice(), 1)
}
