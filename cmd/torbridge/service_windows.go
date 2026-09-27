package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// restartTor stops and starts the "tor" Windows service (net waits for each step).
func restartTor(ctx context.Context) error {
	exec.CommandContext(ctx, "net.exe", "stop", "tor").Run() // fails if already stopped
	if out, err := exec.CommandContext(ctx, "net.exe", "start", "tor").CombinedOutput(); err != nil {
		return fmt.Errorf("net start tor: %s", strings.TrimSpace(string(out)))
	}
	logf("tor service restarted")
	return nil
}

func serviceState() string {
	out, _ := exec.Command("sc.exe", "query", "tor").CombinedOutput()
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, "STATE") {
			f := strings.Fields(l)
			return f[len(f)-1]
		}
	}
	return "not installed"
}
