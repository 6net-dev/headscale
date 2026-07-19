// Package registryclient speaks to the Elixir registry — the source of
// truth for orgs, grants, addressing, and topology. The wire head
// consumes the versioned World(device) contract and reports device
// registrations upward; it holds no org logic of its own.
package registryclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

var ErrUnknownDevice = errors.New("registry: unknown device")

// World is the composed view the registry publishes for one device.
// Version 0 of the contract; see registry/lib/registrar/world.ex.
type World struct {
	Version int       `json:"version"`
	Self    WorldSelf `json:"self"`
	Peers   []Peer    `json:"peers"`
	DNS     WorldDNS  `json:"dns"`
	Filters []Filter  `json:"filters"`
}

type WorldSelf struct {
	Addresses []string `json:"addresses"`
	Orgs      []string `json:"orgs"`
}

type Peer struct {
	Name      string   `json:"name"`
	NodeKey   string   `json:"node_key"`
	Addresses []string `json:"addresses"`
	Orgs      []string `json:"orgs"`
}

type WorldDNS struct {
	SearchDomains []string `json:"search_domains"`
}

type Filter struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
}

type Client struct {
	baseURL string
	http    *http.Client
}

func New(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

// World fetches the composed world for a device by its node key.
func (c *Client) World(ctx context.Context, nodeKey string) (*World, error) {
	u := c.baseURL + "/api/world/" + url.PathEscape(nodeKey)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var w World
		if err := json.NewDecoder(resp.Body).Decode(&w); err != nil {
			return nil, fmt.Errorf("registry: decoding world: %w", err)
		}

		return &w, nil
	case http.StatusNotFound:
		return nil, ErrUnknownDevice
	default:
		return nil, fmt.Errorf("registry: world request returned %d", resp.StatusCode)
	}
}

// UpsertDevice reports a registered device to the registry. Idempotent
// by node key.
func (c *Client) UpsertDevice(ctx context.Context, name, nodeKey string) error {
	body, err := json.Marshal(map[string]string{"name": name, "node_key": nodeKey})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.baseURL+"/api/devices",
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("registry: device upsert returned %d", resp.StatusCode)
	}

	return nil
}
