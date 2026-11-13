package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testBearer = "overlay-head-shared-secret-marker" //nolint:gosec // test marker, not a real credential

func TestChangePreservesRemovalAndUnknownKinds(t *testing.T) {
	for _, kind := range []string{"world", "removed", "future-kind"} {
		t.Run(kind, func(t *testing.T) {
			var event Change
			require.NoError(t, json.Unmarshal(fmt.Appendf(nil, `{"seq":12,"head_node_id":"3","world_revision":8,"kind":%q}`, kind), &event))
			encoded, err := json.Marshal(event)
			require.NoError(t, err)

			var fields map[string]any
			require.NoError(t, json.Unmarshal(encoded, &fields))
			require.Equal(t, kind, fields["kind"], "the reader must retain kind so removal or full refresh can be chosen")
		})
	}
}

// worldV1 is master §2.4's example document, verbatim. It is the fixture
// rather than a hand-rolled map because the point of this test is that the
// document Gentility renders decodes here without amendment.
const worldV1 = `{
  "renderer_version": 1,
  "revision": 17,
  "head_node_id": "3",
  "addresses": ["fd7a:115c:a1e0:1::a", "fd7a:115c:a1e0:2::a"],
  "names": [{"org": "acme", "label": "n1"}, {"org": "beta", "label": "n1"}],
  "dns": [{"name": "gentility-overlay.internal", "addr": "fd7a:115c:a1e0:1::1"},
          {"name": "gentility-overlay.internal", "addr": "fd7a:115c:a1e0:2::1"}],
  "filters": [{"src": "fd7a:115c:a1e0:1::a/128", "dst": "fd7a:115c:a1e0:1::1/128", "ports": [443]}],
  "derp_map": {"regions": {"1": {"name": "embedded", "nodes": [{"host": "derp.example", "port": 443}]}}}
}`

// cursor is a persisted cursor as [Client.Changes] takes one. A nil cursor is
// a different request — the tail — so the pointer is the distinction rather
// than a sentinel number.
func cursor(seq int64) *int64 { return &seq }

// serve starts a test Gentility and returns a client pointed at it.
func serve(t *testing.T, handler http.HandlerFunc, opts ...Option) *Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return New(server.URL, testBearer, opts...)
}

func TestWorldDecodesTheV1Document(t *testing.T) {
	var gotPath, gotAuth, gotMethod string

	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotMethod = r.URL.Path, r.Header.Get("Authorization"), r.Method

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, worldV1)
	})

	world, err := client.World(t.Context(), "3")
	require.NoError(t, err)

	assert.Equal(t, "/overlay/head/world/3", gotPath)
	assert.Equal(t, "Bearer "+testBearer, gotAuth)
	assert.Equal(t, http.MethodGet, gotMethod)

	assert.Equal(t, RendererVersion1, world.RendererVersion)
	assert.Equal(t, int64(17), world.Revision)
	assert.Equal(t, "3", world.HeadNodeID)
	assert.Equal(t, []string{"fd7a:115c:a1e0:1::a", "fd7a:115c:a1e0:2::a"}, world.Addresses)
	assert.Equal(t, []Name{{Org: "acme", Label: "n1"}, {Org: "beta", Label: "n1"}}, world.Names)

	// Several records for one name is the contract, not a duplication.
	require.Len(t, world.DNS, 2)
	assert.Equal(t, world.DNS[0].Name, world.DNS[1].Name)
	assert.Equal(t, "fd7a:115c:a1e0:2::1", world.DNS[1].Addr)
	assert.Empty(t, world.DNS[0].Type, "a rendered record carries no type (design §13)")

	assert.Equal(t,
		[]Filter{{
			Src:   "fd7a:115c:a1e0:1::a/128",
			Dst:   "fd7a:115c:a1e0:1::1/128",
			Ports: []int{443},
		}},
		world.Filters)

	assert.JSONEq(t,
		`{"regions":{"1":{"name":"embedded","nodes":[{"host":"derp.example","port":443}]}}}`,
		string(world.DERPMap))
}

func TestWorldEscapesTheHeadNodeIDInThePath(t *testing.T) {
	var gotPath string

	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusNotFound)
	})

	_, err := client.World(t.Context(), "3/../7")
	require.ErrorIs(t, err, ErrUnknownNode)
	assert.Equal(t, "/overlay/head/world/3%2F..%2F7", gotPath)
}

func TestWorldStatuses(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		want   error
	}{
		{"unknown or revoked node", http.StatusNotFound, ErrUnknownNode},
		{"nothing rendered yet", http.StatusConflict, ErrNotRendered},
		{"not the head", http.StatusUnauthorized, ErrUnauthorized},
		{"too many calls", http.StatusTooManyRequests, ErrRateLimited},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				fmt.Fprint(w, `{"error":"whatever"}`)
			})

			_, err := client.World(t.Context(), "3")
			require.ErrorIs(t, err, tt.want)
		})
	}
}

func TestRegisterSendsTheContractBodyAndDecodesTheWorld(t *testing.T) {
	var got Registration

	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/overlay/head/register", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "Bearer "+testBearer, r.Header.Get("Authorization"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))

		fmt.Fprintf(w, `{"node_id":"01920000-0000-7000-8000-000000000001","world":%s}`, worldV1)
	})

	sent := Registration{
		PreauthKeyID: "pak-1",
		NodeKey:      "nodekey:abc",
		MachineKey:   "mkey:def",
		HeadNodeID:   "3",
	}

	result, err := client.Register(t.Context(), sent)
	require.NoError(t, err)

	assert.Equal(t, sent, got)
	assert.Equal(t, "01920000-0000-7000-8000-000000000001", result.NodeID)
	require.NotNil(t, result.World)
	assert.Equal(t, int64(17), result.World.Revision)
}

// A first registration's render is queued in the transaction that has just
// committed, so there is no World to hand back yet. That is a null, not a
// failure, and not an empty document either.
func TestRegisterAcceptsANullWorld(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"node_id":"01920000-0000-7000-8000-000000000002","world":null}`)
	})

	result, err := client.Register(t.Context(), Registration{})
	require.NoError(t, err)
	assert.Equal(t, "01920000-0000-7000-8000-000000000002", result.NodeID)
	assert.Nil(t, result.World)
}

func TestRegisterStatuses(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		want   error
	}{
		{"unknown pre-auth key", http.StatusNotFound, ErrUnknownKey},
		{"spent pre-auth key", http.StatusGone, ErrExpired},
		{"compromised", http.StatusForbidden, ErrCompromised},
		{"contradicts the record", http.StatusConflict, ErrRefused},
		{"malformed", http.StatusUnprocessableEntity, ErrInvalidRequest},
		{"not the head", http.StatusUnauthorized, ErrUnauthorized},
		{"too many calls", http.StatusTooManyRequests, ErrRateLimited},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				fmt.Fprint(w, `{"error":"whatever"}`)
			})

			_, err := client.Register(t.Context(), Registration{})
			require.ErrorIs(t, err, tt.want)
		})
	}
}

func TestRotateSendsTheContractBodyAndAcceptsNoContent(t *testing.T) {
	var got Rotation

	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/overlay/head/rotate", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "Bearer "+testBearer, r.Header.Get("Authorization"))

		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))

		w.WriteHeader(http.StatusNoContent)
	})

	sent := Rotation{HeadNodeID: "3", OldKey: "nodekey:old", NewKey: "nodekey:new"}
	require.NoError(t, client.Rotate(t.Context(), sent))
	assert.Equal(t, sent, got)
}

func TestRotateStatuses(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		want   error
	}{
		{"unknown node", http.StatusNotFound, ErrUnknownNode},
		{"contradicts the record", http.StatusConflict, ErrRefused},
		{"malformed", http.StatusUnprocessableEntity, ErrInvalidRequest},
		{"not the head", http.StatusUnauthorized, ErrUnauthorized},
		{"too many calls", http.StatusTooManyRequests, ErrRateLimited},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			})

			require.ErrorIs(t, client.Rotate(t.Context(), Rotation{}), tt.want)
		})
	}
}

// The framing is Gentility's, byte for byte:
// GentilityWeb.Overlay.HeadController writes "id: <seq>\ndata: <json>\n\n"
// for an event and ": keepalive\n\n" for a heartbeat.
func TestChangesReadsGentilitysFraming(t *testing.T) {
	var gotQuery, gotAccept string

	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery, gotAccept = r.URL.RawQuery, r.Header.Get("Accept")

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		io.WriteString(w, ": keepalive\n\n")
		io.WriteString(w, "id: 41\ndata: {\"seq\":41,\"head_node_id\":\"3\",\"world_revision\":17}\n\n")
		io.WriteString(w, ": keepalive\n\n")
		io.WriteString(w, "id: 42\ndata: {\"seq\":42,\"head_node_id\":\"7\",\"world_revision\":2}\n\n")
	})

	var (
		connected int
		got       []Change
	)

	err := client.Changes(t.Context(), cursor(40), ChangeHandler{
		Connected: func() error { connected++; return nil },
		Changed:   func(c Change) error { got = append(got, c); return nil },
	})
	require.NoError(t, err, "a clean end of stream is how a long poll ends")

	assert.Equal(t, "since=40", gotQuery)
	assert.Equal(t, "text/event-stream, application/json", gotAccept,
		"a 200 is the stream and every refusal on it is JSON, so both are accepted")
	assert.Equal(t, 1, connected)
	assert.Equal(t, []Change{
		{Seq: 41, HeadNodeID: "3", WorldRevision: 17},
		{Seq: 42, HeadNodeID: "7", WorldRevision: 2},
	}, got)
}

// Nothing in the contract says a payload arrives on one line, and the SSE
// rules join several data fields with a newline. A frame split mid-document
// must decode to the same event.
func TestChangesJoinsMultiLineData(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "id: 9\ndata: {\"seq\": 9,\ndata: \"head_node_id\": \"3\",\ndata: \"world_revision\": 4}\n\n")
	})

	var got []Change

	require.NoError(t, client.Changes(t.Context(), cursor(0), ChangeHandler{
		Changed: func(c Change) error { got = append(got, c); return nil },
	}))
	assert.Equal(t, []Change{{Seq: 9, HeadNodeID: "3", WorldRevision: 4}}, got)
}

// A proxy between the head and Gentility may normalise line endings; the SSE
// rules accept CRLF, LF and a bare CR, and so does this reader.
func TestChangesAcceptsCRLFAndBareCR(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, ": keepalive\r\n\r\n")
		io.WriteString(w, "id: 1\r\ndata: {\"seq\":1,\"head_node_id\":\"3\",\"world_revision\":1}\r\n\r\n")
		io.WriteString(w, "id: 2\rdata: {\"seq\":2,\"head_node_id\":\"3\",\"world_revision\":2}\r\r")
	})

	var got []Change

	require.NoError(t, client.Changes(t.Context(), cursor(0), ChangeHandler{
		Changed: func(c Change) error { got = append(got, c); return nil },
	}))
	assert.Equal(t, []Change{
		{Seq: 1, HeadNodeID: "3", WorldRevision: 1},
		{Seq: 2, HeadNodeID: "3", WorldRevision: 2},
	}, got)
}

// A stream of nothing but heartbeats is a stream that is working.
func TestChangesIgnoresKeepalivesAndUnknownFields(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, ": keepalive\n\n: keepalive\n\nevent: ping\nretry: 500\n\n")
	})

	require.NoError(t, client.Changes(t.Context(), cursor(0), ChangeHandler{
		Changed: func(Change) error { t.Error("no event was sent"); return nil },
	}))
}

func TestChangesBehindHorizonCarriesTheHorizon(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
		fmt.Fprint(w, `{"error":"behind_horizon","compaction_horizon":512}`)
	})

	err := client.Changes(t.Context(), cursor(1), ChangeHandler{
		Connected: func() error { t.Error("a refused stream was never connected"); return nil },
	})

	require.ErrorIs(t, err, ErrBehindHorizon)

	var behind *BehindHorizonError
	require.ErrorAs(t, err, &behind)
	assert.Equal(t, int64(512), behind.CompactionHorizon)
}

func TestChangesStatuses(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		want   error
	}{
		{"not the head", http.StatusUnauthorized, ErrUnauthorized},
		{"too many calls", http.StatusTooManyRequests, ErrRateLimited},
		{"unparseable cursor", http.StatusUnprocessableEntity, ErrInvalidRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			})

			require.ErrorIs(t, client.Changes(t.Context(), cursor(0), ChangeHandler{}), tt.want)
		})
	}
}

func TestChangesRefusesANegativeCursorWithoutCalling(t *testing.T) {
	client := serve(t, func(http.ResponseWriter, *http.Request) {
		t.Error("a cursor Gentility would 422 was sent anyway")
	})

	require.ErrorIs(t, client.Changes(t.Context(), cursor(-1), ChangeHandler{}), ErrInvalidRequest)
}

// Cancelling is how the head stops following, and it is not a stream fault.
func TestChangesReturnsTheContextError(t *testing.T) {
	release := make(chan struct{})

	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()

		select {
		case <-release:
		case <-r.Context().Done():
		}
	})

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() { close(release) })

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := client.Changes(ctx, cursor(0), ChangeHandler{})
	require.True(t, errors.Is(err, context.Canceled), "got %v", err)
}

// A payload that is not a change event is a contract break rather than
// something to skip: skipping it would advance the head past an event it never
// applied.
func TestChangesRefusesAnUndecodableEvent(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "id: 1\ndata: not json\n\n")
	})

	err := client.Changes(t.Context(), cursor(0), ChangeHandler{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding change event")
}

// A head with no persisted cursor has never applied an event, so it asks for
// the TAIL rather than for everything the stream still holds: it is about to
// full-refresh every World it caches on Connected, and replaying the backlog
// would be work whose answer that refresh already has.
func TestChangesOmitsSinceWithoutACursor(t *testing.T) {
	var gotQuery string

	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery

		io.WriteString(w, "id: 9\ndata: {\"seq\":9,\"head_node_id\":\"3\",\"world_revision\":4}\n\n")
	})

	var (
		connected int
		got       []Change
	)

	require.NoError(t, client.Changes(t.Context(), nil, ChangeHandler{
		Connected: func() error { connected++; return nil },
		Changed:   func(c Change) error { got = append(got, c); return nil },
	}))

	assert.Empty(t, gotQuery, "no cursor is an absent since, not since=0")
	assert.Equal(t, 1, connected, "the tail subscriber still full-refreshes on connect")
	assert.Equal(t, []Change{{Seq: 9, HeadNodeID: "3", WorldRevision: 4}}, got)
}

// A cursor of zero is a head that HAS one and has applied nothing, which is a
// different request from having none at all.
func TestChangesSendsAZeroCursor(t *testing.T) {
	var gotQuery string

	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
	})

	require.NoError(t, client.Changes(t.Context(), cursor(0), ChangeHandler{}))
	assert.Equal(t, "since=0", gotQuery)
}

// Recovery from a 410 is a TAIL subscription, not since=0. The cursor is past
// the horizon, so every seq below it is gone; asking for zero would ask
// Gentility to replay from a point it has already compacted away and earn a
// second 410, forever. The head full-refreshes instead and follows from now.
func TestChangesRecoversFromTheHorizonWithoutACursor(t *testing.T) {
	var queries []string

	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)

		if len(queries) == 1 {
			w.WriteHeader(http.StatusGone)
			fmt.Fprint(w, `{"error":"behind_horizon","compaction_horizon":512}`)

			return
		}
	})

	err := client.Changes(t.Context(), cursor(7), ChangeHandler{})
	require.ErrorIs(t, err, ErrBehindHorizon)

	// What the head does next, as the doc comment on Changes prescribes.
	require.NoError(t, client.Changes(t.Context(), nil, ChangeHandler{}))

	assert.Equal(t, []string{"since=7", ""}, queries,
		"recovery asks for the tail; since=0 would earn the same 410 forever")
}

// A connection that is open is not a stream that is working. Nothing in TCP
// tells the head that Gentility stopped writing, so the stream is judged by
// the only evidence it has — bytes arriving — and a silent one is torn down
// for the reconnect to replace.
func TestChangesGivesUpOnAStreamThatFallsSilent(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "id: 1\ndata: {\"seq\":1,\"head_node_id\":\"3\",\"world_revision\":1}\n\n")
		w.(http.Flusher).Flush()

		select {
		case <-release:
		case <-r.Context().Done():
		}
	}, WithIdleTimeout(50*time.Millisecond))

	var got []Change

	err := client.Changes(t.Context(), nil, ChangeHandler{
		Changed: func(c Change) error { got = append(got, c); return nil },
	})

	require.ErrorIs(t, err, ErrStreamIdle)
	assert.Len(t, got, 1, "the events that did arrive were still delivered")
}

// A stream whose only traffic is heartbeats is a stream that is working, and
// every line it sends buys it another window.
func TestChangesKeepsAStreamThatKeepsTalking(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		for range 6 {
			io.WriteString(w, ": keepalive\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}, WithIdleTimeout(60*time.Millisecond))

	require.NoError(t, client.Changes(t.Context(), nil, ChangeHandler{}),
		"120ms of heartbeats through a 60ms window is a live stream")
}

// A Gentility that accepts the connection and never answers is the same
// silence one layer earlier, and the transport is what bounds it — there is no
// stream to watch yet.
func TestNewBoundsTheWaitForResponseHeaders(t *testing.T) {
	client := New("https://example.invalid", testBearer)

	transport, ok := client.stream.Transport.(*http.Transport)
	require.True(t, ok, "the stream client carries a transport of its own")
	assert.Equal(t, responseHeaderTimeout, transport.ResponseHeaderTimeout)
	assert.Zero(t, client.stream.Timeout,
		"a Timeout covers the body too, and would cut a healthy stream at its deadline")
}

// The default is a property of the contract rather than of a caller.
func TestNewDefaultsTheIdleTimeout(t *testing.T) {
	assert.Equal(t, defaultIdleTimeout, New("https://example.invalid", testBearer).idleTimeout)
	assert.Equal(t, time.Minute,
		New("https://example.invalid", testBearer, WithIdleTimeout(time.Minute)).idleTimeout)
}

// # A document is bounded before it is believed
//
// Every decode here reads whatever is on the other end of the socket into
// memory. The bearer authenticates that peer, but a compromised or simply
// broken Gentility — or anything that can answer in its place — should not be
// able to spend this head's whole address space by answering one World call.

// TestWorldRefusesAnOversizedDocument: an otherwise valid World past the cap
// is refused rather than read. It is not [ErrUnknownNode] or [ErrNotRendered],
// so the caller treats it as an outage: the head serves what it has cached and
// withholds the response if it has none.
func TestWorldRefusesAnOversizedDocument(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"renderer_version":1,"head_node_id":"3","addresses":["`)

		chunk := make([]byte, 1<<16)
		for i := range chunk {
			chunk[i] = 'a'
		}

		for written := 0; written < maxDocumentBytes+1; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}

		fmt.Fprint(w, `"]}`)
	})

	world, err := client.World(t.Context(), "3")
	require.Error(t, err)
	assert.Nil(t, world)

	assert.NotErrorIs(t, err, ErrUnknownNode)
	assert.NotErrorIs(t, err, ErrNotRendered)
}

// The same cap on the registration's answer, which carries a World of its own.
func TestRegisterRefusesAnOversizedDocument(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"node_id":"`)

		chunk := make([]byte, 1<<16)
		for i := range chunk {
			chunk[i] = 'a'
		}

		for written := 0; written < maxDocumentBytes+1; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}

		fmt.Fprint(w, `"}`)
	})

	result, err := client.Register(t.Context(), Registration{HeadNodeID: "3"})
	require.Error(t, err)
	assert.Nil(t, result)
}

// A document comfortably inside the cap still decodes: the bound is a ceiling
// on the pathological, not a budget the renderer has to fit in.
func TestWorldDecodesADocumentUnderTheCap(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, worldV1)
	})

	world, err := client.World(t.Context(), "3")
	require.NoError(t, err)
	assert.Equal(t, "3", world.HeadNodeID)
}
