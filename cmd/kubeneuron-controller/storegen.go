// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/kubeneuron/kubeneuron/internal/storegen"
)

// storeGenerationInterval bounds how far a store copied by other means than
// the backup endpoint (a volume snapshot, pg_dump) can lag the generation.
const storeGenerationInterval = 5 * time.Minute

type storeOlderPauser interface {
	PauseForOlderStore(ctx context.Context) error
}

// checkStoreGeneration pauses automation when the store is older than the one
// this controller last used, then raises the generation. Only the writer
// calls it. When the copy cannot be read nothing is claimed; an error is
// returned only when the pause itself could not be stored.
func checkStoreGeneration(ctx context.Context, tracker *storegen.Tracker, ctrl storeOlderPauser, log *slog.Logger) error {
	older, err := tracker.Older(ctx)
	if err != nil {
		log.Warn("Cannot tell whether the workflow store is older than the one this controller last used; not pausing", "err", err)
		return nil
	}
	if older {
		if err := ctrl.PauseForOlderStore(ctx); err != nil {
			return fmt.Errorf("pause for an older workflow store: %w", err)
		}
	}
	if err := tracker.Advance(ctx); err != nil {
		log.Warn("Raising the workflow store generation failed", "err", err)
	}
	return nil
}

// advanceStoreGeneration raises the generation every interval until ctx ends.
func advanceStoreGeneration(ctx context.Context, tracker *storegen.Tracker, log *slog.Logger, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := tracker.Advance(ctx); err != nil && ctx.Err() == nil {
				log.Warn("Raising the workflow store generation failed", "err", err)
			}
		}
	}
}
