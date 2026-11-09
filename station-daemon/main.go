package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	headerMagic = 0x53594E43 // 'SYNC'
	chunkPcmLen = 1920       // 10 ms of 48000Hz 16-bit stereo
)

func getTargetDelayMs() int {
	target := 210 // Calibrated acoustic sweet spot (210 ms)
	data, err := os.ReadFile("/data/audiosync/delay_ms")
	if err == nil {
		str := strings.TrimSpace(string(data))
		if val, err := strconv.Atoi(str); err == nil && val >= 30 && val <= 500 {
			target = val
		}
	}
	return target
}

type Player struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	pid     int
	running bool
}

func (p *Player) Start() error {
	p.Stop()

	p.mu.Lock()
	defer p.mu.Unlock()

	// -B 20000: 20 ms hardware ALSA buffer, -R 10000: 10 ms start threshold
	cmd := exec.Command("/usr/bin/aplay", "-D", "default", "-B", "20000", "-R", "10000", "-t", "raw", "-f", "S16_LE", "-r", "48000", "-c", "2", "-q", "-")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		return err
	}

	// Clamp Linux pipe buffer to 4096 bytes (20 ms) to eliminate invisible OS buffering
	if file, ok := stdin.(*os.File); ok {
		syscall.Syscall(syscall.SYS_FCNTL, file.Fd(), 1031, 4096)
	}

	p.cmd = cmd
	p.stdin = stdin
	p.pid = cmd.Process.Pid
	p.running = true
	return nil
}

func (p *Player) Write(data []byte) error {
	p.mu.Lock()
	if !p.running || p.stdin == nil {
		p.mu.Unlock()
		return fmt.Errorf("player not running")
	}
	w := p.stdin
	p.mu.Unlock()

	_, err := w.Write(data)
	return err
}

func (p *Player) Stop() {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return
	}
	p.running = false
	w := p.stdin
	p.stdin = nil
	cmd := p.cmd
	p.cmd = nil
	pid := p.pid
	p.pid = 0
	p.mu.Unlock()

	if w != nil {
		w.Close()
	}

	if cmd != nil {
		done := make(chan error, 1)
		go func() {
			done <- cmd.Wait()
		}()

		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
			if pid > 0 {
				syscall.Kill(pid, syscall.SIGTERM)
			}
			select {
			case <-done:
			case <-time.After(150 * time.Millisecond):
				if pid > 0 {
					syscall.Kill(pid, syscall.SIGKILL)
				}
				<-done
			}
		}
	}
}

func (p *Player) IsRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
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

	exec.Command("killall", "-9", "aplay").Run()

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
	conn.SetReadBuffer(524288)

	player := &Player{}
	silenceChunk := make([]byte, chunkPcmLen)

	var atomicTargetDelay int64 = int64(getTargetDelayMs())
	var atomicPrebuffer int64
	var atomicTargetQ int64
	var lastTvDelayUpdate int64
	var rxPkts, droppedPkts, duplicatePkts, resyncEvents uint64
	queue := make(chan []byte, 200)

	updatePrebuffer := func(totalDelay int) {
		oldTargetQ := int(atomic.LoadInt64(&atomicTargetQ))
		oldDelay := int(atomic.LoadInt64(&atomicTargetDelay))

		// Unified linear queue formula: Base ALSA + pipe + net latency is ~40 ms
		q := (totalDelay - 40) / 10
		if q < 1 {
			q = 1 // Safety floor: minimum 1 chunk = 10 ms Go buffer
		}
		pre := q + 2 // Prime buffer before starting playback

		atomic.StoreInt64(&atomicTargetQ, int64(q))
		atomic.StoreInt64(&atomicPrebuffer, int64(pre))
		atomic.StoreInt64(&atomicTargetDelay, int64(totalDelay))

		// Dynamic Delay Adjustment during active playback (only executes when target delay changes!)
		if player.IsRunning() && oldDelay > 0 && totalDelay != oldDelay {
			if q > oldTargetQ {
				diff := q - oldTargetQ
				for i := 0; i < diff; i++ {
					_ = player.Write(silenceChunk)
				}
				fmt.Fprintf(os.Stderr, "[station-rx] Dynamic Delay Expansion: %d -> %d ms, injected %d silence chunks (targetQ: %d)\n",
					oldDelay, totalDelay, diff, q)
			} else if q < oldTargetQ {
				diff := oldTargetQ - q
				dropCount := 0
				for i := 0; i < diff && len(queue) > 1; i++ {
					select {
					case <-queue:
						atomic.AddUint64(&droppedPkts, 1)
						dropCount++
					default:
						break
					}
				}
				fmt.Fprintf(os.Stderr, "[station-rx] Dynamic Delay Reduction: %d -> %d ms, dropped %d chunks (targetQ: %d)\n",
					oldDelay, totalDelay, dropCount, q)
			}
		}
	}
	updatePrebuffer(int(atomicTargetDelay))

	fmt.Fprintf(os.Stderr, "[station-rx] Started listening on UDP4 %s with EMA Drift Servo (Calibrated: %d ms)\n",
		addr.String(), atomic.LoadInt64(&atomicTargetDelay))

	var lastPacketNano int64
	var lastSeq uint32
	var hasSeq bool
	var lastStationVol int = -1
	var lastMuted bool = false
	var hasVol bool = false

	// 1. TV Standby Watchdog (triggers 1500 ms after TV screen turns OFF or headphones connect)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		for range ticker.C {
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
				hasSeq = false
			}
		}
	}()

	// 2. Target Delay Live Reload Goroutine (Fallback to /data/audiosync/delay_ms if TV does not stream dynamic delay)
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		for range ticker.C {
			now := time.Now().UnixNano()
			lastUpdate := atomic.LoadInt64(&lastTvDelayUpdate)
			// If TV hasn't sent a dynamic delay in the last 5 seconds, sync with file
			if (now - lastUpdate) > int64(5*time.Second) {
				newTotal := getTargetDelayMs()
				curTotal := int(atomic.LoadInt64(&atomicTargetDelay))
				if newTotal != curTotal {
					updatePrebuffer(newTotal)
					fmt.Fprintf(os.Stderr, "[station-rx] Live target delay changed via file: %d ms -> %d ms\n", curTotal, newTotal)
				}
			}
		}
	}()

	// 3. UDP Receiver Goroutine with SYNC Header & Large Seek Detection
	go func() {
		buf := make([]byte, 4096)
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				continue
			}
			if n > 0 {
				atomic.StoreInt64(&lastPacketNano, time.Now().UnixNano())
				atomic.AddUint64(&rxPkts, 1)

				var pcm []byte
				var isMajorSeek bool

				if n >= 16+chunkPcmLen && binary.BigEndian.Uint32(buf[0:4]) == headerMagic {
					seq := binary.BigEndian.Uint32(buf[4:8])
					tvVol := int(buf[12])
					stationVol := int(buf[13])
					isMuted := buf[14] != 0
					tvTargetDelayUnits := int(buf[15])

					// Dynamic Target Delay from TV header (byte 15: delay / 10, support up to 1200 ms)
					if tvTargetDelayUnits >= 4 && tvTargetDelayUnits <= 120 {
						tvDelayMs := tvTargetDelayUnits * 10
						atomic.StoreInt64(&lastTvDelayUpdate, time.Now().UnixNano())
						curDelay := int(atomic.LoadInt64(&atomicTargetDelay))
						if tvDelayMs != curDelay {
							updatePrebuffer(tvDelayMs)
							fmt.Fprintf(os.Stderr, "[station-rx] Dynamic target delay received from TV: %d ms -> %d ms\n", curDelay, tvDelayMs)
						}
					}

					pcm = make([]byte, n-16)
					copy(pcm, buf[16:n])

					// Dynamic Volume Sync from TV
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
						// Major seek / channel change: sequence jumped by > 100 packets (1 second) or backward
						if seqDiff > 100 || seqDiff < -10 {
							isMajorSeek = true
						}
					}
					lastSeq = seq
					hasSeq = true
				} else {
					pcm = make([]byte, n)
					copy(pcm, buf[:n])
				}

				if isMajorSeek && player.IsRunning() {
					atomic.AddUint64(&resyncEvents, 1)
					curDelay := int(atomic.LoadInt64(&atomicTargetDelay))
					fmt.Fprintf(os.Stderr, "[station-rx] Major seek detected. Re-priming sync to %d ms.\n", curDelay)
					player.Stop()
					for len(queue) > 0 {
						select {
						case <-queue:
						default:
						}
					}
				}

				// Soft capacity limit to prevent memory bloat
				targetPre := int(atomic.LoadInt64(&atomicPrebuffer))
				if len(queue) >= targetPre+30 {
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
				fmt.Fprintf(os.Stderr, "[station-rx] Status: %d rx, %d dropped, %d dups, %d resyncs, qlen: %d, vol: %d (%d%%), player: %v, delay: %d ms\n",
					rx, dp, dup, rs, len(queue), lastStationVol, lastStationVol*10, player.IsRunning(), atomic.LoadInt64(&atomicTargetDelay))
			}
		}
	}()

	// 5. Main Playback Loop with EMA Closed-Loop Drift Servo
	for {
		pre := int(atomic.LoadInt64(&atomicPrebuffer))
		for len(queue) < pre {
			time.Sleep(5 * time.Millisecond)
		}

		last := atomic.LoadInt64(&lastPacketNano)
		gapMs := (time.Now().UnixNano() - last) / int64(time.Millisecond)
		if gapMs > 2000 {
			for len(queue) > 0 {
				select {
				case <-queue:
				default:
				}
			}
			continue
		}

		if err := player.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "[station-rx] aplay start failed: %v\n", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		curTotalDelay := int(atomic.LoadInt64(&atomicTargetDelay))
		targetQ := int(atomic.LoadInt64(&atomicTargetQ))
		fmt.Fprintf(os.Stderr, "[station-rx] aplay started (pid %d) locked at %d ms delay (targetQ: %d)\n", player.pid, curTotalDelay, targetQ)

		// Prime queue so when aplay drains its initial period, queue settles at targetQ
		for len(queue) > targetQ+2 {
			select {
			case <-queue:
			default:
				break
			}
		}

		emaQLen := float64(targetQ)
		lastDriftAdjust := time.Now()

		for player.IsRunning() {
			targetQ = int(atomic.LoadInt64(&atomicTargetQ))
			curQLen := len(queue)

			// Emergency overflow clamp: only triggers if a massive stall / seek filled the queue (> targetQ + 20)
			if curQLen >= targetQ+20 {
				dropCount := 0
				for len(queue) > targetQ+2 {
					select {
					case <-queue:
						atomic.AddUint64(&droppedPkts, 1)
						dropCount++
					default:
						break
					}
				}
				emaQLen = float64(targetQ)
				if dropCount > 0 {
					fmt.Fprintf(os.Stderr, "[station-rx] Massive overflow clamp: trimmed +%d chunks (qlen: %d)\n", dropCount, len(queue))
				}
				curQLen = len(queue)
			}

			// Update Exponential Moving Average (alpha=0.03, ~300ms smoothing window)
			emaQLen = 0.03*float64(curQLen) + 0.97*emaQLen

			// High-Precision EMA Drift Servo (Deadband: 2.5 chunks so UDP arrival jitter doesn't trigger spurious drops/dups)
			deadband := 2.5
			now := time.Now()
			if now.Sub(lastDriftAdjust) >= 3000*time.Millisecond {
				if emaQLen >= float64(targetQ)+deadband { // Persistent surplus (clock drift)
					select {
					case <-queue:
						atomic.AddUint64(&droppedPkts, 1)
						emaQLen -= 1.0
						lastDriftAdjust = now
						fmt.Fprintf(os.Stderr, "[station-rx] EMA Servo: dropped 1 chunk (ema: %.2f, qlen: %d) -> locked at %d ms\n",
							emaQLen, len(queue), atomic.LoadInt64(&atomicTargetDelay))
					default:
					}
				} else if emaQLen <= float64(targetQ)-deadband && curQLen >= 1 { // Persistent deficit (clock drift)
					_ = player.Write(silenceChunk)
					atomic.AddUint64(&duplicatePkts, 1)
					emaQLen += 1.0
					lastDriftAdjust = now
					fmt.Fprintf(os.Stderr, "[station-rx] EMA Servo: injected 1 silence chunk (ema: %.2f, qlen: %d) -> locked at %d ms\n",
						emaQLen, len(queue), atomic.LoadInt64(&atomicTargetDelay))
					continue // Skip reading from queue on this tick so queue gains 1 chunk
				}
			}

			// Normal read and play next audio chunk
			select {
			case chunk := <-queue:
				if err := player.Write(chunk); err != nil {
					fmt.Fprintf(os.Stderr, "[station-rx] aplay write failed: %v\n", err)
					player.Stop()
					break
				}
			case <-time.After(15 * time.Millisecond):
				// Network stall: inject 1 comfort silence chunk to avoid ALSA underrun
				_ = player.Write(silenceChunk)
				atomic.AddUint64(&duplicatePkts, 1)
			}
		}

		player.Stop()
		time.Sleep(20 * time.Millisecond)
	}
}
