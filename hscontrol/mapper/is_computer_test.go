package mapper

import (
	"errors"
	"testing"
	"time"

	"github.com/juanfont/headscale/hscontrol/registry"
	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/stretchr/testify/assert"
)

var errRegistryDown = errors.New("registry down")

func TestIsComputer(t *testing.T) {
	const nodeID = types.NodeID(1)

	computer := world("1", "acme", "n1", []string{selfAddr})
	computer.Computer = true

	machine := world("1", "acme", "n1", []string{selfAddr})

	unsupported := world("1", "acme", "n1", []string{selfAddr})
	unsupported.Computer = true
	unsupported.RendererVersion = 99

	for _, tt := range []struct {
		name   string
		served *registry.World
		err    error
		want   bool
	}{
		{"its World says so", computer, nil, true},
		{"its World does not", machine, nil, false},
		{"a World this head cannot read", unsupported, nil, false},
		{"unknown", nil, registry.ErrUnknownNode, false},
		{"not rendered", nil, registry.ErrNotRendered, false},
		{"removed", nil, registry.ErrRemoved, false},
		{"none cached and the registry down", nil, errRegistryDown, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeWorlds(map[string]*registry.World{"1": tt.served})
			if tt.err != nil {
				store.errs["1"] = tt.err
			}

			b := &Batcher{mapper: newTestMapper(store)}
			assert.Equal(t, tt.want, b.IsComputer(nodeID))
		})
	}
}

func TestIsComputerReadsTheCachedWorld(t *testing.T) {
	computer := world("1", "acme", "n1", []string{selfAddr})
	computer.Computer = true

	store := newFakeWorlds(map[string]*registry.World{"1": computer})
	tick := &clock{at: time.Now()}
	m := newTestMapper(store)
	m.now = tick.now
	b := &Batcher{mapper: m}

	assert.True(t, b.IsComputer(1))
	assert.True(t, b.IsComputer(1))
	assert.Equal(t, 1, store.callsFor("1"), "a fresh World is not asked for again")

	// Past its TTL and the registry down: as last seen, like a netmap.
	store.errs["1"] = errRegistryDown

	tick.advance(2 * defaultWorldTTL)
	assert.True(t, b.IsComputer(1))

	// The registry answers that it is no computer's any more.
	delete(store.errs, "1")
	store.worlds["1"] = world("1", "acme", "n1", []string{selfAddr})

	tick.advance(2 * defaultWorldTTL)
	assert.False(t, b.IsComputer(1))
}

func TestIsComputerWithoutARegistry(t *testing.T) {
	assert.False(t, (&Batcher{}).IsComputer(1))
	assert.False(t, (&Batcher{mapper: &mapper{}}).IsComputer(1))
}
