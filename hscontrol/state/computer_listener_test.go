package state

import (
	"errors"
	"net/http"
	"testing"

	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

func answering(answer bool) RegisterOption {
	return FromComputerListener(func(types.NodeID) bool { return answer })
}

func requireRefusedPAK(t *testing.T, err error) {
	t.Helper()

	perr, ok := errors.AsType[types.PAKError](err)
	require.True(t, ok, "the client hears a PAKError as a failed registration, got %v", err)
	assert.Equal(t, errNotAComputer, perr)
}

func TestRegisterSaysWhichListenerItCameFrom(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts []RegisterOption
		want bool
	}{
		{"the computer listener", []RegisterOption{answering(false)}, true},
		{"any other", nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeRegistry(t)
			s, pak, _ := registryTestState(t, fake)

			_, _, err := s.HandleNodeFromPreAuthKey(
				registerRequest(pak, key.NewNode().Public()), key.NewMachine().Public(), tt.opts...,
			)
			require.NoError(t, err)

			registrations, _ := fake.recorded()
			require.Len(t, registrations, 1)
			assert.Equal(t, tt.want, registrations[0].ComputerListener)
		})
	}
}

// A key Gentility says is not a computer's leaves nothing behind: no node, and
// a one-shot key unspent.
func TestRegisterNotAComputerIsRolledBack(t *testing.T) {
	fake := newFakeRegistry(t)
	fake.answers(http.StatusConflict, `{"error":"not_a_computer"}`)

	s, _, _ := registryTestState(t, fake)
	user := s.CreateUserForTest("one-shot")
	pak, err := s.CreatePreAuthKey(user.TypedID(), false, false, nil, nil)
	require.NoError(t, err)

	machineKey := key.NewMachine().Public()
	nodeKey := key.NewNode().Public()

	_, _, err = s.HandleNodeFromPreAuthKey(registerRequest(pak, nodeKey), machineKey, answering(false))
	requireRefusedPAK(t, err)

	nodes, err := s.DB().ListNodes()
	require.NoError(t, err)
	assert.Empty(t, nodes)
	assert.Empty(t, s.GetNodesByMachineKeyAllUsers(machineKey))

	_, ok := s.GetNodeByNodeKey(nodeKey)
	assert.False(t, ok)

	unspent, err := s.GetPreAuthKey(pak.Key)
	require.NoError(t, err)
	assert.False(t, unspent.Used)
}

// The head asked about the node before it took the registration lock, and a
// node created since then was never asked about. A re-registration makes no
// callback, so this is the only thing that could refuse it.
func TestRegisterAsksAgainAboutANodeThatExists(t *testing.T) {
	fake := newFakeRegistry(t)
	s, pak, _ := registryTestState(t, fake)

	machineKey := key.NewMachine().Public()
	nodeKey := key.NewNode().Public()
	node, _, err := s.HandleNodeFromPreAuthKey(registerRequest(pak, nodeKey), machineKey)
	require.NoError(t, err)

	rotated := registerRequest(pak, key.NewNode().Public())
	rotated.Hostinfo = &tailcfg.Hostinfo{Hostname: "changed"}

	_, _, err = s.HandleNodeFromPreAuthKey(rotated, machineKey, answering(false))
	requireRefusedPAK(t, err)

	stored, err := s.DB().GetNodeByID(node.ID())
	require.NoError(t, err)
	assert.Equal(t, nodeKey, stored.NodeKey)
	assert.Equal(t, "registry-node", stored.Hostinfo.Hostname)

	current, ok := s.GetNodeByID(node.ID())
	require.True(t, ok)
	assert.Equal(t, nodeKey, current.NodeKey())
	assert.Equal(t, "registry-node", current.Hostinfo().Hostname())

	_, rotations := fake.recorded()
	assert.Empty(t, rotations)

	var asked types.NodeID

	_, _, err = s.HandleNodeFromPreAuthKey(rotated, machineKey, FromComputerListener(func(id types.NodeID) bool {
		asked = id

		return true
	}))
	require.NoError(t, err)
	assert.Equal(t, node.ID(), asked)

	_, rotations = fake.recorded()
	assert.Len(t, rotations, 1)
}

// A head with no registry has nothing to ask whose key it is.
func TestRegisterFromTheComputerListenerNeedsARegistry(t *testing.T) {
	s, pak, _ := registryTestState(t, nil)

	_, _, err := s.HandleNodeFromPreAuthKey(
		registerRequest(pak, key.NewNode().Public()), key.NewMachine().Public(), answering(true),
	)
	requireRefusedPAK(t, err)

	nodes, err := s.DB().ListNodes()
	require.NoError(t, err)
	assert.Empty(t, nodes)
}
