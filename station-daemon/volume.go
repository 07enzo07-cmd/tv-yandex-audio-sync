package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// VolumePctToLinearGain maps a 0..100% Station volume level to a linear PCM amplitude multiplier
// matching ALSA softvol Master (-51.0..0.0 dB): gain = 0 at pct == 0, else 10^((-51*(1-pct/100))/20).
func VolumePctToLinearGain(pct int) float64 {
	if pct <= 0 {
		return 0.0
	}
	if pct >= 100 {
		return 1.0
	}
	db := -51.0 * (1.0 - float64(pct)/100.0)
	return math.Pow(10.0, db/20.0)
}

// SoftVolume applies smooth, click-free per-frame software volume scaling to S16_LE stereo PCM
// when bypassing ALSA softvol in direct hw:0,0 low-latency mode.
type SoftVolume struct {
	currentGain float64
	initialized bool
}

// Reset snaps the current gain immediately to targetPct (used on stream start / JOIN).
func (sv *SoftVolume) Reset(targetPct int) {
	sv.currentGain = VolumePctToLinearGain(targetPct)
	sv.initialized = true
}

// ScalePCM scales stereo S16_LE PCM in src into dst, smoothly ramping gain toward targetPct
// (~15 ms full-scale ramp @ 48 kHz) to prevent audible clicks on volume changes.
func (sv *SoftVolume) ScalePCM(src, dst []byte, targetPct int) []byte {
	targetGain := VolumePctToLinearGain(targetPct)
	if !sv.initialized {
		sv.currentGain = targetGain
		sv.initialized = true
	}
	if len(src) == 0 {
		return src
	}
	if cap(dst) < len(src) {
		dst = make([]byte, len(src))
	}
	dst = dst[:len(src)]

	frames := len(src) / 4
	// Max linear gain step per 48 kHz frame (~15 ms for full 0.0->1.0 ramp)
	const maxStepPerFrame = 1.0 / (48.0 * 15.0)
	g := sv.currentGain

	for i := 0; i < frames; i++ {
		diff := targetGain - g
		if diff > maxStepPerFrame {
			g += maxStepPerFrame
		} else if diff < -maxStepPerFrame {
			g -= maxStepPerFrame
		} else {
			g = targetGain
		}
		for ch := 0; ch < 2; ch++ {
			off := i*4 + ch*2
			s := int16(binary.LittleEndian.Uint16(src[off : off+2]))
			v := float64(s) * g
			if v > 32767.0 {
				v = 32767.0
			} else if v < -32768.0 {
				v = -32768.0
			}
			binary.LittleEndian.PutUint16(dst[off:off+2], uint16(int16(math.Round(v))))
		}
	}
	sv.currentGain = g
	return dst
}

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
