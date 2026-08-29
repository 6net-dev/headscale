package hscontrol

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/juanfont/headscale/hscontrol/registry"
	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/rs/zerolog/log"
)

// registryCache is what the head remembers about Gentility across a restart:
// how far it had followed the change stream, and the World it last held for
// every node it knew about.
//
// Design §4: "The head persists its cache to disk and, on any (re)connect or
// when its cursor is past the compaction horizon, refreshes every cached node
// before serving." The file is the first half of that sentence. It is what
// makes fail-static survive a restart — a head that comes back to an
// unreachable Gentility serves the netmaps it last rendered instead of
// serving nothing — and it is what keeps a boot from replaying the whole
// change stream to learn what it already knew.
//
// It is a CACHE and never a source of truth. Nothing in it cannot be
// re-derived by asking Gentility, which is why every failure to read it is
// survivable and none of them stop the head.
type registryCache struct {
	// Cursor is the last seq the head applied, or nil for a head that has
	// never followed the stream. The two are different requests — nil asks
	// for the tail, zero replays from the beginning — which is why this is
	// a pointer and why the JSON has to be able to say null.
	Cursor *int64 `json:"cursor"`

	// Worlds is keyed by head node id in the decimal form this channel
	// names a node by ([registry.NodeID]), because a JSON object's keys
	// are strings and inventing a second spelling for the same id would be
	// a second thing to keep in step.
	Worlds map[string]*registry.World `json:"worlds"`
}

// registryCacheOf is the cache as it stands, ready to be written.
func registryCacheOf(worlds map[types.NodeID]*registry.World, cursor *int64) registryCache {
	out := registryCache{Cursor: cursor, Worlds: make(map[string]*registry.World, len(worlds))}
	for nodeID, world := range worlds {
		out.Worlds[registry.NodeID(nodeID.Uint64())] = world
	}

	return out
}

// worldsByNodeID is the cache as the mapper holds it.
//
// A key that is not a head node id is dropped rather than guessed at: the
// document cannot be attributed to a node, and a World the head fetches again
// costs one request where a misattributed one would be served to the wrong
// device.
func (c registryCache) worldsByNodeID() map[types.NodeID]*registry.World {
	worlds := make(map[types.NodeID]*registry.World, len(c.Worlds))

	for key, world := range c.Worlds {
		id, err := strconv.ParseUint(key, 10, 64)
		if err != nil {
			log.Warn().Str("head.node.id", key).Msg("registry cache carried an unreadable node id")

			continue
		}

		worlds[types.NodeID(id)] = world
	}

	return worlds
}

// loadRegistryCache reads the cache from disk. An absent file is an empty
// cache and not an error — that is what a head that has never run looks like —
// but a file that is there and will not parse is reported, because silently
// discarding one would hide a state volume that has started corrupting.
func loadRegistryCache(path string) (registryCache, error) {
	var cache registryCache

	blob, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cache, nil
	}

	if err != nil {
		return cache, fmt.Errorf("reading registry cache: %w", err)
	}

	if err := json.Unmarshal(blob, &cache); err != nil {
		return registryCache{}, fmt.Errorf("parsing registry cache %s: %w", path, err)
	}

	return cache, nil
}

// saveRegistryCache writes the cache atomically: a temp file in the same
// directory, flushed to the disk, then renamed over the old one. A head killed
// mid-write comes back to the whole of the previous cache rather than half of
// this one, and rename within a directory is the only write this filesystem
// offers that is all-or-nothing.
//
// The temp file is created in the destination's own directory because rename
// is atomic only within a filesystem, and /tmp is routinely a different one.
func saveRegistryCache(path string, cache registryCache) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating registry cache directory: %w", err)
	}

	blob, err := json.Marshal(cache)
	if err != nil {
		return fmt.Errorf("encoding registry cache: %w", err)
	}

	// 0600 by construction. A World is not a secret, but it is the whole
	// topology of an installation and nothing else on the host has a
	// reason to read it.
	tmp, err := os.CreateTemp(dir, ".registry-cache-*")
	if err != nil {
		return fmt.Errorf("creating registry cache temp file: %w", err)
	}

	if err := writeAndSync(tmp, blob); err != nil {
		os.Remove(tmp.Name())

		return err
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())

		return fmt.Errorf("replacing registry cache: %w", err)
	}

	return nil
}

// writeAndSync puts the bytes on the disk rather than in the page cache. The
// point of the temp-and-rename is that the file is whole when the rename makes
// it visible, and without the fsync the rename can outlive the contents across
// a power cut.
func writeAndSync(file *os.File, blob []byte) error {
	defer file.Close()

	if _, err := file.Write(blob); err != nil {
		return fmt.Errorf("writing registry cache: %w", err)
	}

	if err := file.Sync(); err != nil {
		return fmt.Errorf("flushing registry cache: %w", err)
	}

	return nil
}
