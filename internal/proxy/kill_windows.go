//go:build windows

package proxy

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// killPID forcefully terminates the process on Windows.
func killPID(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// listenerPIDs uses netstat to find processes listening on port on Windows.
func listenerPIDs(port int) []int {
	out, err := exec.Command("netstat", "-ano", "-p", "TCP").Output()
	if err != nil {
		return nil
	}
	target := ":" + strconv.Itoa(port)
	var pids []int
	seen := map[int]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, target) || !strings.Contains(line, "LISTENING") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if pid, convErr := strconv.Atoi(fields[len(fields)-1]); convErr == nil && !seen[pid] {
			pids = append(pids, pid)
			seen[pid] = true
		}
	}
	return pids
}
