//go:build !windows

package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// torUnit is the systemd unit written by install.sh.
const torUnit = "torbridge-tor.service"

// restartTor restarts the systemd unit (systemctl waits for the job to finish).
func restartTor(ctx context.Context) error {
	if out, err := exec.CommandContext(ctx, "systemctl", "restart", torUnit).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl restart %s: %s", torUnit, strings.TrimSpace(string(out)))
	}
	logf("tor service restarted")
	return nil
}

func serviceState() string {
	out, _ := exec.Command("systemctl", "is-active", torUnit).Output()
	if s := strings.TrimSpace(string(out)); s != "" {
		return s
	}
	return "unknown"
}
