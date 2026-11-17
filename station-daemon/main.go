package main

import (
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
	headerMagic   = 0x53594E43 // 'SYNC'
	ackMagic      = 0x5341434B // 'SACK' (Station Acoustic Control Packet -> TV)
	chunkPcmLen   = 1920       // 480 stereo frames = 10.0 ms at 48000 Hz S16_LE
	fcntlSetPipe  = 1031       // F_SETPIPE_SZ on Linux
	ioctlFionread = 0x541B     // FIONREAD on Linux

	// Verified hardware + dmix + kernel pipe base latency on Yandex Station Mini 2:
	// With period_size=960 (20 ms) and buffer_size=3840 (80 ms) in ALSA,
	// total hardware/driver playback path is ~100 ms.
	// Calibrated nominal Station delay is 148 ms (4.8 chunks in Go queue + 100 ms ALSA/pipe),
	// which provides a rock-solid 48 ms jitter cushion with 0 stalls, 0 drops, and 0 underruns,
	// while naturally matching the TV's acoustic latency (MediaTek Audio HAL + Dolby Atmos + air travel).
	alsaBaseMs   = 70.0
	minStationMs = 70  // Physical minimum stable latency of Station (4096B pipe + 2880 buffer)
	maxStationMs = 950 // Maximum allowed Station buffer delay (supports deep-buffer streaming & video)
	geomAirMs    = 7.0 // Acoustic time-of-flight from TV to Station microphones (~2.4 meters)
)

func getInitialDelayMs() int {
	target := 218 // Default matching measured TV video player latency (YouTube / Vivax Audio HAL ~222 ms)
	data, err := os.ReadFile("/data/audiosync/delay_ms")
	if err == nil {
		str := strings.TrimSpace(string(data))
		if val, err := strconv.Atoi(str); err == nil && val >= minStationMs && val <= maxStationMs {
			target = val
		}
	}
	return target
}

func saveCalibratedDelay(delayMs int) {
	if delayMs >= minStationMs && delayMs <= maxStationMs {
		_ = os.WriteFile("/data/audiosync/delay_ms", []byte(strconv.Itoa(delayMs)), 0644)
	}
}

func getGeomAirMs() float64 {
	data, err := os.ReadFile("/data/audiosync/geom_ms")
	if err == nil {
		str := strings.TrimSpace(string(data))
		if val, err := strconv.ParseFloat(str, 64); err == nil && val >= -20.0 && val <= 30.0 {
			return val
		}
	}
	return geomAirMs
}

func isObservationMode() bool {
	// If explicit auto_control file exists:
	// "1", "true", or "closed_loop" -> Observation mode OFF (closed loop active)
	// "0", "false", or "observe" -> Observation mode ON
	if data, err := os.ReadFile("/data/audiosync/auto_control"); err == nil {
		str := strings.TrimSpace(string(data))
		if str == "1" || strings.EqualFold(str, "true") || strings.EqualFold(str, "closed_loop") {
			return false
		}
		return true
	}
	// Default for Phase 1: Observation Mode is ACTIVE (closed-loop adjustment frozen)
	return true
}

func isAcousticAutoEnabled() bool {
	if isObservationMode() {
		return false
	}
	data, err := os.ReadFile("/data/audiosync/acoustic_auto")
	if err != nil {
		return true
	}
	str := strings.TrimSpace(string(data))
	return str != "0" && !strings.EqualFold(str, "false")
}

func checkManualStep(curStationMs int, setStationDelay func(int)) {
	// 1. Manual Step: /data/audiosync/cmd_step
	if data, err := os.ReadFile("/data/audiosync/cmd_step"); err == nil {
		_ = os.Remove("/data/audiosync/cmd_step")
		str := strings.TrimSpace(string(data))
		if step, err := strconv.Atoi(str); err == nil {
			newDelay := curStationMs + step
			if newDelay < minStationMs {
				newDelay = minStationMs
			} else if newDelay > maxStationMs {
				newDelay = maxStationMs
			}
			setStationDelay(newDelay)
			fmt.Fprintf(os.Stderr, "[diag-cmd] MANUAL STEP APPLIED: step=%+d ms, delay %d -> %d ms\n", step, curStationMs, newDelay)
		} else {
			fmt.Fprintf(os.Stderr, "[diag-cmd] MANUAL STEP INVALID: '%s': %v\n", str, err)
		}
	}

}

type Player struct {
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

	cmd := exec.Command("/usr/bin/aplay", "-D", "default", "--buffer-size=2880", "--period-size=960", "-R", "20000", "-t", "raw", "-f", "S16_LE", "-r", "48000", "-c", "2", "-q", "-")
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}

	var fd uintptr
	if file, ok := stdin.(*os.File); ok {
		fd = file.Fd()
		_, _, _ = syscall.Syscall(syscall.SYS_FCNTL, fd, fcntlSetPipe, 4096)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		return err
	}

	p.cmd = cmd
	p.stdin = stdin
	p.pipeFd = fd
	p.pid = cmd.Process.Pid
	p.running = true
	return nil
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

// shrinkChunk479 smoothly compresses 480 stereo frames (1920 B) into 479 stereo frames (1916 B)
// by blending frames 239 and 240 (-0.0208 ms per chunk, inaudible 0.2% slew).
func shrinkChunk479(src []byte, dst []byte) []byte {
	if len(src) != chunkPcmLen {
		return src
	}
	copy(dst[0:956], src[0:956])
	l0 := int32(int16(binary.LittleEndian.Uint16(src[956:958])))
	r0 := int32(int16(binary.LittleEndian.Uint16(src[958:960])))
	l1 := int32(int16(binary.LittleEndian.Uint16(src[960:962])))
	r1 := int32(int16(binary.LittleEndian.Uint16(src[962:964])))
	binary.LittleEndian.PutUint16(dst[956:958], uint16(int16((l0+l1)/2)))
	binary.LittleEndian.PutUint16(dst[958:960], uint16(int16((r0+r1)/2)))
	copy(dst[960:1916], src[964:1920])
	return dst[0:1916]
}

// expandChunk481 smoothly expands 480 stereo frames (1920 B) into 481 stereo frames (1924 B)
// by linearly interpolating 1 midpoint stereo frame between frame 239 and 240 (+0.0208 ms per chunk).
func expandChunk481(src []byte, dst []byte) []byte {
	if len(src) != chunkPcmLen {
		return src
	}
	copy(dst[0:960], src[0:960])
	l0 := int32(int16(binary.LittleEndian.Uint16(src[956:958])))
	r0 := int32(int16(binary.LittleEndian.Uint16(src[958:960])))
	l1 := int32(int16(binary.LittleEndian.Uint16(src[960:962])))
	r1 := int32(int16(binary.LittleEndian.Uint16(src[962:964])))
	binary.LittleEndian.PutUint16(dst[960:962], uint16(int16((l0+l1)/2)))
	binary.LittleEndian.PutUint16(dst[962:964], uint16(int16((r0+r1)/2)))
	copy(dst[964:1924], src[960:1920])
	return dst[0:1924]
}

// Gradually change the queue using continuous PCM, with fixed block endpoints.
func retimeChunk(src, dst []byte, frames int) []byte {
	for j := 0; j < frames; j++ {
		pos := j * 479
		i := pos / (frames - 1)
		frac := pos % (frames - 1)
		next := i + 1
		if next > 479 {
			next = 479
		}
		for ch := 0; ch < 2; ch++ {
			a := int(int16(binary.LittleEndian.Uint16(src[i*4+ch*2:])))
			b := int(int16(binary.LittleEndian.Uint16(src[next*4+ch*2:])))
			binary.LittleEndian.PutUint16(dst[j*4+ch*2:], uint16(int16(a+(b-a)*frac/(frames-1))))
		}
	}
	return dst[:frames*4]
}

// targetQueueChunksForDelay returns the target number of 10 ms chunks in the Go queue.
// With alsaBaseMs = 100.0 ms (80 ms ALSA buffer + hardware/driver baseline):
// - delayMs = 148 ms -> 4.8 chunks in Go queue (~48 ms jitter cushion)
// - delayMs = 160 ms -> 6.0 chunks in Go queue
func targetQueueChunksForDelay(delayMs int) float64 {
	q := (float64(delayMs) - alsaBaseMs) / 10.0
	if q < 1.0 {
		q = 1.0
	}
	return q
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
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	go func() { <-stop; player.Stop(); os.Exit(0) }()
	acoustic := NewAcousticEstimator()
	acoustic.StartPassiveDmaReader()

	silenceChunk := make([]byte, chunkPcmLen)
	shrinkBuf := make([]byte, 1916)
	expandBuf := make([]byte, 1924)

	var atomicTargetDelay int64 = int64(getInitialDelayMs())
	var atomicTvSpeakerDelay int64 = 0
	var atomicTvVolume int64 = 8

	var atomicIsMuted int64 = 0
	var atomicTargetQBits uint64

	var rxPkts, droppedPkts, duplicatePkts, resyncEvents, pllShrinks, pllExpands uint64
	var silenceDebt int64
	queue := make(chan []byte, 240)

	var tvAddrMu sync.Mutex
	var lastTvAddr *net.UDPAddr

	setStationDelay := func(delayMs int) {
		if delayMs < minStationMs {
			delayMs = minStationMs
		}
		if delayMs > maxStationMs {
			delayMs = maxStationMs
		}
		q := targetQueueChunksForDelay(delayMs)
		atomic.StoreInt64(&atomicTargetDelay, int64(delayMs))
		atomic.StoreUint64(&atomicTargetQBits, *(*uint64)(unsafe.Pointer(&q)))
		saveCalibratedDelay(delayMs)
	}
	getTargetQ := func() float64 {
		bits := atomic.LoadUint64(&atomicTargetQBits)
		return *(*float64)(unsafe.Pointer(&bits))
	}
	setStationDelay(int(atomicTargetDelay))

	sendTvSpeakerDelay := func(tvSpkDelayMs int) {
		if tvSpkDelayMs < 0 {
			tvSpkDelayMs = 0
		}
		if tvSpkDelayMs > 160 {
			tvSpkDelayMs = 160
		}
		atomic.StoreInt64(&atomicTvSpeakerDelay, int64(tvSpkDelayMs))

		tvAddrMu.Lock()
		dst := lastTvAddr
		tvAddrMu.Unlock()
		if dst == nil {
			return
		}

		// 12-byte control packet back to TV's UDP socket:
		// 0..3: 'SACK' (0x5341434B)
		// 4..5: requested tvSpeakerDelayMs (uint16 BE)
		// 6..7: current stationTargetDelayMs (uint16 BE)
		// 8..9: last measured acoustic error in 0.1 ms (int16 BE)
		// 10..11: confidence 0..1000 (uint16 BE)
		var pkt [12]byte
		binary.BigEndian.PutUint32(pkt[0:4], ackMagic)
		binary.BigEndian.PutUint16(pkt[4:6], uint16(tvSpkDelayMs))
		binary.BigEndian.PutUint16(pkt[6:8], uint16(atomic.LoadInt64(&atomicTargetDelay)))
		binary.BigEndian.PutUint16(pkt[8:10], uint16(int16(atomic.LoadInt64(&acoustic.lastErrTenthsMs))))
		binary.BigEndian.PutUint16(pkt[10:12], uint16(atomic.LoadInt64(&acoustic.lastConfidence)))
		_, _ = conn.WriteToUDP(pkt[:], dst)
	}

	fmt.Fprintf(os.Stderr, "[station-rx] Started Universal Acoustic Sync on UDP4 %s (Initial Station delay: %d ms, targetQ: %.1f)\n",
		addr.String(), atomic.LoadInt64(&atomicTargetDelay), getTargetQ())

	var lastPacketNano int64
	var lastSeq uint32
	var playbackEpoch atomic.Uint64

	var lastStationVol int = -1
	var lastMuted bool = false
	var hasVol bool = false

	// 1. TV Standby Watchdog (1500 ms timeout)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		for range ticker.C {
			player.recoverBlockedWrite()
			last := atomic.LoadInt64(&lastPacketNano)
			if last == 0 {
				continue
			}
			gapMs := (time.Now().UnixNano() - last) / int64(time.Millisecond)
			if gapMs > 1500 && player.IsRunning() {
				fmt.Fprintf(os.Stderr, "[station-rx] TV standby detected (%d ms without packets). Releasing ALSA device.\n", gapMs)
				player.Stop()
				for len(queue) > 0 {
					select {
					case <-queue:
					default:
					}
				}
				atomic.StoreInt64(&silenceDebt, 0)

			}
		}
	}()

	// 2. Closed-Loop Autonomous Universal Acoustic Controller (Zero Profiles, Zero App Corridors)
	go func() {
		var evidence [6]float64
		var nEvidence, head int
		var settleUntil time.Time
		var seenEpoch uint64
		for range time.NewTicker(700 * time.Millisecond).C {
			epoch := playbackEpoch.Load()
			if epoch != seenEpoch {
				nEvidence = 0
				settleUntil = time.Now().Add(900 * time.Millisecond)
				seenEpoch = epoch
			}
			if !player.IsRunning() || atomic.LoadInt64(&atomicTvVolume) < 1 || atomic.LoadInt64(&atomicIsMuted) != 0 || time.Now().Before(settleUntil) {
				nEvidence = 0
				continue
			}
			target := int(atomic.LoadInt64(&atomicTargetDelay))
			checkManualStep(target, setStationDelay)
			target = int(atomic.LoadInt64(&atomicTargetDelay))
			r := acoustic.AnalyzeRoomSync(1000, float64(target))
			if r.Valid {
				fmt.Fprintf(os.Stderr, "[measure] epoch=%d tv=%.1f station=%.1f error=%.1f quality=%.2f psr=%.1f contrast=%.2f mag=%.4f valid=%t target=%d\n", epoch, r.TvLagMs, r.SpkLagMs, r.AcousticErr, r.TvConf, r.TvPsr, r.Ratio, r.TvMag, r.Valid, target)
			}
			if !isAcousticAutoEnabled() || epoch != playbackEpoch.Load() {
				nEvidence = 0
				continue
			}
			sample := math.NaN()
			if r.Valid && math.Abs(r.AcousticErr) > 8 {
				sample = r.AcousticErr
			}
			evidence[head] = sample
			head = (head + 1) % len(evidence)
			if nEvidence < len(evidence) {
				nEvidence++
			}
			if math.IsNaN(sample) {
				continue
			}
			var votes int
			var total float64
			for k := 0; k < nEvidence; k++ {
				v := evidence[(head+len(evidence)-1-k)%len(evidence)]
				if math.Abs(v-sample) <= 9 {
					votes++
					total += v
				}
			}
			if votes < 3 {
				continue
			}
			prior := total / float64(votes)
			// A physical mismatch beyond both actuators cannot justify a jump to a rail.
			if float64(target)+prior < 80-12 || float64(target)+prior > maxStationMs+12 {
				continue
			}
			step := math.Max(-60, math.Min(60, prior))
			desired := target + int(math.Round(step))
			if desired < 80 {
				desired = 80
			}
			if desired > maxStationMs {
				desired = maxStationMs
			}
			if desired == target {
				continue
			}
			setStationDelay(desired)
			fmt.Fprintf(os.Stderr, "[confirmed] epoch=%d target=%d -> %d measured=%.1f\n", epoch, target, desired, prior)
			nEvidence = 0
			settleUntil = time.Now().Add(1600 * time.Millisecond)
		}
	}()

	// 2c. Periodic keepalive of TV speaker delay every 2 seconds so TV stays in sync
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		for range ticker.C {
			if player.IsRunning() {
				requested := 0
				if data, err := os.ReadFile("/tmp/audiosync-tv-delay-test"); err == nil {
					fields := strings.Fields(string(data))
					if len(fields) == 2 {
						ms, e1 := strconv.Atoi(fields[0])
						until, e2 := strconv.ParseInt(fields[1], 10, 64)
						remaining := until - time.Now().Unix()
						if e1 == nil && e2 == nil && ms >= 0 && ms <= 80 && remaining > 0 && remaining <= 60 {
							requested = ms
						}
					}
				}
				sendTvSpeakerDelay(requested)
			}
		}
	}()

	// 3. UDP Receiver Goroutine
	go func() {

		quietPackets := 0
		hasSeq := false
		buf := make([]byte, 4096)
		for {
			n, srcAddr, err := conn.ReadFromUDP(buf)
			if err != nil || n <= 0 {
				continue
			}
			nowNano := time.Now().UnixNano()
			if nowNano-atomic.SwapInt64(&lastPacketNano, nowNano) > int64(1500*time.Millisecond) {
				hasSeq = false
				playbackEpoch.Add(1)
			}
			atomic.AddUint64(&rxPkts, 1)

			if srcAddr != nil {
				tvAddrMu.Lock()
				lastTvAddr = srcAddr
				tvAddrMu.Unlock()
			}

			var pcm []byte
			var isMajorSeek bool

			if n >= 16+chunkPcmLen && binary.BigEndian.Uint32(buf[0:4]) == headerMagic {
				seq := binary.BigEndian.Uint32(buf[4:8])
				tvVol := int(buf[12] & 0x7f)
				stationVol := int(buf[13])
				isMuted := buf[14] != 0

				atomic.StoreInt64(&atomicTvVolume, int64(tvVol))

				if isMuted {
					atomic.StoreInt64(&atomicIsMuted, 1)
				} else {
					atomic.StoreInt64(&atomicIsMuted, 0)
				}

				pcm = make([]byte, n-16)
				copy(pcm, buf[16:n])

				if !hasVol || stationVol != lastStationVol || isMuted != lastMuted {
					lastStationVol = stationVol
					lastMuted = isMuted
					hasVol = true

					targetPct := stationVol * 10
					if isMuted || stationVol == 0 {
						targetPct = 0
					}
					if targetPct > 100 {
						targetPct = 100
					}

					go func(tv, st, pct int, muted bool) {
						_ = exec.Command("amixer", "set", "Master", fmt.Sprintf("%d%%", pct)).Run()
						if muted {
							fmt.Fprintf(os.Stderr, "[station-rx] Volume sync: TV MUTED -> Station Muted (0%%)\n")
						} else {
							fmt.Fprintf(os.Stderr, "[station-rx] Volume sync: TV %d -> Station %d (%d%%)\n", tv, st, pct)
						}
					}(tvVol, stationVol, targetPct, isMuted)
				}

				if hasSeq {
					seqDiff := int64(seq) - int64(lastSeq)
					if seqDiff > 50 || seqDiff < -5 {
						isMajorSeek = true
					}
				}
				lastSeq = seq
				hasSeq = true
			} else {
				pcm = make([]byte, n)
				copy(pcm, buf[:n])
			}

			// Record reference audio monotonically!
			acoustic.FeedUdpReferenceChunk48k(pcm)
			var peak int
			for i := 0; i+1 < len(pcm); i += 2 {
				v := int(int16(binary.LittleEndian.Uint16(pcm[i:])))
				if v < 0 {
					v = -v
				}
				if v > peak {
					peak = v
				}
			}
			if peak <= 12 {
				if quietPackets < 1000 {
					quietPackets++
				}
			} else {
				if quietPackets >= 12 || isMajorSeek || !hasSeq {
					playbackEpoch.Add(1)
				}
				quietPackets = 0
			}

			if isMajorSeek && player.IsRunning() {
				atomic.AddUint64(&resyncEvents, 1)
				atomic.StoreInt64(&silenceDebt, 0)
				targetQ := int(getTargetQ() + 0.5)
				if targetQ < 8 {
					targetQ = 8
				}
				for len(queue) > targetQ {
					select {
					case <-queue:
					default:
						break
					}
				}
				fmt.Fprintf(os.Stderr, "[station-rx] Major seek detected. Snapped queue to %d chunks.\n", len(queue))
			}

			maxCap := int(getTargetQ()) + 45
			if len(queue) >= maxCap {
				select {
				case <-queue:
					atomic.AddUint64(&droppedPkts, 1)
				default:
				}
			}

			select {
			case queue <- pcm:
			default:
			}
		}
	}()

	// 4. Status Logger Goroutine
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		for range ticker.C {
			rx := atomic.LoadUint64(&rxPkts)
			if rx > 0 {
				dp := atomic.LoadUint64(&droppedPkts)
				dup := atomic.LoadUint64(&duplicatePkts)
				rs := atomic.LoadUint64(&resyncEvents)
				sh := atomic.LoadUint64(&pllShrinks)
				ex := atomic.LoadUint64(&pllExpands)
				pipeChunks := float64(player.PipeQueuedBytes()) / float64(chunkPcmLen)
				dmaRx := atomic.LoadUint64(&acoustic.packetsRx)
				tvLag := float64(atomic.LoadInt64(&acoustic.lastTvLagTenthsMs)) / 10.0
				spkLag := float64(atomic.LoadInt64(&acoustic.lastSpkLagTenthsMs)) / 10.0
				errLag := float64(atomic.LoadInt64(&acoustic.lastErrTenthsMs)) / 10.0
				lastConf := float64(atomic.LoadInt64(&acoustic.lastConfidence)) / 1000.0
				micRms := float64(atomic.LoadInt64(&acoustic.lastMicRmsPermille)) / 1000.0
				spkRms := float64(atomic.LoadInt64(&acoustic.lastSpkRmsPermille)) / 1000.0
				fmt.Fprintf(os.Stderr, "[station-rx] Status: %d rx, %d drop, %d stall, %d resync, pll(-%d/+%d), qlen: %d (targetQ: %.1f, pipe: %.1f), st_delay: %d ms, tv_spk: %d ms | mic_dma: %d (rms: %.3f/%.3f, tv: %.1f ms, spk: %.1f ms, err: %.1f ms, conf: %.2f)\n",
					rx, dp, dup, rs, sh, ex, len(queue), getTargetQ(), pipeChunks,
					atomic.LoadInt64(&atomicTargetDelay), atomic.LoadInt64(&atomicTvSpeakerDelay),
					dmaRx, micRms, spkRms, tvLag, spkLag, errLag, lastConf)
			}
		}
	}()

	// 5. Main Playback Loop with Smooth Sub-Millisecond PLL & Zero Churn
	for {
		targetQ := getTargetQ()
		primeCount := int(targetQ + 1.0)
		if primeCount < 4 {
			primeCount = 4
		}
		for len(queue) < primeCount {
			time.Sleep(3 * time.Millisecond)
			targetQ = getTargetQ()
			primeCount = int(targetQ + 1.0)
			if primeCount < 4 {
				primeCount = 4
			}
		}

		last := atomic.LoadInt64(&lastPacketNano)
		if (time.Now().UnixNano()-last)/int64(time.Millisecond) > 2000 {
			for len(queue) > 0 {
				select {
				case <-queue:
				default:
				}
			}
			continue
		}

		for len(queue) > primeCount {
			select {
			case <-queue:
				atomic.AddUint64(&droppedPkts, 1)
			default:
			}
		}
		atomic.StoreInt64(&silenceDebt, 0)
		if err := player.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "[station-rx] aplay start failed: %v\n", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}

		targetQ = getTargetQ()
		emaQ := targetQ
		retimeBuf := make([]byte, (480+24)*4)
		fmt.Fprintf(os.Stderr, "[station-rx] aplay started (pid %d) locked at %d ms (targetQ: %.1f, initial qlen: %d)\n",
			player.pid, atomic.LoadInt64(&atomicTargetDelay), targetQ, len(queue))

		timer := time.NewTimer(35 * time.Millisecond)

		for player.IsRunning() {
			targetQ = getTargetQ()
			pipeChunks := float64(player.PipeQueuedBytes()) / float64(chunkPcmLen)
			curQ := float64(len(queue)) + pipeChunks

			// Paced drain if queue experiences large burst (>targetQ + 15 chunks = >150 ms backlog)
			if curQ > targetQ+15.0 {
				select {
				case <-queue:
					atomic.AddUint64(&droppedPkts, 1)
				default:
				}
			}

			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(35 * time.Millisecond)

			select {
			case chunk := <-queue:
				pipeChunks = float64(player.PipeQueuedBytes()) / float64(chunkPcmLen)
				curQ = float64(len(queue)) + pipeChunks
				emaQ = 0.03*curQ + 0.97*emaQ

				errQ := emaQ - targetQ
				out := chunk
				// Smooth sub-millisecond linear interpolation PLL (deadband: ±0.8 chunks = ±8.0 ms)
				if math.Abs(errQ) > 1.5 && len(chunk) == chunkPcmLen {
					frames := 480 - int(math.Round(math.Max(-24, math.Min(24, errQ*3))))
					out = retimeChunk(chunk, retimeBuf, frames)
				} else if errQ > 0.8 && len(chunk) == chunkPcmLen {
					out = shrinkChunk479(chunk, shrinkBuf)
					atomic.AddUint64(&pllShrinks, 1)
				} else if errQ < -0.8 && len(chunk) == chunkPcmLen {
					out = expandChunk481(chunk, expandBuf)
					atomic.AddUint64(&pllExpands, 1)
				}

				if err := player.Write(out); err != nil {
					fmt.Fprintf(os.Stderr, "[station-rx] aplay write failed: %v\n", err)
					player.Stop()
					break
				}
			case <-timer.C:
				if err := player.Write(silenceChunk); err != nil {
					player.Stop()
					break
				}
				atomic.AddUint64(&duplicatePkts, 1)
			}
		}

		timer.Stop()
		player.Stop()
		time.Sleep(20 * time.Millisecond)
	}
}
