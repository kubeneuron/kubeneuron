package dcgm

import (
	"context"
	"errors"
	"testing"
)

func TestProberVersionIsBoundedAndStrict(t *testing.T) {
	for name, tc := range map[string]struct {
		out     string
		err     error
		want    string
		wantErr bool
	}{
		"dcgmi version":       {out: "dcgmi version: 4.1.2\n", want: "dcgm-4.1.2"},
		"DCGM version":        {out: "NVIDIA DCGM v3.3.9\n", want: "dcgm-3.3.9"},
		"command failure":     {err: errors.New("not found"), wantErr: true},
		"unrecognized output": {out: "version latest\n", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			p := New("dcgmi")
			p.run = func(context.Context, string, ...string) ([]byte, error) {
				return []byte(tc.out), tc.err
			}
			got, err := p.Version(context.Background())
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("Version() = %q, %v; want %q, error=%t", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestProberGPUCountRequiresDiscoveryEvidence(t *testing.T) {
	for name, tc := range map[string]struct {
		out     string
		err     error
		want    int
		wantErr bool
	}{
		"active GPUs":       {out: "8 GPUs found (Active).\n", want: 8},
		"singular GPU":      {out: "1 GPU found.\n", want: 1},
		"command failure":   {err: errors.New("host engine unavailable"), wantErr: true},
		"zero GPUs":         {out: "0 GPUs found.\n", wantErr: true},
		"unparsed response": {out: "discovery unavailable\n", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			p := New("dcgmi")
			p.run = func(context.Context, string, ...string) ([]byte, error) {
				return []byte(tc.out), tc.err
			}
			got, err := p.GPUCount(context.Background())
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("GPUCount() = %d, %v; want %d, error=%t", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// dcgmi rejects --host before the subcommand: it prints usage and exits
// zero-ish, which would look like a successful probe returning nothing.
// Measured against dcgmi 4.5 on a live node, so this ordering is a contract
// with the binary, not a style preference.
func TestDiscoveryPlacesHostAfterSubcommand(t *testing.T) {
	var got []string
	p := NewWithEndpoint("dcgmi", "nvidia-dcgm.gpu-operator.svc:5555")
	p.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		got = args
		return []byte("1 GPU found (Active).\n"), nil
	}
	if _, err := p.GPUCount(context.Background()); err != nil {
		t.Fatalf("GPUCount: %v", err)
	}
	want := []string{"discovery", "--host", "nvidia-dcgm.gpu-operator.svc:5555", "-l"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args = %v, want %v", got, want)
		}
	}

	// Without an endpoint the probe stays local and carries no host flag.
	local := New("dcgmi")
	local.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		got = args
		return []byte("1 GPU found (Active).\n"), nil
	}
	if _, err := local.GPUCount(context.Background()); err != nil {
		t.Fatalf("local GPUCount: %v", err)
	}
	for _, arg := range got {
		if arg == "--host" {
			t.Fatalf("local probe carried --host: %v", got)
		}
	}
}
