package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	decimRate     = 16000 // 16 samples per 1 ms
	historySec    = 2     // 2.048 seconds circular history (32768 frames at 16 kHz)
	historyFrames = 32768 // Power of two for fast masking
	historyMask   = historyFrames - 1

	// Amlogic AUGESOUND Audio Controller physical register block (/soc/audiobus@0xff642000):
	// - TODDR_A (0xff642100): 8-channel PDM microphone capture ring (pcmC0D1c)
	//     +0x08: START_ADDR  (e.g. 0x0F860000)
	//     +0x0C: FINISH_ADDR (e.g. 0x0F867FF8 -> 32768 B = 2048 frames)
	//     +0x18: STATUS2     (current hardware write address in physical RAM!)
	// - TODDR_B (0xff642140): 2-channel hardware TDM loopback ring (pcmC0D0c)
	//     +0x08: START_ADDR  (e.g. 0x0F858000)
	//     +0x0C: FINISH_ADDR (e.g. 0x0F859FF8 -> 8192 B = 2048 frames)
	//     +0x18: STATUS2     (current hardware write address in physical RAM!)
	augePhysBase    = 0xff642000
	augeMapSize     = 0x1000
	toddrAStartOff  = 0x108
	toddrAStatusOff = 0x118
	toddrBStartOff  = 0x148
	toddrBStatusOff = 0x158

	dmaMicsFrames = 2048 // buffer_size=2048 frames, 8 channels, S16_LE (32768 B)
	dmaMicsBytes  = dmaMicsFrames * 8 * 2
	dmaSpkFrames  = 2048 // buffer_size=2048 frames, 2 channels, S16_LE (8192 B)
	dmaSpkBytes   = dmaSpkFrames * 2 * 2

	// NLMS Adaptive Acoustic Echo Canceller parameters (16 kHz):
	// Cancels the Station's internal speaker AND couch/table reflections up to 16.0 ms using hardware loopback TODDR_B.
	// 256 taps = 16.0 ms filter length: cancels enclosure resonance, plastic chassis AND couch reflections (3.9 ms).
	nlmsFilterLen = 256
	nlmsStepMu    = 0.05
	nlmsEps       = 1e-4

	// FFT size for GCC-PHAT cross-correlation:
	// N = 32768 samples at 16 kHz = 2048 ms analysis window (covers lags up to ±1024 ms).
	fftSize = 32768
)

type AcousticResult struct {
	SpkLagMs    float64 // Measured lag from UDP arrival (refRing) to Station speaker hardware loopback (TODDR_B)
	TvLagMs     float64 // Measured lag from UDP arrival (refRing) to TV sound arriving at Station mics
	AcousticErr float64 // True room mismatch: (TvLagMs - geomOffsetMs) - SpkLagMs (>0 means TV is later than Station)
	SpkConf     float64 // Peak-to-sidelobe confidence of Station loopback
	TvConf      float64 // Peak-to-sidelobe PSR confidence of TV acoustic arrival
	Valid       bool
}

type AcousticEstimator struct {
	refMu sync.Mutex
	dmaMu sync.Mutex

	// Unified 16 kHz timeline rings indexed by micWritePos (hardware DMA clock):
	refRing     [historyFrames]float32
	refWritePos uint64

	// 4 echo-cancelled microphone channels (after per-mic NLMS against spkLoopRing)
	micCleanRing [4][historyFrames]float32
	spkLoopRing  [historyFrames]float32
	micWritePos  uint64

	// Per-microphone NLMS adaptive filter weights (4 mics x 64 taps) and delay line
	nlmsW    [4][nlmsFilterLen]float32
	nlmsX    [nlmsFilterLen]float32
	nlmsXPow float32
	nlmsHead int

	// Pre-allocated FFT buffers to avoid GC pressure and keep CPU < 2%
	fftA    []complex128
	fftB    []complex128
	fftCorr []complex128
	corrMag []float64

	// Smoothed offset (refWritePos - micWritePos) between UDP reference stream and DMA clock
	refOffsetEma float64
	hasRefOffset bool

	// Atomic telemetry for status logging
	lastTvLagTenthsMs  int64
	lastSpkLagTenthsMs int64
	lastErrTenthsMs    int64
	lastConfidence     int64
	lastMicRmsPermille int64
	lastSpkRmsPermille int64
	packetsRx          uint64
}

func NewAcousticEstimator() *AcousticEstimator {
	return &AcousticEstimator{
		fftA:    make([]complex128, fftSize),
		fftB:    make([]complex128, fftSize),
		fftCorr: make([]complex128, fftSize),
		corrMag: make([]float64, fftSize),
	}
}

// FeedUdpReferenceChunk48k records incoming UDP audio from TV into refRing monotonically (exactly 160
// decimated samples per 10 ms packet, zero sample insertion/deletion phase distortion) while tracking
// the smoothed offset against the hardware DMA clock (micWritePos).
// Only locks refMu, so it NEVER blocks behind DMA reader or FFT!
func (ae *AcousticEstimator) FeedUdpReferenceChunk48k(pcm []byte) {
	if len(pcm) < 1920 {
		return
	}
	dmaPos := atomic.LoadUint64(&ae.micWritePos)

	ae.refMu.Lock()
	defer ae.refMu.Unlock()

	if dmaPos > 0 {
		diff := float64(int64(ae.refWritePos) - int64(dmaPos))
		if !ae.hasRefOffset || math.Abs(diff) > 3200 { // Re-anchor only on stream start or >200 ms pause
			ae.refWritePos = dmaPos
			ae.refOffsetEma = 0
			ae.hasRefOffset = true
		} else {
			ae.refOffsetEma = 0.05*diff + 0.95*ae.refOffsetEma
		}
	}

	pos := ae.refWritePos
	for i := 0; i < 160; i++ {
		base := i * 12
		var sum int32
		for k := 0; k < 3; k++ {
			off := base + k*4
			l := int32(int16(binary.LittleEndian.Uint16(pcm[off : off+2])))
			r := int32(int16(binary.LittleEndian.Uint16(pcm[off+2 : off+4])))
			sum += (l + r) >> 1
		}
		ae.refRing[pos&historyMask] = float32(sum) / (3.0 * 32768.0)
		pos++
	}
	ae.refWritePos = pos
}

// StartPassiveDmaReader maps the physical Amlogic AUGESOUND registers (0xff642000) and DMA buffers
// (TODDR_A for 4 mics, TODDR_B for hardware speaker loopback) via /dev/mem.
// Reads hardware write pointers from TODDR_A_STATUS2 (+0x118) and TODDR_B_STATUS2 (+0x158) independently!
func (ae *AcousticEstimator) StartPassiveDmaReader() {
	go func() {
		for {
			devMem, err := os.OpenFile("/dev/mem", os.O_RDONLY|syscall.O_SYNC, 0)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[acoustic-dma] open /dev/mem failed: %v\n", err)
				time.Sleep(3 * time.Second)
				continue
			}

			regs, err := syscall.Mmap(int(devMem.Fd()), augePhysBase, augeMapSize, syscall.PROT_READ, syscall.MAP_SHARED)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[acoustic-dma] mmap audiobus regs failed: %v\n", err)
				_ = devMem.Close()
				time.Sleep(3 * time.Second)
				continue
			}

			micsPhys := binary.LittleEndian.Uint32(regs[toddrAStartOff : toddrAStartOff+4])
			spkPhys := binary.LittleEndian.Uint32(regs[toddrBStartOff : toddrBStartOff+4])
			if micsPhys == 0 || spkPhys == 0 || (micsPhys&0xfff) != 0 || (spkPhys&0xfff) != 0 {
				fmt.Fprintf(os.Stderr, "[acoustic-dma] invalid TODDR addrs: mics=0x%08x, spk=0x%08x\n", micsPhys, spkPhys)
				_ = syscall.Munmap(regs)
				_ = devMem.Close()
				time.Sleep(3 * time.Second)
				continue
			}

			micsRaw, err1 := syscall.Mmap(int(devMem.Fd()), int64(micsPhys), dmaMicsBytes, syscall.PROT_READ, syscall.MAP_SHARED)
			spkRaw, err2 := syscall.Mmap(int(devMem.Fd()), int64(spkPhys), dmaSpkBytes, syscall.PROT_READ, syscall.MAP_SHARED)
			if err1 != nil || err2 != nil {
				if err1 == nil {
					_ = syscall.Munmap(micsRaw)
				}
				if err2 == nil {
					_ = syscall.Munmap(spkRaw)
				}
				_ = syscall.Munmap(regs)
				_ = devMem.Close()
				time.Sleep(3 * time.Second)
				continue
			}

			fmt.Fprintf(os.Stderr, "[acoustic-dma] Zero-syscall MMAP active: TODDR_A(mics)=0x%08x, TODDR_B(spk)=0x%08x\n",
				micsPhys, spkPhys)

			var lastMicRingFrame int = -1
			var lastSpkRingFrame int = -1

			ticker := time.NewTicker(10 * time.Millisecond)
			for range ticker.C {
				// Read current DMA write addresses from both TODDR_A_STATUS2 (+0x118) and TODDR_B_STATUS2 (+0x158)
				curMicsPhys := binary.LittleEndian.Uint32(regs[toddrAStatusOff : toddrAStatusOff+4])
				curSpkPhys := binary.LittleEndian.Uint32(regs[toddrBStatusOff : toddrBStatusOff+4])
				if curMicsPhys < micsPhys || curMicsPhys >= micsPhys+dmaMicsBytes {
					continue
				}
				if curSpkPhys < spkPhys || curSpkPhys >= spkPhys+dmaSpkBytes {
					continue
				}

				curMicFrame := int(curMicsPhys-micsPhys) / 16 // 16 bytes per 8-ch frame
				curSpkFrame := int(curSpkPhys-spkPhys) / 4    // 4 bytes per 2-ch frame

				if lastMicRingFrame < 0 || lastSpkRingFrame < 0 {
					lastMicRingFrame = curMicFrame
					lastSpkRingFrame = curSpkFrame
					continue
				}

				availMic := (curMicFrame - lastMicRingFrame + dmaMicsFrames) % dmaMicsFrames
				availSpk := (curSpkFrame - lastSpkRingFrame + dmaSpkFrames) % dmaSpkFrames
				availFrames := availMic
				if availSpk < availFrames {
					availFrames = availSpk
				}
				if availFrames < 6 {
					continue
				}
				if availFrames > 1536 || availMic > 1536 || availSpk > 1536 {
					lastMicRingFrame = (curMicFrame - 480 + dmaMicsFrames) % dmaMicsFrames
					lastSpkRingFrame = (curSpkFrame - 480 + dmaSpkFrames) % dmaSpkFrames
					availFrames = 480
				}

				var micSqSum, spkSqSum float64
				var outSamples int

				ae.dmaMu.Lock()
				pos := ae.micWritePos
				i := 0
				for ; i+2 < availFrames; i += 3 {
					var mSum [4]int32
					var spkSum int32

					for k := 0; k < 3; k++ {
						rfMic := (lastMicRingFrame + i + k) & (dmaMicsFrames - 1)
						rfSpk := (lastSpkRingFrame + i + k) & (dmaSpkFrames - 1)
						mOff := rfMic * 16 // 8 channels * 2 bytes
						sOff := rfSpk * 4  // 2 channels * 2 bytes

						mSum[0] += int32(int16(binary.LittleEndian.Uint16(micsRaw[mOff+0 : mOff+2])))
						mSum[1] += int32(int16(binary.LittleEndian.Uint16(micsRaw[mOff+4 : mOff+6])))
						mSum[2] += int32(int16(binary.LittleEndian.Uint16(micsRaw[mOff+8 : mOff+10])))
						mSum[3] += int32(int16(binary.LittleEndian.Uint16(micsRaw[mOff+12 : mOff+14])))

						s0 := int32(int16(binary.LittleEndian.Uint16(spkRaw[sOff+0 : sOff+2])))
						s1 := int32(int16(binary.LittleEndian.Uint16(spkRaw[sOff+2 : sOff+4])))
						spkSum += (s0 + s1) >> 1
					}

					spkVal := float32(spkSum) / (3.0 * 32768.0)
					var micVals [4]float32
					for ch := 0; ch < 4; ch++ {
						micVals[ch] = float32(mSum[ch]) / (3.0 * 32768.0)
					}

					// Fast recursive power calculation & delay line update
					oldVal := ae.nlmsX[ae.nlmsHead]
					ae.nlmsX[ae.nlmsHead] = spkVal
					ae.nlmsXPow += spkVal*spkVal - oldVal*oldVal
					if ae.nlmsXPow < nlmsEps {
						ae.nlmsXPow = nlmsEps
					}
					normStep := float32(nlmsStepMu) / ae.nlmsXPow
					adaptActive := ae.nlmsXPow > 1e-3

					head := ae.nlmsHead
					ae.nlmsHead = (ae.nlmsHead + 1) & (nlmsFilterLen - 1)

					idx := pos & historyMask
					ae.spkLoopRing[idx] = spkVal

					var cleanAvg float32
					for ch := 0; ch < 4; ch++ {
						var echoEst float32
						w := &ae.nlmsW[ch]
						for t := 0; t < nlmsFilterLen; t++ {
							xIdx := (head - t + nlmsFilterLen) & (nlmsFilterLen - 1)
							echoEst += w[t] * ae.nlmsX[xIdx]
						}
						errSig := micVals[ch] - echoEst
						if adaptActive {
							muErr := normStep * errSig
							for t := 0; t < nlmsFilterLen; t++ {
								xIdx := (head - t + nlmsFilterLen) & (nlmsFilterLen - 1)
								w[t] += muErr * ae.nlmsX[xIdx]
							}
						}
						ae.micCleanRing[ch][idx] = errSig
						cleanAvg += errSig * 0.25
					}

					pos++
					micSqSum += float64(cleanAvg * cleanAvg)
					spkSqSum += float64(spkVal * spkVal)
					outSamples++
				}
				lastMicRingFrame = (lastMicRingFrame + i) & (dmaMicsFrames - 1)
				lastSpkRingFrame = (lastSpkRingFrame + i) & (dmaSpkFrames - 1)
				atomic.StoreUint64(&ae.micWritePos, pos)
				ae.dmaMu.Unlock()

				if outSamples > 0 {
					micRms := math.Sqrt(micSqSum / float64(outSamples))
					spkRms := math.Sqrt(spkSqSum / float64(outSamples))
					atomic.StoreInt64(&ae.lastMicRmsPermille, int64(math.Round(micRms*1000)))
					atomic.StoreInt64(&ae.lastSpkRmsPermille, int64(math.Round(spkRms*1000)))
				}
				atomic.AddUint64(&ae.packetsRx, 1)
			}

			ticker.Stop()
			_ = syscall.Munmap(micsRaw)
			_ = syscall.Munmap(spkRaw)
			_ = syscall.Munmap(regs)
			_ = devMem.Close()
			time.Sleep(1 * time.Second)
		}
	}()
}

// fftInPlace performs an in-place radix-2 Cooley-Tukey FFT (or IFFT if invert=true).
func fftInPlace(a []complex128, invert bool) {
	n := len(a)
	shift := 64 - bits.Len(uint(n-1))
	for i := 1; i < n; i++ {
		j := int(bits.Reverse64(uint64(i)) >> shift)
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}

	for length := 2; length <= n; length <<= 1 {
		ang := 2 * math.Pi / float64(length)
		if !invert {
			ang = -ang
		}
		wlen := complex(math.Cos(ang), math.Sin(ang))
		half := length >> 1
		for i := 0; i < n; i += length {
			w := complex(1, 0)
			for j := 0; j < half; j++ {
				u := a[i+j]
				v := a[i+j+half] * w
				a[i+j] = u + v
				a[i+j+half] = u - v
				w *= wlen
			}
		}
	}

	if invert {
		invN := complex(1.0/float64(n), 0)
		for i := range a {
			a[i] *= invN
		}
	}
}

// AnalyzeRoomSync runs regularized FFT-based GCC-PHAT over the latest windowSamples (e.g. 8192 samples = 512 ms)
// completely passively without interrupting playback.
func (ae *AcousticEstimator) AnalyzeRoomSync(windowMs int, expectedStationDelayMs float64, geomAirMs float64, appHint int) AcousticResult {
	var res AcousticResult
	winSamples := windowMs * (decimRate / 1000)
	if winSamples > fftSize/2 {
		winSamples = fftSize / 2
	}
	if winSamples < 1024 {
		winSamples = 1024
	}

	endMicPos := atomic.LoadUint64(&ae.micWritePos)

	ae.refMu.Lock()
	refWritePos := ae.refWritePos
	refOffsetEma := ae.refOffsetEma
	ae.refMu.Unlock()

	if endMicPos < uint64(winSamples+1600) || refWritePos < uint64(winSamples+1600) {
		return res
	}

	offsetSamples := int64(math.Round(refOffsetEma))
	endRefPos := uint64(int64(endMicPos) + offsetSamples)
	if endRefPos > refWritePos {
		endRefPos = refWritePos
	}
	if endRefPos < uint64(winSamples) {
		return res
	}

	startMicPos := endMicPos - uint64(winSamples)
	startRefPos := endRefPos - uint64(winSamples)

	refSlice := make([]float32, winSamples)
	spkSlice := make([]float32, winSamples)
	var micSlices [4][]float32
	for ch := 0; ch < 4; ch++ {
		micSlices[ch] = make([]float32, winSamples)
	}

	var rawRefSq float64
	ae.refMu.Lock()
	for i := 0; i < winSamples; i++ {
		rIdx := (startRefPos + uint64(i)) & historyMask
		rv := ae.refRing[rIdx]
		refSlice[i] = rv
		rawRefSq += float64(rv * rv)
	}
	ae.refMu.Unlock()

	var rawMicSq float64
	ae.dmaMu.Lock()
	for i := 0; i < winSamples; i++ {
		mIdx := (startMicPos + uint64(i)) & historyMask
		spkSlice[i] = ae.spkLoopRing[mIdx]
		for ch := 0; ch < 4; ch++ {
			mv := ae.micCleanRing[ch][mIdx]
			micSlices[ch][i] = mv
			rawMicSq += float64(mv * mv)
		}
	}
	ae.dmaMu.Unlock()

	// Calibrated Noise Gate: check raw RMS before running FFT
	// Sensitive enough for quiet Spotify music / dialogue at TV volume 4..6, while rejecting room silence
	refRms := math.Sqrt(rawRefSq / float64(winSamples))
	micRms := math.Sqrt(rawMicSq / float64(winSamples*4))
	if refRms < 0.001 || micRms < 0.00008 {
		return res
	}

	// Pre-emphasis filter (high-pass transient sharpener) & energy check
	var refEnergy, spkEnergy, micEnergy float64
	for i := winSamples - 1; i >= 1; i-- {
		refSlice[i] -= 0.95 * refSlice[i-1]
		spkSlice[i] -= 0.95 * spkSlice[i-1]
		refEnergy += float64(refSlice[i] * refSlice[i])
		spkEnergy += float64(spkSlice[i] * spkSlice[i])
		for ch := 0; ch < 4; ch++ {
			micSlices[ch][i] -= 0.95 * micSlices[ch][i-1]
			micEnergy += float64(micSlices[ch][i] * micSlices[ch][i])
		}
	}
	refSlice[0] = 0
	spkSlice[0] = 0
	for ch := 0; ch < 4; ch++ {
		micSlices[ch][0] = 0
	}

	if refEnergy < 0.0005 || micEnergy < 0.00005 {
		return res
	}

	// 1. FFT of reference signal (stored in fftA)
	for i := 0; i < fftSize; i++ {
		if i < winSamples {
			ae.fftA[i] = complex(float64(refSlice[i]), 0)
		} else {
			ae.fftA[i] = 0
		}
	}
	fftInPlace(ae.fftA, false)

	// Band-pass bin bounds for GCC-PHAT (300 Hz .. 6000 Hz at 16 kHz, fftSize=16384)
	minBin := (300 * fftSize) / decimRate
	maxBin := (6000 * fftSize) / decimRate

	// 2. Measure exact Station playback delay (refSlice -> spkSlice) via regularized GCC-PHAT
	res.SpkLagMs = expectedStationDelayMs
	if spkEnergy >= 1e-4 {
		for i := 0; i < fftSize; i++ {
			if i < winSamples {
				ae.fftB[i] = complex(float64(spkSlice[i]), 0)
			} else {
				ae.fftB[i] = 0
			}
			ae.fftCorr[i] = 0
		}
		fftInPlace(ae.fftB, false)

		var sumSpkMag float64
		for k := minBin; k <= maxBin; k++ {
			ra := ae.fftA[k]
			sb := ae.fftB[k]
			prod := sb * complex(real(ra), -imag(ra))
			sumSpkMag += math.Hypot(real(prod), imag(prod))
		}
		meanSpkMag := sumSpkMag / float64(maxBin-minBin+1)

		for k := minBin; k <= maxBin; k++ {
			ra := ae.fftA[k]
			sb := ae.fftB[k]
			prod := sb * complex(real(ra), -imag(ra))
			mag := math.Hypot(real(prod), imag(prod))
			ae.fftCorr[k] = prod / complex(mag+0.05*meanSpkMag+1e-6, 0)
			ae.fftCorr[fftSize-k] = complex(real(ae.fftCorr[k]), -imag(ae.fftCorr[k]))
		}
		fftInPlace(ae.fftCorr, true)

		// Search Station speaker hardware loopback within ±35 ms of expected Station buffer delay
		minSpkSample := int((expectedStationDelayMs - 35.0) * 16.0)
		if minSpkSample < 480 {
			minSpkSample = 480
		}
		maxSpkSample := int((expectedStationDelayMs + 35.0) * 16.0)
		if maxSpkSample > fftSize/2 {
			maxSpkSample = fftSize / 2
		}
		var maxSpk, sumSpk float64
		var countSpk int
		bestSpkSample := int(expectedStationDelayMs * 16.0)
		for s := minSpkSample; s <= maxSpkSample; s++ {
			v := math.Abs(real(ae.fftCorr[s]))
			sumSpk += v
			countSpk++
			if v > maxSpk {
				maxSpk = v
				bestSpkSample = s
			}
		}
		meanSpk := sumSpk / float64(countSpk+1)
		if meanSpk > 0 {
			res.SpkConf = maxSpk / (meanSpk * 10.0)
			if res.SpkConf > 1.0 {
				res.SpkConf = 1.0
			}
		}
		if res.SpkConf >= 0.60 {
			res.SpkLagMs = float64(bestSpkSample) * 1000.0 / float64(decimRate)
		} else {
			res.SpkLagMs = expectedStationDelayMs
		}
	}

	// 3. Multi-microphone Regularized GCC-PHAT for TV arrival:
	// Sum magnitude profiles across all 4 NLMS-cleaned mics with spectral regularization
	for i := 0; i < fftSize; i++ {
		ae.corrMag[i] = 0
	}

	for ch := 0; ch < 4; ch++ {
		for i := 0; i < fftSize; i++ {
			if i < winSamples {
				ae.fftB[i] = complex(float64(micSlices[ch][i]), 0)
			} else {
				ae.fftB[i] = 0
			}
			ae.fftCorr[i] = 0
		}
		fftInPlace(ae.fftB, false)

		var sumCrossMag float64
		for k := minBin; k <= maxBin; k++ {
			ra := ae.fftA[k]
			mb := ae.fftB[k]
			prod := mb * complex(real(ra), -imag(ra))
			sumCrossMag += math.Hypot(real(prod), imag(prod))
		}
		meanCrossMag := sumCrossMag / float64(maxBin-minBin+1)

		for k := minBin; k <= maxBin; k++ {
			ra := ae.fftA[k]
			mb := ae.fftB[k]
			prod := mb * complex(real(ra), -imag(ra))
			mag := math.Hypot(real(prod), imag(prod))
			// Regularized denominator suppresses flat noise bins
			ae.fftCorr[k] = prod / complex(mag+0.05*meanCrossMag+1e-6, 0)
			ae.fftCorr[fftSize-k] = complex(real(ae.fftCorr[k]), -imag(ae.fftCorr[k]))
		}
		fftInPlace(ae.fftCorr, true)
		for s := 0; s < fftSize/2; s++ {
			ae.corrMag[s] += math.Abs(real(ae.fftCorr[s])) * 0.25
		}
	}

	// Blank narrow chassis direct coupling (±3.0 ms = ±48 samples around Station direct speaker).
	// With 64-tap NLMS active, Station's own sound is cancelled in micCleanRing, but blanking avoids edge leakage.
	spkCenterSample := int(math.Round(res.SpkLagMs * 16.0))
	spkBlankRadius := 48

	// Physical corridor for TV arrival at Station microphones:
	// - YouTube / Kinopoisk (ExoPlayer Video): TV Audio HAL write latency is 163 ms + buffer = ~215..230 ms
	// - Spotify (pure audio): ~130..150 ms
	// - Twitch (low latency): ~125..145 ms
	minSearchSample := int(100.0 * 16.0)
	maxSearchSample := int(350.0 * 16.0)

	switch appHint {
	case 1, 4, 14, 15, 40: // YouTube / Kinopoisk / generic video
		minSearchSample = int(175.0 * 16.0) // 175 ms
		maxSearchSample = int(265.0 * 16.0) // 265 ms
	case 2, 12: // Spotify pure audio (Spotify AudioTrack 44.1k buffer = 732.65 ms in AudioFlinger)
		minSearchSample = int(680.0 * 16.0) // 680 ms
		maxSearchSample = int(785.0 * 16.0) // 785 ms
	case 3, 7: // Twitch low latency
		minSearchSample = int(115.0 * 16.0) // 115 ms
		maxSearchSample = int(165.0 * 16.0) // 165 ms
	}

	var maxTv float64
	bestTvSample := -1
	for s := minSearchSample; s <= maxSearchSample; s++ {
		if s >= spkCenterSample-spkBlankRadius && s <= spkCenterSample+spkBlankRadius {
			continue
		}
		v := ae.corrMag[s]
		if v > maxTv {
			maxTv = v
			bestTvSample = s
		}
	}
	if bestTvSample < 0 || maxTv <= 1e-9 {
		return res
	}

	// Compute mean background floor outside ±8 ms (±128 samples) around the peak and speaker loopback
	var sumBg float64
	var countBg int
	for s := minSearchSample; s <= maxSearchSample; s++ {
		if s >= spkCenterSample-spkBlankRadius && s <= spkCenterSample+spkBlankRadius {
			continue
		}
		if s >= bestTvSample-128 && s <= bestTvSample+128 {
			continue
		}
		sumBg += ae.corrMag[s]
		countBg++
	}
	meanBg := sumBg / float64(countBg+1)
	if meanBg <= 1e-9 {
		return res
	}

	// Peak-to-sidelobe ratio (PSR): require PSR >= 4.5 for valid lock
	psr := maxTv / meanBg
	res.TvConf = psr / 10.0
	if res.TvConf > 1.0 {
		res.TvConf = 1.0
	}
	if psr < 4.5 {
		return res
	}

	// Direct-path arrival selection within 12 ms before the strongest reflection peak:
	// With Station placed on the couch, rear-wall reflection is largely suppressed.
	// Only lock onto an earlier peak if it is a prominent direct arrival (>= 0.85 * maxTv).
	directWindowStart := bestTvSample - int(12.0*16.0)
	if directWindowStart < minSearchSample {
		directWindowStart = minSearchSample
	}
	thresh := 0.85 * maxTv
	for s := directWindowStart; s < bestTvSample; s++ {
		if s >= spkCenterSample-spkBlankRadius && s <= spkCenterSample+spkBlankRadius {
			continue
		}
		if ae.corrMag[s] >= thresh && ae.corrMag[s] > ae.corrMag[s-1] && ae.corrMag[s] > ae.corrMag[s+1] {
			bestTvSample = s
			break
		}
	}

	res.TvLagMs = float64(bestTvSample) * 1000.0 / float64(decimRate)
	// True physical room acoustic mismatch at the listener's position on the couch:
	// Sound from TV arrives at couch at TvLagMs. Sound from Station arrives at couch at SpkLagMs.
	// When AcousticErr = 0, sound waves hit listener's ears at the exact same instant!
	res.AcousticErr = res.TvLagMs - res.SpkLagMs
	res.Valid = true

	atomic.StoreInt64(&ae.lastTvLagTenthsMs, int64(math.Round(res.TvLagMs*10)))
	atomic.StoreInt64(&ae.lastSpkLagTenthsMs, int64(math.Round(res.SpkLagMs*10)))
	atomic.StoreInt64(&ae.lastErrTenthsMs, int64(math.Round(res.AcousticErr*10)))
	atomic.StoreInt64(&ae.lastConfidence, int64(math.Round(res.TvConf*1000)))
	return res
}
