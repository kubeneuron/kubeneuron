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

var versionPattern = regexp.MustCompile(`(?i)\b(?:dcgmi|dcgm)\b[^0-9\r\n]*\b(v?[0-9]+(?:\.[0-9]+){1,3}(?:[-+][0-9a-z.-]+)?)\b`)
var discoveryCountPattern = regexp.MustCompile(`(?im)\b([0-9]+)\s+GPUs?\s+found\b`)

type runner func(context.Context, string, ...string) ([]byte, error)

// Prober runs the DCGM CLI mounted into the agent's reviewed runtime image.
// Path defaults to dcgmi from PATH.
type Prober struct {
	Path string
	run  runner
}

// New returns a bounded DCGM version prober.
func New(path string) *Prober {
	if path == "" {
		path = "dcgmi"
	}
	return &Prober{Path: path, run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}}
}

// Version returns a normalized exact DCGM version, prefixed with dcgm-. It
// rejects arbitrary command text rather than accepting a configured/image tag
// as runtime evidence.
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
	out, err := p.run(ctx, p.Path, "discovery", "-l")
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
