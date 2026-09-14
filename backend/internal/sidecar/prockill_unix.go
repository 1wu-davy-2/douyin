//go:build !windows

package sidecar

import (
	"errors"
	"log"
	"os/exec"
	"syscall"
)

// setNewProcessGroup puts the sidecar into its own process group (POSIX) so
// the whole tree — python plus any F2 node helpers it spawns — can be killed
// with one signal to the negative pgid.
func setNewProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree kills the process group and the process itself. It calls
// syscall.Kill directly on purpose: minimal container images (python:*-slim)
// ship no procps, so exec.Command("kill", ...) fails with "executable file
// not found in $PATH" and leaves an orphan sidecar holding its port — every
// restart then dies with "Address already in use" until the container is
// recreated.
func killProcessTree(pid int) {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		log.Printf("[sidecar] kill pgid=%d: %v", pid, err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		log.Printf("[sidecar] kill pid=%d: %v", pid, err)
	}
}
