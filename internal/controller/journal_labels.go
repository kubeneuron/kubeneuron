// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sync"
	"time"

	kubeneuronv1alpha1 "github.com/kubeneuron/kubeneuron/api/v1alpha1"
	"github.com/kubeneuron/kubeneuron/internal/config"
	"github.com/kubeneuron/kubeneuron/internal/platform"
)

// Why a node's agent has no journal. The agent reports the journal as
// unavailable; these say why, in health and in the KernelLogSource
// condition.
const (
	JournalReasonDisabled             = "disabled"
	JournalReasonOSImageNotRecognized = "os-image-not-recognized"
	JournalReasonMountFailed          = "journal-mount-failed"
)

const (
	// journalMountWait is how long a journal-variant agent Pod may wait to
	// start its container before the label is taken off its node.
	journalMountWait = 3 * time.Minute
	// journalMountRetry is how long the label stays off after a mount
	// failure, unless the node's OS image or its override changes.
	journalMountRetry = time.Hour
)

// journalLabels is the controller's memory of the journal label. It is lost
// on restart: a node whose mount failed is then labelled again at once, and
// taken off again after journalMountWait.
type journalLabels struct {
	mu sync.Mutex
	// failed is when the mount failed on a node, and for which OS image
	// and directory.
	failed map[string]journalFailure
	// reasons is why each accelerator node has no journal label.
	reasons map[string]string
}

type journalFailure struct {
	at    time.Time
	basis string
}

// JournalReason is why the node has no journal mount, or "" when the
// controller chose a journal directory for it or does not know the node.
func (c *Controller) JournalReason(node string) string {
	c.journal.mu.Lock()
	defer c.journal.mu.Unlock()
	return c.journal.reasons[node]
}

// reconcileJournalLabels sets kubeneuron.io/journal-dir on accelerator nodes
// the agent runs on, from the OS image table or an override, and removes it
// from the other nodes the agent's node selector matches. A configuration without the agent_journal block leaves
// the label alone. A failed read changes nothing.
func (c *Controller) reconcileJournalLabels(ctx context.Context) {
	rc := c.runtimeConfig(ctx)
	labeler, ok := c.platform.(platform.JournalLabeler)
	if rc == nil || rc.AgentJournal == nil || !ok {
		return
	}
	cfg := rc.AgentJournal
	nodes, err := labeler.JournalNodes(ctx, cfg.Namespace, cfg.Installation)
	if err != nil {
		if c.logOnce.first("journal-nodes/" + err.Error()) {
			c.log.Warn("could not read nodes for the journal label", "err", err)
		}
		return
	}
	rows, err := c.store.ListNodes(ctx)
	if err != nil {
		if c.logOnce.first("journal-inventory/" + err.Error()) {
			c.log.Warn("could not read the inventory for the journal label", "err", err)
		}
		return
	}
	// The GPUs an agent reported count for the node it reported them on: a
	// node recreated with the same name is another machine, which may have
	// none.
	accelerators := map[string]string{}
	for _, row := range rows {
		if len(row.GPUs) != 0 || len(row.Devices) != 0 {
			accelerators[row.Name] = row.UID
		}
	}
	now := time.Now()
	reasons := map[string]string{}
	for i := range nodes {
		n := &nodes[i]
		uid, reported := accelerators[n.Name]
		accelerator := n.Accelerator || reported && (n.UID == "" || uid == n.UID)
		dir, reason := c.journal.want(cfg, n, accelerator, now)
		if accelerator && reason != "" && !cfg.KernelMessages {
			reasons[n.Name] = reason
		}
		if dir == n.Dir {
			continue
		}
		if err := labeler.SetJournalDir(ctx, n.Name, dir); err != nil {
			if c.logOnce.first("journal-label/" + n.Name + "/" + err.Error()) {
				c.log.Warn("could not set the journal label", "node", n.Name, "err", err)
			}
			continue
		}
		c.log.Info("journal label changed", "node", n.Name, "from", n.Dir, "to", dir, "reason", reason)
	}
	c.journal.mu.Lock()
	c.journal.reasons = reasons
	for name := range c.journal.failed {
		if _, ok := reasons[name]; !ok {
			// Gone, or labelled again: the node names are not a bounded set.
			delete(c.journal.failed, name)
		}
	}
	c.journal.mu.Unlock()
}

// want is the label value for a node, and why it has none. A node outside
// the agent's node selector keeps its label as it is: it may belong to
// another installation's agents.
func (j *journalLabels) want(cfg *config.AgentJournal, n *platform.JournalNode, accelerator bool, now time.Time) (string, string) {
	if !labelsMatchSelector(cfg.AgentNodeSelector, n.Labels) {
		return n.Dir, ""
	}
	if !cfg.Enabled {
		return "", JournalReasonDisabled
	}
	if !accelerator {
		return "", ""
	}
	dir := ""
	overridden := false
	for _, o := range cfg.NodeOverrides {
		if labelsMatchSelector(o.NodeSelector, n.Labels) {
			dir, overridden = o.Dir, true
			break
		}
	}
	if !overridden {
		dir = journalDirForImage(n.OSImage, n.ProviderID)
	}
	switch dir {
	case kubeneuronv1alpha1.JournalDirNone:
		return "", JournalReasonDisabled
	case "":
		return "", JournalReasonOSImageNotRecognized
	}
	// A mount failure is the node's: the node recreated with the same name
	// is tried again at once.
	basis := n.UID + "\x00" + n.OSImage + "\x00" + dir
	j.mu.Lock()
	defer j.mu.Unlock()
	if n.Dir == dir && !n.WaitingSince.IsZero() && now.Sub(n.WaitingSince) >= journalMountWait {
		if j.failed == nil {
			j.failed = map[string]journalFailure{}
		}
		j.failed[n.Name] = journalFailure{at: now, basis: basis}
	}
	if f, ok := j.failed[n.Name]; ok {
		if f.basis == basis && now.Sub(f.at) < journalMountRetry {
			return "", JournalReasonMountFailed
		}
		delete(j.failed, n.Name)
	}
	return dir, ""
}
