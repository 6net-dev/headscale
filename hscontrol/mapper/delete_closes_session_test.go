package mapper

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"tailscale.com/tailcfg"
)

// TestDeleteNodeEndsTheLongPoll is the whole of design §9's revocation step 2,
// end to end: a node with a live control session is deleted, and the session
// ends. Deletion is how a revocation reaches a device, and a device whose poll
// survives its own deletion keeps the netmap it was last sent for as long as
// it cares to hold the connection.
//
// The stop function is what poll.go hands the batcher for exactly this
// purpose; calling it is what unblocks the poll and closes the stream.
func TestDeleteNodeEndsTheLongPoll(t *testing.T) {
	data, cleanup := setupBatcherWithTestData(t, NewBatcherAndMapper, 1, 2, normalBufferSize)
	defer cleanup()

	target := &data.Nodes[0]

	var once sync.Once

	stopped := make(chan struct{})

	require.NoError(t, data.Batcher.AddNode(
		target.n.ID,
		target.ch,
		tailcfg.CapabilityVersion(100),
		func() { once.Do(func() { close(stopped) }) },
	))

	node, ok := data.State.GetNodeByID(target.n.ID)
	require.True(t, ok)

	deleted, err := data.State.DeleteNode(node)
	require.NoError(t, err)

	data.Batcher.AddWork(deleted)

	select {
	case <-stopped:
	case <-time.After(updateTimeout):
		t.Fatal("deleting a node left its control session open")
	}
}
