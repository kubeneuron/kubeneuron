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
