// Package dcgm provides a deliberately narrow, bounded local DCGM version
// probe. It does not run diagnostics or modify the host; it only supplies
// attested runtime identity for the accelerator readiness contract.
package dcgm

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const probeTimeout = 5 * time.Second

// BundledClientPath is the DCGM client shipped in the agent image.
//
// It is an absolute path on purpose. The agent's PATH begins with the host
// tooling directory (nvidia-smi must come from the host, to match the host
// driver), so a bare "dcgmi" lookup would silently prefer whatever client
// happens to be installed on the node. That makes the attested runtime version
// a property of how each node was provisioned: one fleet, one profile, and
// nodes quietly attesting different versions. Attestation gates destructive
// actions, so it resolves to one known binary unless an operator names another.
const BundledClientPath = "/usr/bin/dcgmi"

var versionPattern = regexp.MustCompile(`(?i)\b(?:dcgmi|dcgm)\b[^0-9\r\n]*\b(v?[0-9]+(?:\.[0-9]+){1,3}(?:[-+][0-9a-z.-]+)?)\b`)
var discoveryCountPattern = regexp.MustCompile(`(?im)\b([0-9]+)\s+GPUs?\s+found\b`)

type runner func(context.Context, string, ...string) ([]byte, error)

// Prober runs the DCGM CLI mounted into the agent's reviewed runtime image.
// Path defaults to dcgmi from PATH.
type Prober struct {
	Path string
	// Endpoint is the DCGM host engine address, e.g. "10.0.1.7:5555".
	// Empty uses dcgmi's own default (the local engine). The caller is
	// responsible for it naming this node: an endpoint pointing elsewhere
	// would attest the wrong hardware.
	Endpoint string
	run      runner
}

// New returns a bounded DCGM version prober.
// NewWithEndpoint builds a prober that talks to a specific host engine.
func NewWithEndpoint(path, endpoint string) *Prober {
	p := New(path)
	p.Endpoint = strings.TrimSpace(endpoint)
	return p
}

func New(path string) *Prober {
	if path == "" {
		path = "dcgmi"
	}
	return &Prober{Path: path, run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}}
}

// hostArgs points dcgmi at the configured host engine. The flag must follow
// the subcommand: dcgmi parses "dcgmi discovery --host X -l" but answers
// "dcgmi --host X discovery -l" with its usage text, which a caller reading
// only the exit status would mistake for a working probe. Only queries that
// need the engine carry it; --version answers from the client binary alone.
func (p *Prober) hostArgs() []string {
	if p == nil || strings.TrimSpace(p.Endpoint) == "" {
		return nil
	}
	return []string{"--host", p.Endpoint}
}

// Version returns a normalized exact DCGM version, prefixed with dcgm-. It
// rejects arbitrary command text rather than accepting a configured/image tag
// as runtime evidence.
//
// This is the version of the DCGM *client*, not of the host engine it talks
// to: `dcgmi --version` answers from the local binary and never contacts the
// engine, and dcgmi exposes no command that reports the engine's build. With
// the GPU Operator the two differ in practice — a 4.6.1 client happily serves a
// 4.5.2 engine — so an AcceleratorRuntimeProfile's runtimeVersion pins the
// client the agent carries, and the engine is attested separately by GPUCount:
// a live connection whose device count must match the independent nvidia-smi
// inventory. Both must hold before a report can be ready.
func (p *Prober) Version(ctx context.Context) (string, error) {
	if p == nil || strings.TrimSpace(p.Path) == "" {
		return "", fmt.Errorf("DCGM version probe is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := p.run(ctx, p.Path, "--version")
	if err != nil {
		return "", fmt.Errorf("dcgmi --version: %w", err)
	}
	version, err := parseVersion(string(out))
	if err != nil {
		return "", err
	}
	return "dcgm-" + strings.TrimPrefix(version, "v"), nil
}

// GPUCount verifies the DCGM host-engine connection without changing health
// watches or starting a diagnostic. NVIDIA documents `dcgmi discovery -l` as
// the installation verification command; its count is later compared with the
// independent nvidia-smi inventory before an agent can claim a ready runtime.
func (p *Prober) GPUCount(ctx context.Context) (int, error) {
	if p == nil || strings.TrimSpace(p.Path) == "" {
		return 0, fmt.Errorf("DCGM discovery probe is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	args := append([]string{"discovery"}, p.hostArgs()...)
	out, err := p.run(ctx, p.Path, append(args, "-l")...)
	if err != nil {
		return 0, fmt.Errorf("dcgmi discovery -l: %w", err)
	}
	match := discoveryCountPattern.FindStringSubmatch(string(out))
	if len(match) != 2 {
		return 0, fmt.Errorf("dcgmi discovery -l returned no GPU count")
	}
	count, err := strconv.Atoi(match[1])
	if err != nil || count <= 0 {
		return 0, fmt.Errorf("dcgmi discovery -l returned invalid GPU count %q", match[1])
	}
	return count, nil
}

func parseVersion(output string) (string, error) {
	match := versionPattern.FindStringSubmatch(output)
	if len(match) != 2 {
		return "", fmt.Errorf("dcgmi --version returned no recognizable DCGM version")
	}
	return match[1], nil
}
