package state

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/juanfont/headscale/hscontrol/registry"
	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/rs/zerolog/log"
	"tailscale.com/types/key"
)

// RegistryHook is called inside the node write's transaction. A registration
// is not committed on the head until Gentility has recorded it (design §4
// channels 3 and 4): a refusal returns an error, the gorm transaction rolls
// back, and the node never existed.
//
// It is an interface so the head can run without one. A deployment with no
// registry.url installs none, and both call sites are then upstream headscale
// unchanged.
type RegistryHook interface {
	Registered(ctx context.Context, r registry.Registration) error
	Rotated(ctx context.Context, r registry.Rotation) error
}

// WorldSeeder takes the World that came back with a registration and puts it
// where the head's map builds will find it. [mapper.Batcher.SeedWorld] is the
// implementation; it is a function here because [State] has no business
// knowing what a World cache is, only that a document arrived for a node.
type WorldSeeder func(types.NodeID, *registry.World)

// registryCallbacks is the [RegistryHook] a head with a registry.url runs:
// Gentility over HTTP, plus where a World goes.
type registryCallbacks struct {
	client *registry.Client
	seed   WorldSeeder
}

// NewRegistryHook wires the two callbacks to a Gentility. seed may be nil,
// which discards the World a registration answers with — the change stream
// delivers it again on the next render.
func NewRegistryHook(client *registry.Client, seed WorldSeeder) RegistryHook {
	return &registryCallbacks{client: client, seed: seed}
}

func (h *registryCallbacks) Registered(ctx context.Context, r registry.Registration) error {
	result, err := h.client.Register(ctx, r)
	if err != nil {
		return err
	}

	if result.World == nil || h.seed == nil {
		return nil
	}

	// The inverse of [registry.NodeID], which is what put the id in the
	// payload a moment ago. It cannot fail on a value this process wrote;
	// a World that cannot be attributed to a node is dropped rather than
	// guessed at, and costs one fetch when the node next polls.
	nodeID, err := strconv.ParseUint(r.HeadNodeID, 10, 64)
	if err != nil {
		log.Warn().Str("head.node.id", r.HeadNodeID).
			Msg("registration answered a world for something that is not a head node id")

		return nil
	}

	h.seed(types.NodeID(nodeID), result.World)

	return nil
}

func (h *registryCallbacks) Rotated(ctx context.Context, r registry.Rotation) error {
	return h.client.Rotate(ctx, r)
}

// SetRegistryHook installs the callbacks Gentility is recorded through. It is
// called once at boot, before the head serves anything, because the two call
// sites read it without a lock: a head either has a registry for the whole of
// its run or it does not.
func (s *State) SetRegistryHook(hook RegistryHook) {
	s.registry = hook
}

// recordRegistration reports a pre-auth-key registration to Gentility from
// inside the write that allocated the node id (design §4 channel 3). The
// caller returns its error, which rolls the row back.
//
// Only a pre-auth-key registration is reported: an enrollment is what the
// pre-auth key id identifies, and an interactive or OIDC registration has
// none.
func (s *State) recordRegistration(params newNodeParams, nodeID types.NodeID) error {
	if s.registry == nil || params.PreAuthKey == nil {
		return nil
	}

	// context.Background rather than a request context: what is being
	// bounded here is a database transaction the head has open, and
	// [registry.Client] already deadlines the call at five seconds for
	// exactly that reason. A client that hangs up mid-registration must
	// not leave the head and Gentility disagreeing about whether the node
	// exists.
	err := s.registry.Registered(context.Background(), registry.Registration{
		PreauthKeyID: strconv.FormatUint(params.PreAuthKey.ID, 10),
		NodeKey:      params.NodeKey.String(),
		MachineKey:   params.MachineKey.String(),
		HeadNodeID:   registry.NodeID(nodeID.Uint64()),
	})
	if err != nil {
		return registrationRefusal(err)
	}

	return nil
}

// registrationRefusal says how the client hears about it.
//
// A refusal is FINAL — §2.3's 404, 410, 409 and 403 on register all mean the
// binding contradicts what Gentility holds — so it is reported the way every
// other bad pre-auth key is, as a [types.PAKError] that auth.go turns into a
// 401 carrying the reason. A client that retried one would only be refused
// again.
//
// Anything else is transient: an unreachable Gentility, a 5xx, a timeout. The
// write is rolled back either way, but it surfaces as a plain error — a 500 —
// because the client SHOULD retry, and register is idempotent by content so a
// retry after a lost response records nothing twice.
func registrationRefusal(err error) error {
	switch {
	case errors.Is(err, registry.ErrUnknownKey):
		return fmt.Errorf("%w: %w", types.PAKError("invalid authkey"), err)
	case errors.Is(err, registry.ErrExpired):
		return fmt.Errorf("%w: %w", types.PAKError("authkey expired"), err)
	case errors.Is(err, registry.ErrRefused):
		return fmt.Errorf("%w: %w", types.PAKError("registration refused by the registry"), err)
	case errors.Is(err, registry.ErrCompromised):
		return fmt.Errorf("%w: %w", types.PAKError("authkey is bound to a different machine"), err)
	default:
		return fmt.Errorf("recording registration: %w", err)
	}
}

// recordRotation reports a node key rotation to Gentility from inside the
// write that applies it (design §4 channel 4). The caller returns its error,
// which rolls the update back — row and NodeStore alike.
//
// Idempotent on the new key: a retry after a lost response offers the same
// (old, new) pair and is answered 204 again, so a head whose transaction
// failed simply tries the whole thing once more.
func (s *State) recordRotation(nodeID types.NodeID, oldKey, newKey key.NodePublic) error {
	if s.registry == nil {
		return nil
	}

	err := s.registry.Rotated(context.Background(), registry.Rotation{
		HeadNodeID: registry.NodeID(nodeID.Uint64()),
		OldKey:     oldKey.String(),
		NewKey:     newKey.String(),
	})
	if err != nil {
		return rotationRefusal(err)
	}

	return nil
}

// rotationRefusal is [registrationRefusal]'s counterpart for §2.3's rotate
// column, which has two refusals rather than four and the same rule behind
// them: a contradiction of the record is final and is reported to the client,
// anything else is transient and is retried.
func rotationRefusal(err error) error {
	switch {
	case errors.Is(err, registry.ErrUnknownNode):
		return fmt.Errorf("%w: %w", types.PAKError("the registry does not know this node"), err)
	case errors.Is(err, registry.ErrRefused):
		return fmt.Errorf("%w: %w", types.PAKError("node key rotation refused by the registry"), err)
	default:
		return fmt.Errorf("recording node key rotation: %w", err)
	}
}
