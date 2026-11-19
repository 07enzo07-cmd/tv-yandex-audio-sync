//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func setPdeathsig(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}

// ensureLowLatAsoundMount reversibly bind-mounts /data/audiosync/asound.lowlat.conf (dmix 960/240)
// onto /etc/asound.conf and cleanly respawns maind --mediad (supervised by quasar_launcher2)
// ONLY when newly mounted so both Alice and aplay share the 960/240 dmix IPC without EINVAL.
func ensureLowLatAsoundMount() {
	const src = "/data/audiosync/asound.lowlat.conf"
	const dst = "/etc/asound.conf"
	if _, err := os.Stat(src); err != nil {
		return
	}
	if mounts, err := os.ReadFile("/proc/mounts"); err == nil {
		if strings.Contains(string(mounts), " /etc/asound.conf ") {
			return // Already mounted cleanly
		}
	}
	if err := syscall.Mount(src, dst, "", syscall.MS_BIND, ""); err != nil {
		fmt.Fprintf(os.Stderr, "[alsa-init] bind-mount %s -> %s skipped: %v\n", src, dst, err)
		return
	}
	fmt.Fprintf(os.Stderr, "[alsa-init] bind-mounted %s -> %s (dmix 960/240)\n", src, dst)

	// Respawn maind --mediad so it opens the new 960/240 dmix_spk IPC cleanly (quasar_launcher2 restarts it in <1s)
	out, err := exec.Command("sh", "-c", `ps | grep "maind --mediad" | grep -v grep | awk '{print $1}'`).Output()
	if err == nil {
		for _, pidStr := range strings.Fields(string(out)) {
			_ = exec.Command("kill", "-9", pidStr).Run()
		}
		if len(strings.Fields(string(out))) > 0 {
			time.Sleep(1500 * time.Millisecond)
			fmt.Fprintf(os.Stderr, "[alsa-init] respawned maind --mediad for 960/240 dmix IPC alignment\n")
		}
	}
}
