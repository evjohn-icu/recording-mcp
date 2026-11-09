package cloudsync

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ProbeLocalCUDA verifies the configured interpreter can actually reach the
// GPU before jobs are queued. CPU devices return nil: there is nothing to probe.
func ProbeLocalCUDA(python, device string) error {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(device)), "cuda") {
		return nil
	}
	if strings.TrimSpace(python) == "" {
		python = "python3"
	}
	code := "import torch\n" +
		"assert torch.cuda.is_available(), 'torch.cuda.is_available() is false'\n" +
		"print(torch.cuda.get_device_name(0))\n"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, python, "-c", code).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = "interpreter did not start"
		}
		return fmt.Errorf("cloudsync: local ASR CUDA probe failed on %s: %s: %w", device, detail, err)
	}
	return nil
}

// GPUInfo reads the first GPU's name and memory through nvidia-smi. An absent
// binary or a CPU host returns empty/zero values, which the heartbeat reports
// as "no GPU" rather than an error.
func GPUInfo() (string, int, int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=name,memory.total,memory.free", "--format=csv,noheader,nounits", "-i", "0").Output()
	if err != nil {
		return "", 0, 0
	}
	fields := strings.Split(strings.TrimSpace(string(out)), ",")
	if len(fields) != 3 {
		return "", 0, 0
	}
	name := strings.TrimSpace(fields[0])
	total, err := strconv.Atoi(strings.TrimSpace(fields[1]))
	if err != nil {
		return name, 0, 0
	}
	free, err := strconv.Atoi(strings.TrimSpace(fields[2]))
	if err != nil {
		return name, total, 0
	}
	return name, total, free
}
