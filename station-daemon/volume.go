package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type volRequest struct {
	tvVol int
	pct   int
	muted bool
}

// VolumeWorker serializes all amixer calls into a single owner goroutine with
// latest-value coalescing and 45 ms debounce to eliminate race conditions.
type VolumeWorker struct {
	mu      sync.Mutex
	pending *volRequest
	notify  chan struct{}
	stopCh  chan struct{}
	lastPct int
	hasLast bool
}

func NewVolumeWorker() *VolumeWorker {
	vw := &VolumeWorker{
		notify: make(chan struct{}, 1),
		stopCh: make(chan struct{}),
	}
	go vw.loop()
	return vw
}

func (vw *VolumeWorker) Set(tvVol int, pct int, muted bool) {
	if muted || pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	vw.mu.Lock()
	if vw.hasLast && vw.lastPct == pct && vw.pending == nil {
		vw.mu.Unlock()
		return
	}
	vw.pending = &volRequest{tvVol: tvVol, pct: pct, muted: muted}
	vw.mu.Unlock()

	select {
	case vw.notify <- struct{}{}:
	default:
	}
}

func (vw *VolumeWorker) Stop() {
	select {
	case <-vw.stopCh:
	default:
		close(vw.stopCh)
	}
}

func (vw *VolumeWorker) loop() {
	for {
		select {
		case <-vw.stopCh:
			return
		case <-vw.notify:
		}

		// Debounce 45 ms so rapid remote clicks coalesce to the latest value
		// (except mute=true which applies immediately!)
		vw.mu.Lock()
		isMute := vw.pending != nil && vw.pending.muted
		vw.mu.Unlock()
		if !isMute {
			select {
			case <-vw.stopCh:
				return
			case <-time.After(45 * time.Millisecond):
			}
		}

		vw.mu.Lock()
		req := vw.pending
		vw.pending = nil
		if req == nil || (vw.hasLast && vw.lastPct == req.pct) {
			vw.mu.Unlock()
			continue
		}
		vw.lastPct = req.pct
		vw.hasLast = true
		vw.mu.Unlock()

		out, err := exec.Command("amixer", "set", "Master", fmt.Sprintf("%d%%", req.pct)).CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[volume] amixer set Master %d%% failed: %v (%s)\n",
				req.pct, err, strings.TrimSpace(string(out)))
		} else {
			if req.muted {
				fmt.Fprintf(os.Stderr, "[volume] TV MUTED -> Station 0%%\n")
			} else {
				fmt.Fprintf(os.Stderr, "[volume] TV %d -> Station %d%%\n", req.tvVol, req.pct)
			}
		}
	}
}
