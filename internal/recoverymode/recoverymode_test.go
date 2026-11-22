// SPDX-License-Identifier: Apache-2.0

package recoverymode

import (
	"strings"
	"testing"
	"time"
)

func valid() *Settings {
	return &Settings{
		NodeSelector: map[string]string{"pool": "train"}, MaxConcurrentNodes: 1,
		NodeCooldown: time.Hour, MaxUnhealthyPercent: 30,
		EnabledBy: "alice", EnabledAt: time.Now(), Acknowledgement: Acknowledgement,
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]struct {
		mode    Mode
		mutate  func(*Settings) *Settings
		wantErr string
	}{
		"approval without settings":  {Approval, func(*Settings) *Settings { return nil }, ""},
		"unset without settings":     {Unset, func(*Settings) *Settings { return nil }, ""},
		"dry-run with settings":      {DryRun, func(s *Settings) *Settings { return s }, "only allowed with mode"},
		"automatic without settings": {Automatic, func(*Settings) *Settings { return nil }, "requires the automatic settings"},
		"automatic, valid":           {Automatic, func(s *Settings) *Settings { return s }, ""},
		"empty selector":             {Automatic, func(s *Settings) *Settings { s.NodeSelector = nil; return s }, "node_selector must name"},
		"blank selector value":       {Automatic, func(s *Settings) *Settings { s.NodeSelector = map[string]string{"pool": ""}; return s }, "must not be empty"},
		"zero concurrency":           {Automatic, func(s *Settings) *Settings { s.MaxConcurrentNodes = 0; return s }, "max_concurrent_nodes"},
		"per-pool above total":       {Automatic, func(s *Settings) *Settings { s.MaxConcurrentNodesPerPool = 2; return s }, "per_pool"},
		"no cool-down":               {Automatic, func(s *Settings) *Settings { s.NodeCooldown = 0; return s }, "node_cooldown"},
		"breaker out of range":       {Automatic, func(s *Settings) *Settings { s.MaxUnhealthyPercent = 0; return s }, "max_unhealthy_percent"},
		"nobody enabled it":          {Automatic, func(s *Settings) *Settings { s.EnabledBy = " "; return s }, "enabled_by"},
		"no time":                    {Automatic, func(s *Settings) *Settings { s.EnabledAt = time.Time{}; return s }, "enabled_at"},
		"wrong acknowledgement":      {Automatic, func(s *Settings) *Settings { s.Acknowledgement = "ok"; return s }, "acknowledgement"},
		"unknown mode":               {Mode("yolo"), func(*Settings) *Settings { return nil }, "unknown recovery mode"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := Validate(tc.mode, tc.mutate(valid()))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestSelectsNodeNeverMatchesEverything(t *testing.T) {
	s := valid()
	if !s.SelectsNode(map[string]string{"pool": "train", "zone": "a"}) {
		t.Fatal("matching labels must select")
	}
	if s.SelectsNode(map[string]string{"pool": "infer"}) || s.SelectsNode(nil) {
		t.Fatal("other or unknown labels must not select")
	}
	s.NodeSelector = nil
	if s.SelectsNode(map[string]string{"pool": "train"}) {
		t.Fatal("an empty selector must select nothing")
	}
	if Unset.Effective() != Approval {
		t.Fatal("an unset mode is approval")
	}
}
