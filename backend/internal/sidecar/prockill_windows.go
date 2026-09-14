//go:build windows

package sidecar

import (
	"log"
	"os/exec"
	"strconv"
)

// setNewProcessGroup is a no-op on Windows: the tree is killed via
// taskkill /T, which walks child processes without needing a separate group.
func setNewProcessGroup(cmd *exec.Cmd) {}

// killProcessTree kills the process and all of its children via taskkill.
func killProcessTree(pid int) {
	out, err := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").CombinedOutput()
	if err != nil {
		// "process not found" simply means it already died — fine.
		log.Printf("[sidecar] kill pid=%d: %v: %s", pid, err, string(out))
	}
}
