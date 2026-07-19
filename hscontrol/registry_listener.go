package hscontrol

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/juanfont/headscale/hscontrol/types/change"
	"github.com/rs/zerolog/log"
)

const registryReconnectDelay = 3 * time.Second

// registryChangeListener follows the registry's world-change stream so
// grant toggles reach connected clients within a map cycle instead of
// waiting for an unrelated push.
//
// v0: any world change fans a full update to every connected node.
// Fine at dogfood scale; switch to targeted [change.FullSelf] once the
// event's node keys are resolved to node IDs here.
func (h *Headscale) registryChangeListener(ctx context.Context) {
	url := h.cfg.RegistryURL + "/api/changes"

	for ctx.Err() == nil {
		if err := h.consumeRegistryChanges(ctx, url); err != nil && ctx.Err() == nil {
			log.Warn().Err(err).Msg("registry change stream disconnected; reconnecting")
		}

		select {
		case <-time.After(registryReconnectDelay):
		case <-ctx.Done():
			return
		}
	}
}

func (h *Headscale) consumeRegistryChanges(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "text/event-stream")

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry change stream returned %d", resp.StatusCode)
	}

	log.Info().Msg("connected to registry change stream")

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}

		log.Debug().Str("event", data).Msg("registry world changed; sending full updates")
		h.Change(change.FullUpdate())
	}

	return scanner.Err()
}
