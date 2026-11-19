package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	fcntlSetPipe  = 1031   // F_SETPIPE_SZ on Linux
	ioctlFionread = 0x541B // FIONREAD on Linux
)

func getInitialDelayMs() int {
	target := 65 // Safe low-latency default for webOS (dmix 1920/480 + aplay 960/240 -> H_min~35ms)
	data, err := os.ReadFile("/data/audiosync/delay_ms")
	if err == nil {
		str := strings.TrimSpace(string(data))
		if val, err := strconv.Atoi(str); err == nil && val >= defaultMinStationMs && val <= maxStationMs {
			target = val
		}
	}
	return target
}

func saveCalibratedDelay(delayMs int) {
	if delayMs >= defaultMinStationMs && delayMs <= maxStationMs {
		_ = os.WriteFile("/data/audiosync/delay_ms", []byte(strconv.Itoa(delayMs)), 0644)
	}
}

func isClosedLoopEnabled() bool {
	data, err := os.ReadFile("/data/audiosync/auto_control")
	if err != nil {
		return true // Default ON in v3
	}
	str := strings.TrimSpace(string(data))
	return str == "1" || strings.EqualFold(str, "true") || strings.EqualFold(str, "closed_loop")
}

func isTvActuatorEnabled() bool {
	data, err := os.ReadFile("/data/audiosync/tv_actuator_enabled")
	if err != nil {
		return false // Disabled by default on VOX (Phase 0.3 showed avSyncSpeaker is not post-tap)
	}
	str := strings.TrimSpace(string(data))
	return str == "1" || strings.EqualFold(str, "true")
}

func getConfiguredPeerIP() string {
	data, err := os.ReadFile("/data/audiosync/peer_ip")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

type Player struct {
	alsaXruns  atomic.Uint64
	writeSince time.Time
	mu         sync.Mutex
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	pipeFd     uintptr
	pid        int
	running    bool
}

func (p *Player) Start() error {
	p.Stop()

	p.mu.Lock()
	defer p.mu.Unlock()

	// Route through 'default' (softvol -> dmix_spk_plug -> dmix_spk 1920/480) to preserve amixer Master and Alice!
	// Omit -q so we can parse real ALSA underrun (XRUN) messages from stderr!
	cmd := exec.Command("/usr/bin/aplay",
		"-D", "default",
		"--buffer-size=1440",
		"--period-size=480",
		"-R", "20000",
		"-t", "raw",
		"-f", "S16_LE",
		"-r", "48000",
		"-c", "2",
		"-",
	)
	setPdeathsig(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		return err
	}

	var fd uintptr
	if file, ok := stdin.(*os.File); ok {
		fd = file.Fd()
		_, _, _ = syscall.Syscall(syscall.SYS_FCNTL, fd, fcntlSetPipe, 4096)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return err
	}

	// Parse aplay stderr for real ALSA XRUNs and errors
	go func(r io.ReadCloser) {
		defer r.Close()
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line := sc.Text()
			low := strings.ToLower(line)
			if strings.Contains(low, "underrun") || strings.Contains(low, "xrun") {
				p.alsaXruns.Add(1)
				fmt.Fprintf(os.Stderr, "[alsa-xrun] %s\n", line)
			} else if strings.Contains(low, "error") || strings.Contains(low, "failed") || strings.Contains(low, "invalid") {
				fmt.Fprintf(os.Stderr, "[aplay-err] %s\n", line)
			}
		}
	}(stderrPipe)

	p.cmd = cmd
	p.stdin = stdin
	p.pipeFd = fd
	p.pid = cmd.Process.Pid
	p.running = true
	return nil
}

func (p *Player) AlsaXruns() uint64 {
	return p.alsaXruns.Load()
}

func (p *Player) PipeQueuedBytes() int {
	p.mu.Lock()
	if !p.running || p.pipeFd == 0 {
		p.mu.Unlock()
		return 0
	}
	fd := p.pipeFd
	p.mu.Unlock()

	var bytesInPipe int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlFionread, uintptr(unsafe.Pointer(&bytesInPipe)))
	if errno != 0 || bytesInPipe < 0 {
		return 0
	}
	return int(bytesInPipe)
}

func (p *Player) Write(data []byte) error {
	p.mu.Lock()
	if !p.running || p.stdin == nil {
		p.mu.Unlock()
		return fmt.Errorf("player not running")
	}
	w := p.stdin
	p.writeSince = time.Now()
	p.mu.Unlock()

	_, err := w.Write(data)
	p.mu.Lock()
	if p.stdin == w {
		p.writeSince = time.Time{}
	}
	p.mu.Unlock()
	return err
}

func (p *Player) Stop() {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return
	}
	p.running = false
	p.writeSince = time.Time{}
	w := p.stdin
	p.stdin = nil
	p.pipeFd = 0
	cmd := p.cmd
	p.cmd = nil
	pid := p.pid
	p.pid = 0
	p.mu.Unlock()

	if w != nil {
		_ = w.Close()
	}

	if cmd != nil {
		done := make(chan error, 1)
		go func() {
			done <- cmd.Wait()
		}()
		select {
		case <-done:
		case <-time.After(150 * time.Millisecond):
			if pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGTERM)
			}
			select {
			case <-done:
			case <-time.After(100 * time.Millisecond):
				if pid > 0 {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
				<-done
			}
		}
	}
}

func (p *Player) recoverBlockedWrite() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running && !p.writeSince.IsZero() && time.Since(p.writeSince) > 750*time.Millisecond {
		_ = p.cmd.Process.Signal(syscall.SIGINT)
		p.writeSince = time.Now()
	}
}

func (p *Player) IsRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// retimePCM linearly resamples stereo S16_LE PCM from inFrames to outFrames smoothly.
func retimePCM(src, dst []byte, inFrames, outFrames int) []byte {
	if inFrames == outFrames || inFrames < 2 || outFrames < 2 {
		return src
	}
	needed := outFrames * 4
	if cap(dst) < needed {
		dst = make([]byte, needed)
	}
	dst = dst[:needed]
	denom := outFrames - 1
	maxIdx := inFrames - 1
	for j := 0; j < outFrames; j++ {
		pos := j * maxIdx
		i := pos / denom
		frac := pos % denom
		next := i + 1
		if next > maxIdx {
			next = maxIdx
		}
		for ch := 0; ch < 2; ch++ {
			a := int(int16(binary.LittleEndian.Uint16(src[i*4+ch*2:])))
			b := int(int16(binary.LittleEndian.Uint16(src[next*4+ch*2:])))
			binary.LittleEndian.PutUint16(dst[j*4+ch*2:], uint16(int16(a+(b-a)*frac/denom)))
		}
	}
	return dst
}

// applyFade scales stereo S16_LE PCM linearly from startGain to endGain (0.0..1.0).
func applyFade(pcm []byte, startGain, endGain float64) []byte {
	out := make([]byte, len(pcm))
	frames := len(pcm) / 4
	if frames == 0 {
		return out
	}
	for i := 0; i < frames; i++ {
		t := float64(i) / float64(frames)
		g := startGain + (endGain-startGain)*t
		for ch := 0; ch < 2; ch++ {
			off := i*4 + ch*2
			v := float64(int16(binary.LittleEndian.Uint16(pcm[off:off+2]))) * g
			if v > 32767 {
				v = 32767
			} else if v < -32768 {
				v = -32768
			}
			binary.LittleEndian.PutUint16(out[off:off+2], uint16(int16(math.Round(v))))
		}
	}
	return out
}

type queuedFrame struct {
	pcm     []byte
	frameMs float64
	isPLC   bool
}

func main() {
	port := "12345"
	if len(os.Args) > 1 {
		port = os.Args[1]
	}

	logFile, err := os.OpenFile("/tmp/station-rx.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err == nil {
		os.Stderr = logFile
	}
	ensureLowLatAsoundMount()

	addr, err := net.ResolveUDPAddr("udp4", "0.0.0.0:"+port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ResolveUDPAddr error: %v\n", err)
		os.Exit(1)
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ListenUDP error: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()
	_ = conn.SetReadBuffer(524288)

	player := &Player{}
	volWorker := NewVolumeWorker()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-stop
		volWorker.Stop()
		player.Stop()
		os.Exit(0)
	}()

	closedLoop := isClosedLoopEnabled()
	tvActEnabled := isTvActuatorEnabled()
	initialDelay := getInitialDelayMs()
	ctrl := NewSyncController(initialDelay, closedLoop, tvActEnabled)
	acoustic := NewAcousticEstimator()
	acoustic.StartPassiveDmaReader()

	fmt.Fprintf(os.Stderr, "[station-rx] v3.0 started on UDP4 %s | closed_loop=%t tv_actuator=%t init_delay=%d ms (H_min=%.0f ms, Q_min=%.0f ms)\n",
		addr.String(), closedLoop, tvActEnabled, initialDelay, defaultAlsaBaseMs, defaultMinQMs)

	var (
		rxPkts         uint64
		droppedPkts    uint64
		appStarvation  uint64
		plcFrames      uint64
		silenceRelease uint64
		resyncEvents   uint64
		queuedBytes    int64
		lastPacketNano int64
		atomicIsMuted  int64
		atomicTvVol    int64 = 5
		playbackEpoch  uint64
		acquireActive  int32 // 1 while in ACQUIRE/probe (playback paused silently)
		muteFadeReq    int32 // 1 to trigger immediate 20 ms fade-out & stop
	)

	queue := make(chan queuedFrame, 400)
	enqueue := func(f queuedFrame) {
		// Drop oldest if queue exceeds target + 180 ms
		maxBytes := int64((ctrl.TargetQueueMs() + 180.0) * 192.0)
		for atomic.LoadInt64(&queuedBytes) > maxBytes {
			select {
			case old := <-queue:
				atomic.AddInt64(&queuedBytes, -int64(len(old.pcm)))
				atomic.AddUint64(&droppedPkts, 1)
			default:
				goto pushNow
			}
		}
	pushNow:
		select {
		case queue <- f:
			atomic.AddInt64(&queuedBytes, int64(len(f.pcm)))
		default:
			atomic.AddUint64(&droppedPkts, 1)
		}
	}
	drainQueue := func() {
		for {
			select {
			case old := <-queue:
				atomic.AddInt64(&queuedBytes, -int64(len(old.pcm)))
			default:
				return
			}
		}
	}

	var tvAddrMu sync.Mutex
	var lastTvAddr *net.UDPAddr
	configuredPeer := getConfiguredPeerIP()

	sendTelemetry := func(cmdSeq uint8) {
		tvAddrMu.Lock()
		dst := lastTvAddr
		tvAddrMu.Unlock()
		if dst == nil {
			return
		}
		qMs := float64(atomic.LoadInt64(&queuedBytes)+int64(player.PipeQueuedBytes())) / 192.0
		snap := ctrl.Snapshot(qMs, time.Now())

		var flags uint8
		if snap.ClosedLoop {
			flags |= sak2FlagClosedLoop
		}
		if snap.LastValidAgeMs >= 0 && snap.LastValidAgeMs < 15000 {
			flags |= sak2FlagValid
		}
		if snap.CacheUsed {
			flags |= sak2FlagCacheUsed
		}

		sak2 := EncodeSak2(Sak2Packet{
			State:          snap.State,
			Flags:          flags,
			ReqTvDelayMs:   uint16(snap.ReqDtvMs),
			StationDelayMs: uint16(snap.DstMs),
			LtvTenths:      uint16(math.Max(0, math.Round(snap.LtvMs*10))),
			ErrTenths:      int16(math.Round(snap.ErrMs * 10)),
			ConfPermille:   uint16(math.Round(snap.Conf * 1000)),
			CmdSeq:         cmdSeq,
		})
		_, _ = conn.WriteToUDP(sak2[:], dst)

		// Also send legacy SACK for Android v2 / legacy clients
		legacy := EncodeLegacySack(
			uint16(snap.ReqDtvMs),
			uint16(snap.DstMs),
			int16(math.Round(snap.ErrMs*10)),
			uint16(math.Round(snap.Conf*1000)),
		)
		_, _ = conn.WriteToUDP(legacy[:], dst)
	}

	// Trigger silent ACQUIRE sequence (keeps speaker silent 0.4..2.0s to measure clean L_tv)
	acquireTrigger := make(chan string, 4)
	triggerAcquire := func(reason string) {
		select {
		case acquireTrigger <- reason:
		default:
		}
	}

	// 1. Standby & Watchdog Goroutine
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		for range ticker.C {
			player.recoverBlockedWrite()
			last := atomic.LoadInt64(&lastPacketNano)
			if last == 0 {
				continue
			}
			gapMs := (time.Now().UnixNano() - last) / int64(time.Millisecond)
			if gapMs > 1500 && (player.IsRunning() || ctrl.State() != StateIdle) {
				fmt.Fprintf(os.Stderr, "[station-rx] Standby/silence (%d ms without packets) -> releasing ALSA for Alice.\n", gapMs)
				atomic.StoreInt32(&acquireActive, 0)
				player.Stop()
				drainQueue()
				ctrl.EnterIdle("standby_timeout")
				atomic.AddUint64(&silenceRelease, 1)
			}
		}
	}()

	// 2. ACQUIRE + TRACK Acoustic Controller Goroutine (also handles cmd_probe & cmd_step)
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		for {
			select {
			case reason := <-acquireTrigger:
				if atomic.LoadInt64(&atomicIsMuted) != 0 {
					continue
				}
				atomic.StoreInt32(&acquireActive, 1)
				player.Stop()
				drainQueue()
				acoustic.ResetAccumulator()
				ctrl.EnterAcquire(reason)
				sendTelemetry(0)
				fmt.Fprintf(os.Stderr, "[acquire] Started silent capture (%s)\n", reason)

				deadline := time.Now().Add(2000 * time.Millisecond)
				// Wait minimum 450 ms to accumulate clean audio before first FFT
				time.Sleep(450 * time.Millisecond)
				joined := false
				for time.Now().Before(deadline) {
					if atomic.LoadInt64(&atomicIsMuted) != 0 {
						break
					}
					last := atomic.LoadInt64(&lastPacketNano)
					if last > 0 && (time.Now().UnixNano()-last) > int64(1200*time.Millisecond) {
						break
					}
					res := acoustic.AnalyzeCleanTvLag(800, true, 6.0, 1.15)
					fmt.Fprintf(os.Stderr, "[acquire-check] ltv=%.2f psr=%.2f contrast=%.2f rms_ref=%.4f rms_mic=%.5f status=%s\n",
						res.TvLagMs, res.TvPsr, res.Contrast, res.RefRms, res.MicRms, res.Status)
					if ok, newDst, _, cmdSeq := ctrl.HandleAcquireResult(res, time.Now()); ok {
						saveCalibratedDelay(newDst)
						sendTelemetry(cmdSeq)
						joined = true
						break
					}
					time.Sleep(250 * time.Millisecond)
				}
				if !joined && atomic.LoadInt64(&atomicIsMuted) == 0 {
					newDst, _, cmdSeq := ctrl.HandleAcquireTimeout(time.Now())
					saveCalibratedDelay(newDst)
					sendTelemetry(cmdSeq)
				}
				drainQueue()
				atomic.StoreInt32(&acquireActive, 0)
				fmt.Fprintf(os.Stderr, "[acquire] Complete -> JOIN (dst=%d ms, state=%s)\n", ctrl.TargetDelayMs(), ctrl.State())

			case <-ticker.C:
				// Check manual diagnostic probe trigger: /data/audiosync/cmd_probe
				if data, err := os.ReadFile("/data/audiosync/cmd_probe"); err == nil {
					_ = os.Remove("/data/audiosync/cmd_probe")
					durMs := 2000
					if v, e := strconv.Atoi(strings.TrimSpace(string(data))); e == nil && v >= 500 && v <= 5000 {
						durMs = v
					}
					atomic.StoreInt32(&acquireActive, 1)
					player.Stop()
					acoustic.ResetAccumulator()
					time.Sleep(time.Duration(durMs) * time.Millisecond)
					res := acoustic.AnalyzeCleanTvLag(min(durMs, 1500), false, 5.0, 1.10)
					fmt.Fprintf(os.Stderr, "[probe] ltv=%.2f psr=%.2f contrast=%.2f peak_idx=%d rms_ref=%.5f rms_mic=%.5f status=%s\n",
						res.TvLagMs, res.TvPsr, res.Contrast, res.PeakIdx, res.RefRms, res.MicRms, res.Status)
					atomic.StoreInt32(&acquireActive, 0)
					continue
				}

				// Check manual step trigger: /data/audiosync/cmd_step
				if data, err := os.ReadFile("/data/audiosync/cmd_step"); err == nil {
					_ = os.Remove("/data/audiosync/cmd_step")
					if step, e := strconv.Atoi(strings.TrimSpace(string(data))); e == nil {
						oldD := ctrl.TargetDelayMs()
						ctrl.SetStationDelayManual(oldD + step)
						saveCalibratedDelay(ctrl.TargetDelayMs())
						fmt.Fprintf(os.Stderr, "[diag-cmd] MANUAL STEP: %+d ms (%d -> %d ms)\n", step, oldD, ctrl.TargetDelayMs())
					}
				}

				ctrl.SetClosedLoop(isClosedLoopEnabled())
				ctrl.SetTvActuatorEnabled(isTvActuatorEnabled())

				if !player.IsRunning() || atomic.LoadInt32(&acquireActive) != 0 || atomic.LoadInt64(&atomicIsMuted) != 0 || atomic.LoadInt64(&atomicTvVol) < 1 {
					continue
				}

				res := acoustic.AnalyzeTrackSync(1000, float64(ctrl.TargetDelayMs()))
				if res.SpkConf >= 0.65 && res.SpkLagMs > 10.0 {
					curQMs := float64(atomic.LoadInt64(&queuedBytes)+int64(player.PipeQueuedBytes())) / 192.0
					ctrl.UpdateMeasuredAlsaBase(res.SpkLagMs, curQMs)
				}
				reacq, newDst, _, cmdSeq := ctrl.HandleTrackResult(res, time.Now())
				if res.Valid || res.SpkConf >= 0.55 {
					fmt.Fprintf(os.Stderr, "[track] epoch=%d spk=%.1f tv=%.1f err=%.1f psr=%.1f contrast=%.2f status=%s dst=%d\n",
						atomic.LoadUint64(&playbackEpoch), res.SpkLagMs, res.TvLagMs, res.AcousticErr, res.TvPsr, res.Contrast, res.Status, newDst)
				}
				saveCalibratedDelay(newDst)
				sendTelemetry(cmdSeq)
				if reacq {
					triggerAcquire("track_large_shift")
				}
			}
		}
	}()

	// 3. Periodic SAK2 / SACK Telemetry Keepalive (every 500 ms)
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		for range ticker.C {
			if player.IsRunning() || atomic.LoadInt32(&acquireActive) != 0 {
				sendTelemetry(0)
			}
		}
	}()

	// 4. UDP Receiver Goroutine (SYN3 + legacy SYNC + TVAP + Peer IP validation + ReorderBuffer)
	reorder := NewReorderBuffer(3)
	go func() {
		buf := make([]byte, 4096)
		var peerIP string = configuredPeer
		var lastPeerTime time.Time

		for {
			n, srcAddr, err := conn.ReadFromUDP(buf)
			if err != nil || n < 4 || srcAddr == nil {
				continue
			}

			// Peer IP validation: only accept packets from paired TV IP (resets after 15s idle if unconfigured)
			srcIP := srcAddr.IP.String()
			if configuredPeer != "" && srcIP != configuredPeer {
				continue
			}
			if configuredPeer == "" {
				if peerIP == "" || time.Since(lastPeerTime) > 15*time.Second {
					peerIP = srcIP
				} else if srcIP != peerIP {
					continue
				}
			}
			lastPeerTime = time.Now()

			// Check TVAP control packet
			if tvap, ok := ParseTvapPacket(buf[:n]); ok {
				ctrl.HandleTvap(tvap)
				fmt.Fprintf(os.Stderr, "[tvap] applied=%d ms status=%d seq=%d\n", tvap.AppliedTvDelayMs, tvap.Status, tvap.CmdSeq)
				continue
			}

			pkt, ok := ParseAudioPacket(buf[:n])
			if !ok {
				continue
			}

			tvAddrMu.Lock()
			lastTvAddr = srcAddr
			tvAddrMu.Unlock()

			nowNano := time.Now().UnixNano()
			prevNano := atomic.SwapInt64(&lastPacketNano, nowNano)
			wasIdle := prevNano == 0 || (nowNano-prevNano) > int64(1500*time.Millisecond)
			atomic.AddUint64(&rxPkts, 1)

			// Update volume via serialized worker
			atomic.StoreInt64(&atomicTvVol, int64(pkt.TvVol))
			volWorker.Set(pkt.TvVol, pkt.StationVolPct, pkt.IsMuted)

			// Instant Mute handling (do not wait for 1.5s standby timeout!)
			if pkt.IsMuted {
				if atomic.SwapInt64(&atomicIsMuted, 1) == 0 {
					atomic.StoreInt32(&muteFadeReq, 1)
					fmt.Fprintf(os.Stderr, "[station-rx] Instant Mute received -> fade-out & pause\n")
				}
				continue
			} else {
				if atomic.SwapInt64(&atomicIsMuted, 0) != 0 {
					wasIdle = true
				}
			}

			if wasIdle {
				reorder.Reset()
				atomic.AddUint64(&playbackEpoch, 1)
				triggerAcquire("stream_start")
			}

			ready, majorReset := reorder.Push(pkt)
			if majorReset && !wasIdle {
				atomic.AddUint64(&resyncEvents, 1)
				atomic.AddUint64(&playbackEpoch, 1)
				drainQueue()
				triggerAcquire("major_seek")
			}

			for _, rp := range ready {
				if rp.IsPLC {
					atomic.AddUint64(&plcFrames, 1)
				}
				// Feed monotonic reference ring (PLC frames marked so correlator ignores them)
				acoustic.FeedUdpReferenceChunk48k(rp.PCM, rp.IsPLC)

				// Only enqueue for playback when not in silent ACQUIRE
				if atomic.LoadInt32(&acquireActive) == 0 {
					enqueue(queuedFrame{pcm: rp.PCM, frameMs: rp.FrameMs, isPLC: rp.IsPLC})
				}
			}
		}
	}()

	// 5. Honest Atomic Telemetry Logger (every 5 seconds)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		for range ticker.C {
			rx := atomic.LoadUint64(&rxPkts)
			if rx == 0 {
				continue
			}
			qMs := float64(atomic.LoadInt64(&queuedBytes)+int64(player.PipeQueuedBytes())) / 192.0
			snap := ctrl.Snapshot(qMs, time.Now())
			var validRate float64
			if snap.TotalChecks > 0 {
				validRate = float64(snap.ValidCount) / float64(snap.TotalChecks)
			}
			fmt.Fprintf(os.Stderr, "[tel] state=%s cl=%t ltv=%.1f lst=%.1f err=%+.1f conf=%.2f age_ms=%d vrate=%.2f dst=%d dtv=%d q_ms=%.1f drift=%.0fppm | rx=%d drop=%d xrun=%d starv=%d plc=%d loss=%d reord=%d rel=%d reason=%s\n",
				snap.State, snap.ClosedLoop, snap.LtvMs, snap.LstMs, snap.ErrMs, snap.Conf, snap.LastValidAgeMs,
				validRate, snap.DstMs, snap.DtvMs, snap.QMs, snap.DriftPpm,
				rx, atomic.LoadUint64(&droppedPkts), player.AlsaXruns(), atomic.LoadUint64(&appStarvation),
				atomic.LoadUint64(&plcFrames), reorder.LossCount, reorder.ReorderCnt, atomic.LoadUint64(&silenceRelease),
				snap.Reason)
		}
	}()

	// 6. Main Playback Loop with Seamless Fade-In JOIN, Instant Mute Fade-Out, and Separated PLL / Delay Slew
	silence10ms := make([]byte, 1920) // 10 ms stereo S16_LE (matches dmix 480-frame period -> 0 tail XRUNs)
	retimeBuf := make([]byte, 4096)

	for {
		// Wait while idle, muted, or in silent ACQUIRE
		if atomic.LoadInt32(&acquireActive) != 0 || atomic.LoadInt64(&atomicIsMuted) != 0 {
			if player.IsRunning() {
				player.Stop()
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}

		targetQMs := ctrl.TargetQueueMs()
		primeBytes := int64(math.Max(15.0, targetQMs) * 192.0) // 192 bytes/ms @ 48kHz stereo S16_LE

		for atomic.LoadInt64(&queuedBytes) < primeBytes {
			if atomic.LoadInt32(&acquireActive) != 0 || atomic.LoadInt64(&atomicIsMuted) != 0 {
				break
			}
			time.Sleep(3 * time.Millisecond)
			targetQMs = ctrl.TargetQueueMs()
			primeBytes = int64(math.Max(15.0, targetQMs) * 192.0)
		}
		if atomic.LoadInt32(&acquireActive) != 0 || atomic.LoadInt64(&atomicIsMuted) != 0 {
			continue
		}

		// Discard stale frames above target primeBytes so we JOIN on-time
		for atomic.LoadInt64(&queuedBytes) > primeBytes+1920 {
			select {
			case old := <-queue:
				atomic.AddInt64(&queuedBytes, -int64(len(old.pcm)))
				atomic.AddUint64(&droppedPkts, 1)
			default:
				goto startAplay
			}
		}

	startAplay:
		last := atomic.LoadInt64(&lastPacketNano)
		if last == 0 || (time.Now().UnixNano()-last) > int64(1500*time.Millisecond) {
			drainQueue()
			continue
		}

		if err := player.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "[station-rx] aplay start failed: %v\n", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		ctrl.MarkJoined()
		fmt.Fprintf(os.Stderr, "[station-rx] aplay started (pid %d) state=%s dst=%d ms targetQ=%.1f ms\n",
			player.pid, ctrl.State(), ctrl.TargetDelayMs(), ctrl.TargetQueueMs())

		emaQMs := ctrl.TargetQueueMs()
		lastPiTime := time.Now()
		fadeInMsLeft := 40.0 // 40 ms linear fade-in on JOIN
		timer := time.NewTimer(25 * time.Millisecond)

		for player.IsRunning() {
			if atomic.LoadInt32(&acquireActive) != 0 {
				player.Stop()
				break
			}
			if atomic.SwapInt32(&muteFadeReq, 0) != 0 || atomic.LoadInt64(&atomicIsMuted) != 0 {
				// Fast 20 ms fade-out on next available chunk or silence, then stop ALSA immediately
				select {
				case f := <-queue:
					atomic.AddInt64(&queuedBytes, -int64(len(f.pcm)))
					_ = player.Write(applyFade(f.pcm, 1.0, 0.0))
				default:
				}
				player.Stop()
				drainQueue()
				ctrl.EnterIdle("instant_mute")
				break
			}

			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(25 * time.Millisecond)

			select {
			case f := <-queue:
				atomic.AddInt64(&queuedBytes, -int64(len(f.pcm)))
				targetQMs = ctrl.TargetQueueMs()
				curQMs := float64(atomic.LoadInt64(&queuedBytes)+int64(player.PipeQueuedBytes())) / 192.0
				emaQMs = 0.04*curQMs + 0.96*emaQMs

				errQMs := emaQMs - targetQMs
				now := time.Now()
				driftPpm := ctrl.UpdateClockDriftPI(errQMs, now.Sub(lastPiTime).Seconds())
				lastPiTime = now

				// Separated Delay Slew (up to ±1.5% rate-nudge) + Clock Drift PI (±200 ppm)
				inFrames := len(f.pcm) / 4
				outFrames := inFrames
				if inFrames >= 48 {
					var rateDelta float64
					// Clock drift component (±0.0002)
					rateDelta -= driftPpm * 1e-6
					// Delay slew component for |errQMs| > 3 ms (up to ±1.5%)
					if math.Abs(errQMs) > 3.0 {
						slew := math.Max(-0.015, math.Min(0.015, errQMs*0.0008))
						rateDelta -= slew
					}
					deltaFrames := int(math.Round(float64(inFrames) * rateDelta))
					if deltaFrames != 0 {
						outFrames = inFrames + deltaFrames
					}
				}

				out := f.pcm
				if outFrames != inFrames {
					out = retimePCM(f.pcm, retimeBuf, inFrames, outFrames)
				}

				if fadeInMsLeft > 0 {
					startG := (40.0 - fadeInMsLeft) / 40.0
					fadeInMsLeft -= f.frameMs
					if fadeInMsLeft < 0 {
						fadeInMsLeft = 0
					}
					endG := (40.0 - fadeInMsLeft) / 40.0
					out = applyFade(out, startG, endG)
				}

				if err := player.Write(out); err != nil {
					fmt.Fprintf(os.Stderr, "[station-rx] aplay write failed: %v\n", err)
					player.Stop()
					break
				}

			case <-timer.C:
				if err := player.Write(silence10ms); err != nil {
					player.Stop()
					break
				}
				atomic.AddUint64(&appStarvation, 1)
			}
		}

		timer.Stop()
		player.Stop()
		time.Sleep(15 * time.Millisecond)
	}
}
