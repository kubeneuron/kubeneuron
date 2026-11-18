// SPDX-License-Identifier: Apache-2.0

package cloudtest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kubeneuron/kubeneuron/pkg/cloud"
)

// pool is a fixed-size pool that tags itself with the operation it grew for,
// as a provider would mark its own request.
type pool struct {
	members  []string
	size     int
	marker   string
	held     bool
	pending  bool
	lose     bool
	requests int
	next     int
}

func (p *pool) HoldNextRequest() { p.held = true }
func (p *pool) LoseNextAnswer()  { p.lose = true }
func (p *pool) Requests() int    { return p.requests }
func (p *pool) Size() int        { return p.size }
func (p *pool) Deliver() {
	if p.pending {
		p.pending = false
		p.add()
	}
}

func (p *pool) add() {
	p.next++
	p.members = append(p.members, "new-"+strconv.Itoa(p.next))
}

func (p *pool) CheckAddFirst(context.Context, string) error { return nil }

func (p *pool) PrepareSuccessor(_ context.Context, s cloud.Successor) (cloud.Successor, error) {
	if s.UnrequestedPlan() {
		return s, cloud.ErrNeedsOperator
	}
	switch s.Phase {
	case "":
		s.Phase = cloud.SuccessorPlanned
		s.State = map[string]string{"size": strconv.Itoa(p.size), "members": strings.Join(p.members, ",")}
		return s, nil
	case cloud.SuccessorPlanned:
		if p.marker != s.Operation {
			p.requests++
			p.size++
			p.marker = s.Operation
			if p.held {
				p.held, p.pending = false, true
			} else {
				p.add()
			}
			if p.lose {
				p.lose = false
				return s, errors.New("the answer was lost")
			}
		}
		s.Phase = cloud.SuccessorRequested
		return s, nil
	case cloud.SuccessorRequested:
		added := p.added(s)
		switch {
		case len(added) == 0 && p.pending:
			return s, nil
		case len(added) == 1:
			s.Phase, s.ProviderID = cloud.SuccessorCreated, "fake://"+added[0]
			return s, nil
		}
		return s, fmt.Errorf("%w: %d new members", cloud.ErrNeedsOperator, len(added))
	}
	return s, fmt.Errorf("phase %q", s.Phase)
}

func (p *pool) added(s cloud.Successor) []string {
	before := strings.Split(s.State["members"], ",")
	var added []string
	for _, m := range p.members {
		if !slices.Contains(before, m) {
			added = append(added, m)
		}
	}
	return added
}

func (p *pool) ReleaseSuccessor(_ context.Context, s cloud.Successor) error {
	if s.UnrequestedPlan() {
		return nil
	}
	if p.marker != s.Operation {
		return nil // nothing of this operation reached the cloud
	}
	if s.Phase == cloud.SuccessorCreated {
		p.remove(strings.TrimPrefix(s.ProviderID, "fake://"))
		return nil
	}
	if p.pending || len(p.added(s)) != 0 {
		return fmt.Errorf("%w: the pool's request is not settled", cloud.ErrNeedsOperator)
	}
	p.size--
	p.marker = ""
	return nil
}

func (p *pool) ReplaceWithSuccessor(_ context.Context, s cloud.Successor) error {
	p.remove(s.Instance)
	return nil
}

func (p *pool) remove(member string) {
	if i := slices.Index(p.members, member); i >= 0 {
		p.members = slices.Delete(p.members, i, i+1)
		p.size--
	}
	p.marker = ""
}

func TestSuccessorConformanceOnAReferencePool(t *testing.T) {
	SuccessorConformance(t, SuccessorCase{
		Offline:  (*pool)(nil),
		Instance: "old",
		New: func(*testing.T) (cloud.SuccessorProvider, SuccessorCloud) {
			p := &pool{members: []string{"old", "other"}, size: 2}
			return p, p
		},
	})
}
