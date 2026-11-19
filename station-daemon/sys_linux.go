//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

func setPdeathsig(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}

// ensureLowLatAsoundMount is a no-op when maind already holds dmix_spk (ipc_key 1024)
// at 7680/960; altering dmix_spk while maind is active causes ALSA IPC EINVAL.
func ensureLowLatAsoundMount() {}
