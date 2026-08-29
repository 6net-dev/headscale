package hscontrol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/juanfont/headscale/hscontrol/registry"
	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cachedWorld(headNodeID string) *registry.World {
	return &registry.World{
		RendererVersion: registry.RendererVersion1,
		Revision:        7,
		HeadNodeID:      headNodeID,
		Addresses:       []string{"fd7a:115c:a1e0:1::a"},
	}
}

// The whole point of the file: what a head knew before it stopped is what it
// knows when it starts, cursor and documents together.
func TestRegistryCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry-cache.json")
	cursor := int64(42)

	require.NoError(t, saveRegistryCache(path, registryCache{
		Cursor: &cursor,
		Worlds: map[string]*registry.World{"3": cachedWorld("3")},
	}))

	loaded, err := loadRegistryCache(path)
	require.NoError(t, err)
	require.NotNil(t, loaded.Cursor)
	assert.Equal(t, int64(42), *loaded.Cursor)
	assert.Equal(t, cachedWorld("3"), loaded.Worlds["3"])

	assert.Equal(t, map[types.NodeID]*registry.World{types.NodeID(3): cachedWorld("3")},
		loaded.worldsByNodeID())
}

// A cursor of zero and no cursor at all are different requests — the client
// takes a POINTER for exactly that reason — so the file has to be able to say
// both.
func TestRegistryCacheDistinguishesNoCursorFromZero(t *testing.T) {
	dir := t.TempDir()

	zero := int64(0)
	require.NoError(t, saveRegistryCache(filepath.Join(dir, "zero.json"), registryCache{Cursor: &zero}))
	require.NoError(t, saveRegistryCache(filepath.Join(dir, "none.json"), registryCache{}))

	loadedZero, err := loadRegistryCache(filepath.Join(dir, "zero.json"))
	require.NoError(t, err)
	require.NotNil(t, loadedZero.Cursor)
	assert.Equal(t, int64(0), *loadedZero.Cursor)

	loadedNone, err := loadRegistryCache(filepath.Join(dir, "none.json"))
	require.NoError(t, err)
	assert.Nil(t, loadedNone.Cursor)
}

// A head that has never run has no file, and that is not a fault: it starts
// cold, asks for the tail, and caches what it fetches on demand.
func TestRegistryCacheIsEmptyWhenAbsent(t *testing.T) {
	loaded, err := loadRegistryCache(filepath.Join(t.TempDir(), "absent.json"))
	require.NoError(t, err)
	assert.Nil(t, loaded.Cursor)
	assert.Empty(t, loaded.Worlds)
}

// Temp file plus rename, so a head killed mid-write comes back to the whole of
// the last cache rather than half of this one — and leaves no debris behind.
func TestRegistryCacheWritesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry-cache.json")

	first := int64(1)
	require.NoError(t, saveRegistryCache(path, registryCache{Cursor: &first}))

	second := int64(2)
	require.NoError(t, saveRegistryCache(path, registryCache{Cursor: &second}))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temp file may be left behind")
	assert.Equal(t, "registry-cache.json", entries[0].Name())

	loaded, err := loadRegistryCache(path)
	require.NoError(t, err)
	require.NotNil(t, loaded.Cursor)
	assert.Equal(t, int64(2), *loaded.Cursor)
}

// The directory is made rather than demanded: a fresh volume mounted at the
// state directory is the ordinary first boot.
func TestRegistryCacheCreatesItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "registry-cache.json")

	require.NoError(t, saveRegistryCache(path, registryCache{}))
	assert.FileExists(t, path)
}

// A file that will not parse is reported rather than swallowed here; what the
// head DOES about it is the runner's decision, not the loader's.
func TestRegistryCacheReportsUnreadableContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry-cache.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	_, err := loadRegistryCache(path)
	require.Error(t, err)
}

// The worlds a mapper hands back are keyed by node id and the file's are
// keyed by the decimal id the channel names a node by, so the conversion has
// to survive the trip in both directions.
func TestRegistryCacheKeysAreHeadNodeIDs(t *testing.T) {
	cache := registryCacheOf(map[types.NodeID]*registry.World{types.NodeID(3): cachedWorld("3")}, nil)

	blob, err := json.Marshal(cache)
	require.NoError(t, err)
	assert.Contains(t, string(blob), `"3":`)

	assert.Equal(t, map[types.NodeID]*registry.World{types.NodeID(3): cachedWorld("3")},
		cache.worldsByNodeID())
}

// An id the file carries that is not a head node id is dropped rather than
// guessed at: the cache is a convenience, and a document that cannot be
// attributed to a node is one the head is better off fetching again.
func TestRegistryCacheDropsAnUnreadableKey(t *testing.T) {
	cache := registryCache{Worlds: map[string]*registry.World{"three": cachedWorld("three")}}

	assert.Empty(t, cache.worldsByNodeID())
}

// writeUnreadable puts something at the cache path that is not a cache.
func writeUnreadable(path string) error {
	return os.WriteFile(path, []byte("{not json"), 0o600)
}
