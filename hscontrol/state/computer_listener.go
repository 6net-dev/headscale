package state

import (
	"github.com/juanfont/headscale/hscontrol/types"
)

// errNotAComputer refuses a registration from the computer listener. It is a
// [types.PAKError], so the client hears it as a failed registration.
const errNotAComputer = types.PAKError("not a computer's authkey")

// RegisterOption says what the head knows about the session a registration
// arrived on and the request itself cannot say.
type RegisterOption func(*registerOptions)

type registerOptions struct {
	// isComputer is set when the session came from the computer listener.
	isComputer func(types.NodeID) bool
}

// FromComputerListener marks a registration that arrived on a session from the
// computer listener, which may carry only a computer's own node. Gentility
// judges a key for a new node, and isComputer a node that already exists. The
// head asked isComputer before it handled the request, and it is asked again
// under the registration lock because the node may have been created since.
func FromComputerListener(isComputer func(types.NodeID) bool) RegisterOption {
	return func(o *registerOptions) { o.isComputer = isComputer }
}

func newRegisterOptions(opts []RegisterOption) registerOptions {
	var o registerOptions
	for _, opt := range opts {
		opt(&o)
	}

	return o
}

func (o registerOptions) computerListener() bool {
	return o.isComputer != nil
}

// refuses says whether the session may not carry a node that already exists.
func (o registerOptions) refuses(node types.NodeView) bool {
	return o.computerListener() && !o.isComputer(node.ID())
}
