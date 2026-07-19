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
	Name      string       `json:"name"`
	Addresses []string     `json:"addresses"`
	Orgs      []string     `json:"orgs"`
	Entries   []WorldEntry `json:"entries"`
}

// WorldEntry is one (org, address, fqdn) triple — a device is named
// separately inside each org it participates in.
type WorldEntry struct {
	Org     string `json:"org"`
	Address string `json:"address"`
	FQDN    string `json:"fqdn"`
}

type Peer struct {
	Name      string       `json:"name"`
	NodeKey   string       `json:"node_key"`
	Addresses []string     `json:"addresses"`
	Orgs      []string     `json:"orgs"`
	Entries   []WorldEntry `json:"entries"`
}

type WorldDNS struct {
	BaseDomain    string   `json:"base_domain"`
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

// Grant is one org membership as reported by the registry's grants
// listing. Inactive grants still hold their address slot.
type Grant struct {
	Org                string  `json:"org"`
	OrgName            string  `json:"org_name"`
	Identity           string  `json:"identity"`
	Active             bool    `json:"active"`
	Address            string  `json:"address"`
	AdmissionExpiresAt *string `json:"admission_expires_at"`
}

type DeviceGrants struct {
	Device struct {
		Name    string `json:"name"`
		NodeKey string `json:"node_key"`
	} `json:"device"`
	Grants []Grant `json:"grants"`
}

// Grants lists a device's grants, active and inactive.
func (c *Client) Grants(ctx context.Context, nodeKey string) (*DeviceGrants, error) {
	u := c.baseURL + "/api/devices/" + url.PathEscape(nodeKey) + "/grants"

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
		var dg DeviceGrants
		if err := json.NewDecoder(resp.Body).Decode(&dg); err != nil {
			return nil, fmt.Errorf("registry: decoding grants: %w", err)
		}

		return &dg, nil
	case http.StatusNotFound:
		return nil, ErrUnknownDevice
	default:
		return nil, fmt.Errorf("registry: grants request returned %d", resp.StatusCode)
	}
}

// SetActive toggles a grant's activation — the six o'clock switch.
func (c *Client) SetActive(ctx context.Context, nodeKey, org string, active bool) error {
	body, err := json.Marshal(map[string]bool{"active": active})
	if err != nil {
		return err
	}

	u := c.baseURL + "/api/devices/" + url.PathEscape(nodeKey) + "/grants/" + url.PathEscape(org)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry: toggle returned %d", resp.StatusCode)
	}

	return nil
}

// RedeemInvite presents an invite token, admitting this device to the
// invite's org with an active grant.
func (c *Client) RedeemInvite(ctx context.Context, token, nodeKey string) (string, error) {
	body, err := json.Marshal(map[string]string{"node_key": nodeKey})
	if err != nil {
		return "", err
	}

	u := c.baseURL + "/api/invites/" + url.PathEscape(token) + "/redeem"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var out struct {
		Org     string `json:"org"`
		Address string `json:"address"`
		Error   string `json:"error"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("registry: decoding redemption: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("registry: invite redemption failed: %s", out.Error)
	}

	return out.Org + " → " + out.Address, nil
}
