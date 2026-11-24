package hscontrol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/juanfont/headscale/hscontrol/state"
	"github.com/rs/zerolog/log"
	"tailscale.com/tailcfg"
)

// computerListenerHeader marks a control session that came through the
// listener computers reach the head on. The proxy that owns the listener sets
// it on every request, replacing whatever the client sent, and its presence is
// the mark: the value means nothing. A client that sends it on any other
// listener only asks for the same refusals.
const computerListenerHeader = "X-Gentility-Computer-Listener"

const noiseRegisterPath = "/machine/register"

var errNotAComputersNode = errors.New("not a computer's node")

func fromComputerListener(req *http.Request) bool {
	return len(req.Header.Values(computerListenerHeader)) > 0
}

// computersOnly is the one check on a session from the computer listener: it
// may carry only a computer's own node. It wraps the whole Noise router, so no
// handler sees, and nothing is stored from, a request it refuses.
//
// The session proves a machine key, and every handler acts only on a node with
// that key, so the nodes with that key are what is checked.
func (ns *noiseServer) computersOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := ns.admitComputer(r) //nolint:contextcheck // a World read has a deadline of its own and fills a cache other requests use
		if err != nil {
			log.Info().
				Err(err).
				Str("machine.key", ns.machineKey.ShortString()).
				Str("path", r.URL.Path).
				Msg("refused a request from the computer listener")

			// HTTP/2 has no such header: to the server it means close the
			// connection once it is idle.
			w.Header().Set("Connection", "close")
			http.Error(w, errNotAComputersNode.Error(), http.StatusForbidden)

			return
		}

		next.ServeHTTP(w, r)
	})
}

// admitComputer answers nil when every node with the session's machine key is
// a computer's own. With no node there is one request that can make one a
// computer's: a registration with a pre-auth key, which Gentility judges.
func (ns *noiseServer) admitComputer(r *http.Request) error {
	nodes := ns.headscale.state.GetNodesByMachineKeyAllUsers(ns.machineKey)

	for _, node := range nodes {
		if !ns.headscale.mapBatcher.IsComputer(node.ID()) {
			return fmt.Errorf("%w: node %d", errNotAComputersNode, node.ID())
		}
	}

	if len(nodes) > 0 {
		return nil
	}

	if r.URL.Path != noiseRegisterPath {
		return fmt.Errorf("%w: no node", errNotAComputersNode)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("reading registration: %w", err)
	}

	r.Body = io.NopCloser(bytes.NewReader(body))

	var req tailcfg.RegisterRequest

	err = json.Unmarshal(body, &req)
	if err != nil {
		return fmt.Errorf("decoding registration: %w", err)
	}

	// A followup is read before the key is, so one with a key is not a
	// registration by it.
	if !isAuthKey(req) || req.Followup != "" {
		return fmt.Errorf("%w: no node and no authkey", errNotAComputersNode)
	}

	return nil
}

func (ns *noiseServer) registerOptions() []state.RegisterOption {
	if !ns.computerListener {
		return nil
	}

	return []state.RegisterOption{
		state.FromComputerListener(ns.headscale.mapBatcher.IsComputer),
	}
}
