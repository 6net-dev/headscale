package registryclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWorldFetchAndParse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/world/nk:mac":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"version": 0,
				"self": map[string]any{
					"addresses": []string{"fd7a:115c:a1e0::1", "fd7a:115c:a1e0:100::1"},
					"orgs":      []string{"personal", "acme"},
				},
				"peers": []map[string]any{
					{
						"name":      "core7",
						"node_key":  "nk:core7",
						"addresses": []string{"fd7a:115c:a1e0::2"},
						"orgs":      []string{"personal"},
					},
				},
				"dns":     map[string]any{"search_domains": []string{"personal.overlay.internal"}},
				"filters": []map[string]any{{"src": "fd7a:115c:a1e0::/56", "dst": "fd7a:115c:a1e0::/56"}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(srv.URL)

	world, err := c.World(context.Background(), "nk:mac")
	if err != nil {
		t.Fatalf("World: %v", err)
	}

	if len(world.Self.Addresses) != 2 {
		t.Errorf("want 2 self addresses, got %d", len(world.Self.Addresses))
	}

	if len(world.Peers) != 1 || world.Peers[0].NodeKey != "nk:core7" {
		t.Errorf("unexpected peers: %+v", world.Peers)
	}

	if _, err := c.World(context.Background(), "nk:unknown"); err != ErrUnknownDevice {
		t.Errorf("want ErrUnknownDevice, got %v", err)
	}
}

func TestUpsertDevice(t *testing.T) {
	var got map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/devices" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	if err := New(srv.URL).UpsertDevice(context.Background(), "mac", "nk:mac"); err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}

	if got["name"] != "mac" || got["node_key"] != "nk:mac" {
		t.Errorf("unexpected payload: %+v", got)
	}
}
