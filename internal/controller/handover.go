// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"

	"github.com/kubeneuron/kubeneuron/pkg/types"
)

type handover struct {
	What, Check, Command string
	auditParams          map[string]string
}

func (h handover) message() string {
	return strings.Join([]string{h.What, h.Check, h.Command}, " ")
}

func (h handover) validate(id string) error {
	if strings.TrimSpace(h.What) == "" || strings.TrimSpace(h.Check) == "" {
		return errors.New("handover requires a reason and an operator check")
	}
	if id == "" || (h.Command != "kubeneuronctl resolve "+id && h.Command != settlementCommand(id)) {
		return errors.New("handover requires a complete resolve command for this incident")
	}
	return nil
}

func (c *Controller) handOver(ctx context.Context, inc *types.Incident, actor, action string, h handover, completed bool) error {
	if err := h.validate(inc.ID); err != nil {
		return err
	}
	return c.transitionWithRepairStep(ctx, inc, types.StateNeedsHuman, actor, action, h.message(), h.auditParams, "", completed)
}
