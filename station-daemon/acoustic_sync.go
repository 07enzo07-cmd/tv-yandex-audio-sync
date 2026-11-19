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
	historyFrames = 65536 // 4.096 seconds circular history at 16 kHz (power of two)
	historyMask   = historyFrames - 1

	// Amlogic AUGESOUND Audio Controller physical register block (/soc/audiobus@0xff642000)
	augePhysBase    = 0xff642000
	augeMapSize     = 0x1000
	toddrAStartOff  = 0x108
	toddrAStatusOff = 0x118
	toddrBStartOff  = 0x148
	toddrBStatusOff = 0x158

	dmaMicsFrames = 2048 // 8 channels, S16_LE (32768 B)
	dmaMicsBytes  = dmaMicsFrames * 8 * 2
	dmaSpkFrames  = 2048 // 2 channels, S16_LE (8192 B)
	dmaSpkBytes   = dmaSpkFrames * 2 * 2

	// FFT size for GCC-PHAT cross-correlation (2048 ms window @ 16 kHz)
	fftSize = 32768

	// Measured acoustic offset between TODDR_B loopback and Station speaker mics (~0.0..-0.5 ms)
	deltaLoopMs = -0.5

	// Near-field overlap half-width around SpkLag in TRACK mode (ms)
	overlapHalfMs = 20.0
)

type AcousticResult struct {
	SpkLagMs    float64 // Measured lag from UDP arrival (refRing) to Station speaker loopback (TODDR_B + deltaLoopMs)
	TvLagMs     float64 // Measured lag from UDP arrival (refRing) to TV sound at Station mics
	AcousticErr float64 // e = TvLagMs - SpkLagMs (<0 means Station plays later than TV)
	SpkConf     float64 // Confidence of Station loopback detection
	TvConf      float64 // Confidence of TV detection (0..1)
	Valid       bool
	InOverlap   bool   // True when TV peak is masked by near-field overlap zone around SpkLag
	Status      string // "CLEAN_LOCKED", "TV_DETECTED", "HOLD_OVERLAP", "LOW_ENERGY", "LOW_PSR", "NO_SPK"
	TvMag       float64
	TvPsr       float64
	Contrast    float64
	RefRms      float64
	MicRms      float64
	PeakIdx     int
}

type AcousticEstimator struct {
	// Atomic 64-bit fields MUST be at the top of struct for 8-byte alignment on 32-bit ARM (ARMv7):
	micWritePos        uint64
	refWritePos        uint64
	lastTvLagTenthsMs  int64
	lastSpkLagTenthsMs int64
	lastErrTenthsMs    int64
	lastConfidence     int64
	lastMicRmsPermille int64
	lastSpkRmsPermille int64
	packetsRx          uint64
	lastValidUnixMs    int64

	refMu sync.Mutex
	dmaMu sync.Mutex
	dspMu sync.Mutex

	// Unified 16 kHz timeline rings indexed by micWritePos (hardware DMA clock):
	refRing     [historyFrames]float32
	refPlcRing  [historyFrames]uint8 // 1 if frame came from PLC/loss (excluded from correlation)
	micRing     [4][historyFrames]float32
	spkLoopRing [historyFrames]float32

	// Pre-allocated FFT buffers
	fftA    []complex128
	fftB    []complex128
	fftCorr []complex128
	corrMag []float64

	// Accumulative weighted GCC-PHAT state for TRACK / ACQUIRE
	accumCorr   []float64
	accumWeight float64
	accumFrames int

	// Smoothed offset (refWritePos - micWritePos) between UDP reference stream and DMA clock
	refOffsetEma float64
	hasRefOffset bool
}

func NewAcousticEstimator() *AcousticEstimator {
	return &AcousticEstimator{
		fftA:      make([]complex128, fftSize),
		fftB:      make([]complex128, fftSize),
		fftCorr:   make([]complex128, fftSize),
		corrMag:   make([]float64, fftSize),
		accumCorr: make([]float64, fftSize),
	}
}

func (ae *AcousticEstimator) ResetAccumulator() {
	ae.dspMu.Lock()
	defer ae.dspMu.Unlock()
	clear(ae.accumCorr)
	ae.accumWeight = 0
	ae.accumFrames = 0
}

// FeedUdpReferenceChunk48k records incoming UDP audio from TV into refRing monotonically.
// Supports both 5 ms (960 B -> 80 samples @ 16 kHz) and 10 ms (1920 B -> 160 samples @ 16 kHz) chunks.
// If isPLC is true, marks samples in refPlcRing so they are excluded from correlation.
func (ae *AcousticEstimator) FeedUdpReferenceChunk48k(pcm []byte, isPLC bool) {
	frames48 := len(pcm) / 4
	nOut := frames48 / 3
	if nOut <= 0 {
		return
	}
	dmaPos := atomic.LoadUint64(&ae.micWritePos)

	ae.refMu.Lock()
	defer ae.refMu.Unlock()

	if dmaPos > 0 {
		diff := float64(int64(ae.refWritePos) - int64(dmaPos))
		if !ae.hasRefOffset || math.Abs(diff) > 3200 { // Re-anchor only on stream start or >200 ms gap
			ae.refWritePos = dmaPos
			ae.refOffsetEma = 0
			ae.hasRefOffset = true
		} else {
			ae.refOffsetEma = 0.05*diff + 0.95*ae.refOffsetEma
		}
	}

	var plcMark uint8
	if isPLC {
		plcMark = 1
	}

	pos := ae.refWritePos
	for i := 0; i < nOut; i++ {
		base := i * 12
		var sum int32
		if !isPLC {
			for k := 0; k < 3; k++ {
				off := base + k*4
				l := int32(int16(binary.LittleEndian.Uint16(pcm[off : off+2])))
				r := int32(int16(binary.LittleEndian.Uint16(pcm[off+2 : off+4])))
				sum += (l + r) >> 1
			}
		}
		idx := pos & historyMask
		ae.refRing[idx] = float32(sum) / (3.0 * 32768.0)
		ae.refPlcRing[idx] = plcMark
		pos++
	}
	ae.refWritePos = pos
}

// StartPassiveDmaReader maps Amlogic AUGESOUND DMA rings (TODDR_A mics, TODDR_B loopback) read-only.
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

			lastMicRingFrame := -1
			lastSpkRingFrame := -1

			ticker := time.NewTicker(8 * time.Millisecond)
			for range ticker.C {
				curMicsPhys := binary.LittleEndian.Uint32(regs[toddrAStatusOff : toddrAStatusOff+4])
				curSpkPhys := binary.LittleEndian.Uint32(regs[toddrBStatusOff : toddrBStatusOff+4])
				if curMicsPhys < micsPhys || curMicsPhys >= micsPhys+dmaMicsBytes {
					continue
				}
				if curSpkPhys < spkPhys || curSpkPhys >= spkPhys+dmaSpkBytes {
					continue
				}

				curMicFrame := int(curMicsPhys-micsPhys) / 16
				curSpkFrame := int(curSpkPhys-spkPhys) / 4

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
						mOff := rfMic * 16
						sOff := rfSpk * 4

						mSum[0] += int32(int16(binary.LittleEndian.Uint16(micsRaw[mOff+0 : mOff+2])))
						mSum[1] += int32(int16(binary.LittleEndian.Uint16(micsRaw[mOff+4 : mOff+6])))
						mSum[2] += int32(int16(binary.LittleEndian.Uint16(micsRaw[mOff+8 : mOff+10])))
						mSum[3] += int32(int16(binary.LittleEndian.Uint16(micsRaw[mOff+12 : mOff+14])))

						s0 := int32(int16(binary.LittleEndian.Uint16(spkRaw[sOff+0 : sOff+2])))
						s1 := int32(int16(binary.LittleEndian.Uint16(spkRaw[sOff+2 : sOff+4])))
						spkSum += (s0 + s1) >> 1
					}

					spkVal := float32(spkSum) / (3.0 * 32768.0)
					idx := pos & historyMask
					ae.spkLoopRing[idx] = spkVal
					var micEnergy float64
					for ch := 0; ch < 4; ch++ {
						mv := float32(mSum[ch]) / (3.0 * 32768.0)
						ae.micRing[ch][idx] = mv
						micEnergy += float64(mv * mv)
					}

					pos++
					micSqSum += micEnergy / 4
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

func parabolicPeak(arr []float64, mid, best, minLag, maxLag int) float64 {
	interp := float64(best)
	if best > minLag && best < maxLag {
		y0 := arr[mid+best-1]
		y1 := arr[mid+best]
		y2 := arr[mid+best+1]
		denom := y0 - 2*y1 + y2
		if math.Abs(denom) > 1e-12 {
			delta := 0.5 * (y0 - y2) / denom
			if delta > -0.5 && delta < 0.5 {
				interp += delta
			}
		}
	}
	return interp
}

// extractWindow safely copies synchronized ref, spk, and 4 mic slices of length winSamples.
func (ae *AcousticEstimator) extractWindow(winSamples int) (refSlice, spkSlice []float32, micSlices [4][]float32, refRms, micRms float64, ok bool) {
	endMicPos := atomic.LoadUint64(&ae.micWritePos)

	ae.refMu.Lock()
	refWritePos := ae.refWritePos
	refOffsetEma := ae.refOffsetEma
	ae.refMu.Unlock()

	if endMicPos < uint64(winSamples) || refWritePos < uint64(winSamples) {
		return nil, nil, micSlices, 0, 0, false
	}

	endRef := int64(endMicPos) + int64(math.Round(refOffsetEma))
	if endRef > int64(refWritePos) {
		// Move both windows together. Clipping only the network reference
		// would change the measured lag with every receive burst.
		back := uint64(endRef - int64(refWritePos))
		if back > endMicPos {
			return nil, nil, micSlices, 0, 0, false
		}
		endMicPos -= back
		endRef = int64(refWritePos)
	}
	if endRef < int64(winSamples) || endMicPos < uint64(winSamples) {
		return nil, nil, micSlices, 0, 0, false
	}
	endRefPos := uint64(endRef)
	startMicPos := endMicPos - uint64(winSamples)
	startRefPos := endRefPos - uint64(winSamples)

	refSlice = make([]float32, winSamples)
	spkSlice = make([]float32, winSamples)
	for ch := 0; ch < 4; ch++ {
		micSlices[ch] = make([]float32, winSamples)
	}

	var rawRefSq float64
	var plcCount int
	ae.refMu.Lock()
	for i := 0; i < winSamples; i++ {
		rIdx := (startRefPos + uint64(i)) & historyMask
		if ae.refPlcRing[rIdx] != 0 {
			refSlice[i] = 0
			plcCount++
		} else {
			rv := ae.refRing[rIdx]
			refSlice[i] = rv
			rawRefSq += float64(rv * rv)
		}
	}
	ae.refMu.Unlock()

	// Reject window if >20% was PLC/lost packets
	if plcCount > winSamples/5 {
		return nil, nil, micSlices, 0, 0, false
	}

	var rawMicSq float64
	ae.dmaMu.Lock()
	for i := 0; i < winSamples; i++ {
		mIdx := (startMicPos + uint64(i)) & historyMask
		spkSlice[i] = ae.spkLoopRing[mIdx]
		for ch := 0; ch < 4; ch++ {
			mv := ae.micRing[ch][mIdx]
			micSlices[ch][i] = mv
			rawMicSq += float64(mv * mv)
		}
	}
	ae.dmaMu.Unlock()

	refRms = math.Sqrt(rawRefSq / float64(winSamples))
	micRms = math.Sqrt(rawMicSq / float64(winSamples*4))
	return refSlice, spkSlice, micSlices, refRms, micRms, true
}

func preEmphasis(s []float32) float64 {
	var energy float64
	for i := len(s) - 1; i >= 1; i-- {
		s[i] -= 0.95 * s[i-1]
		energy += float64(s[i] * s[i])
	}
	if len(s) > 0 {
		s[0] = 0
	}
	return energy
}

// AnalyzeCleanTvLag runs full-range GCC-PHAT [3..950 ms] with NO blind zone and NO target cropping.
// Intended for ACQUIRE (Station speaker silent) and diagnostic /data/audiosync/cmd_probe.
func (ae *AcousticEstimator) AnalyzeCleanTvLag(windowMs int, accumulate bool, minPsr float64, minContrast float64) AcousticResult {
	ae.dspMu.Lock()
	defer ae.dspMu.Unlock()

	res := AcousticResult{Status: "LOW_ENERGY"}
	winSamples := windowMs * (decimRate / 1000)
	if winSamples > fftSize/2 {
		winSamples = fftSize / 2
	}
	if winSamples < 4096 {
		winSamples = 4096
	}

	refSlice, _, micSlices, refRms, micRms, ok := ae.extractWindow(winSamples)
	res.RefRms = refRms
	res.MicRms = micRms
	if !ok || refRms < 0.0015 || micRms < 0.00002 {
		return res
	}

	refEnergy := preEmphasis(refSlice)
	var micEnergy float64
	for ch := 0; ch < 4; ch++ {
		micEnergy += preEmphasis(micSlices[ch])
	}
	if refEnergy < 0.0005 || micEnergy < 1e-6 {
		return res
	}

	// Reference FFT
	for i := 0; i < fftSize; i++ {
		if i < winSamples {
			ae.fftA[i] = complex(float64(refSlice[i]), 0)
		} else {
			ae.fftA[i] = 0
		}
	}
	fftInPlace(ae.fftA, false)

	minBin := (300 * fftSize) / decimRate
	maxBin := (6000 * fftSize) / decimRate
	clear(ae.corrMag)

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

		var sumMag float64
		for k := minBin; k <= maxBin; k++ {
			ra := ae.fftA[k]
			mb := ae.fftB[k]
			prod := mb * complex(real(ra), -imag(ra))
			sumMag += math.Hypot(real(prod), imag(prod))
		}
		meanMag := sumMag / float64(maxBin-minBin+1)

		for k := minBin; k <= maxBin; k++ {
			ra := ae.fftA[k]
			mb := ae.fftB[k]
			prod := mb * complex(real(ra), -imag(ra))
			mag := math.Hypot(real(prod), imag(prod))
			ae.fftCorr[k] = prod / complex(mag+0.05*meanMag+1e-6, 0)
			ae.fftCorr[fftSize-k] = complex(real(ae.fftCorr[k]), -imag(ae.fftCorr[k]))
		}
		fftInPlace(ae.fftCorr, true)
		for lag := -fftSize / 2; lag < fftSize/2; lag++ {
			ae.corrMag[lag+fftSize/2] += math.Abs(real(ae.fftCorr[lag&(fftSize-1)])) * 0.25
		}
	}

	var curve []float64
	if accumulate {
		w := math.Min(1.0, refRms*20.0)
		const decay = 0.85
		ae.accumWeight = ae.accumWeight*decay + w
		ae.accumFrames++
		for i := 0; i < fftSize; i++ {
			ae.accumCorr[i] = ae.accumCorr[i]*decay + ae.corrMag[i]*w
		}
		curve = ae.accumCorr
	} else {
		curve = ae.corrMag
	}

	mid := fftSize / 2
	minLag := 48 // 3.0 ms
	maxLag := min(15200, winSamples-1600)
	if maxLag <= minLag {
		return res
	}

	best := minLag
	for lag := minLag; lag <= maxLag; lag++ {
		if curve[mid+lag] > curve[mid+best] {
			best = lag
		}
	}
	peak := curve[mid+best]
	var sum, rival float64
	var cnt int
	for lag := minLag; lag <= maxLag; lag++ {
		if absLag(lag-best) <= 64 { // ±4 ms around main lobe
			continue
		}
		v := curve[mid+lag]
		if v > rival {
			rival = v
		}
		sum += v
		cnt++
	}
	psr := peak / (sum/float64(cnt) + 1e-9)
	contrast := peak / (rival + 1e-9)

	interpLag := parabolicPeak(curve, mid, best, minLag, maxLag)
	res.TvLagMs = interpLag / 16.0
	res.TvPsr = psr
	res.Contrast = contrast
	res.TvMag = peak
	res.PeakIdx = best
	res.TvConf = math.Min(1.0, psr/12.0)

	if psr >= minPsr && contrast >= minContrast && peak > 0.001 {
		res.Valid = true
		res.Status = "CLEAN_LOCKED"
		atomic.StoreInt64(&ae.lastTvLagTenthsMs, int64(math.Round(res.TvLagMs*10)))
		atomic.StoreInt64(&ae.lastConfidence, int64(math.Round(res.TvConf*1000)))
		atomic.StoreInt64(&ae.lastValidUnixMs, time.Now().UnixMilli())
	} else {
		res.Status = "LOW_PSR"
	}
	return res
}

// confirmCleanTV allows a strong clean observation immediately. A moderate
// observation needs a second plausible peak at the same lag before it can
// change playback. Both observations come from a muted-Station window.
func confirmCleanTV(current, previous AcousticResult) (AcousticResult, bool) {
	if current.Valid && current.TvPsr >= 8 && current.Contrast >= 1.3 {
		return current, true
	}
	if current.TvPsr < 5 || current.Contrast < 1.1 || current.RefRms < 0.004 ||
		previous.TvPsr < 5 || previous.Contrast < 1.1 || previous.RefRms < 0.004 ||
		math.Abs(current.TvLagMs-previous.TvLagMs) > 7 {
		return AcousticResult{}, false
	}
	if current.Valid {
		return current, true
	}
	return previous, previous.Valid
}

// AnalyzeTrackSync runs in TRACK mode while Station is playing:
//  1. Measures exact SpkLagMs from ref -> TODDR_B (always visible, high SNR).
//  2. Accumulates weighted GCC-PHAT across active speech/music frames over full range [3..950 ms].
//  3. Does NOT use destructive blind-zone cropping or coherent zero-lag near-field subtraction;
//     instead cleanly distinguishes outside-overlap TV drift vs inside-overlap HOLD.
func (ae *AcousticEstimator) AnalyzeTrackSync(windowMs int, expectedStationDelayMs float64) AcousticResult {
	ae.dspMu.Lock()
	defer ae.dspMu.Unlock()

	res := AcousticResult{Status: "LOW_ENERGY"}
	winSamples := windowMs * (decimRate / 1000)
	if winSamples > fftSize/2 {
		winSamples = fftSize / 2
	}
	if winSamples < 4096 {
		winSamples = 4096
	}

	refSlice, spkSlice, micSlices, refRms, micRms, ok := ae.extractWindow(winSamples)
	res.RefRms = refRms
	res.MicRms = micRms
	if !ok || refRms < 0.0015 || micRms < 0.00002 {
		return res
	}

	refEnergy := preEmphasis(refSlice)
	spkEnergy := preEmphasis(spkSlice)
	var micEnergy float64
	for ch := 0; ch < 4; ch++ {
		micEnergy += preEmphasis(micSlices[ch])
	}
	if refEnergy < 0.0005 || micEnergy < 1e-6 {
		return res
	}

	// 1. FFT of reference
	for i := 0; i < fftSize; i++ {
		if i < winSamples {
			ae.fftA[i] = complex(float64(refSlice[i]), 0)
		} else {
			ae.fftA[i] = 0
		}
	}
	fftInPlace(ae.fftA, false)

	minBin := (300 * fftSize) / decimRate
	maxBin := (6000 * fftSize) / decimRate

	// 2. Measure exact Station playback delay (ref -> TODDR_B)
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

		minSpkS := max(160, int((expectedStationDelayMs-150.0)*16.0))
		maxSpkS := min(fftSize/2-1, int((expectedStationDelayMs+180.0)*16.0))
		var maxSpk, sumSpk float64
		var countSpk int
		bestSpkS := minSpkS
		for s := minSpkS; s <= maxSpkS; s++ {
			v := math.Abs(real(ae.fftCorr[s]))
			sumSpk += v
			countSpk++
			if v > maxSpk {
				maxSpk = v
				bestSpkS = s
			}
		}
		meanSpk := sumSpk / float64(countSpk+1)
		if meanSpk > 0 {
			res.SpkConf = math.Min(1.0, maxSpk/(meanSpk*10.0))
		}
		if res.SpkConf >= 0.50 {
			res.SpkLagMs = float64(bestSpkS)/16.0 + deltaLoopMs
			atomic.StoreInt64(&ae.lastSpkLagTenthsMs, int64(math.Round(res.SpkLagMs*10)))
		}
	}

	if res.SpkConf < 0.55 {
		res.Status = "NO_SPK"
		return res
	}

	// 3. Weighted accumulative GCC-PHAT of 4 mics vs ref (NO destructive coherent subtraction)
	clear(ae.corrMag)
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

		var sumMag float64
		for k := minBin; k <= maxBin; k++ {
			ra := ae.fftA[k]
			mb := ae.fftB[k]
			prod := mb * complex(real(ra), -imag(ra))
			sumMag += math.Hypot(real(prod), imag(prod))
		}
		meanMag := sumMag / float64(maxBin-minBin+1)
		for k := minBin; k <= maxBin; k++ {
			ra := ae.fftA[k]
			mb := ae.fftB[k]
			prod := mb * complex(real(ra), -imag(ra))
			mag := math.Hypot(real(prod), imag(prod))
			ae.fftCorr[k] = prod / complex(mag+0.05*meanMag+1e-6, 0)
			ae.fftCorr[fftSize-k] = complex(real(ae.fftCorr[k]), -imag(ae.fftCorr[k]))
		}
		fftInPlace(ae.fftCorr, true)
		for lag := -fftSize / 2; lag < fftSize/2; lag++ {
			ae.corrMag[lag+fftSize/2] += math.Abs(real(ae.fftCorr[lag&(fftSize-1)])) * 0.25
		}
	}

	// Exponential forgetting across active frames (~8s time constant at 500ms step -> decay=0.90)
	w := math.Min(1.0, refRms*20.0)
	const decay = 0.88
	ae.accumWeight = ae.accumWeight*decay + w
	ae.accumFrames++
	for i := 0; i < fftSize; i++ {
		ae.accumCorr[i] = ae.accumCorr[i]*decay + ae.corrMag[i]*w
	}

	// Search full range [3..950 ms] OUTSIDE the near-field overlap mask around SpkLagMs
	mid := fftSize / 2
	minLag := 48 // 3 ms
	maxLag := min(15200, winSamples-1600)
	if maxLag <= minLag {
		return res
	}

	spkLagSamples := res.SpkLagMs * 16.0
	overlapSamples := overlapHalfMs * 16.0

	best := -1
	for lag := minLag; lag <= maxLag; lag++ {
		if math.Abs(float64(lag)-spkLagSamples) <= overlapSamples {
			continue
		}
		if best < 0 || ae.accumCorr[mid+lag] > ae.accumCorr[mid+best] {
			best = lag
		}
	}
	if best < 0 {
		res.InOverlap = true
		res.Status = "HOLD_OVERLAP"
		return res
	}

	peak := ae.accumCorr[mid+best]
	var sum, rival float64
	var cnt int
	for lag := minLag; lag <= maxLag; lag++ {
		if math.Abs(float64(lag)-spkLagSamples) <= overlapSamples || absLag(lag-best) <= 64 {
			continue
		}
		v := ae.accumCorr[mid+lag]
		if v > rival {
			rival = v
		}
		sum += v
		cnt++
	}
	psr := peak / (sum/float64(cnt) + 1e-9)
	contrast := peak / (rival + 1e-9)

	interpLag := parabolicPeak(ae.accumCorr, mid, best, minLag, maxLag)
	res.TvLagMs = interpLag / 16.0
	res.TvPsr = psr
	res.Contrast = contrast
	res.TvMag = peak
	res.PeakIdx = best
	res.AcousticErr = res.TvLagMs - res.SpkLagMs
	res.TvConf = math.Min(1.0, psr/12.0)

	// High threshold outside overlap to prevent false locks on room reflections
	if psr >= 7.5 && contrast >= 1.25 && peak > 0.001 {
		res.Valid = true
		res.Status = "TV_DETECTED"
		atomic.StoreInt64(&ae.lastTvLagTenthsMs, int64(math.Round(res.TvLagMs*10)))
		atomic.StoreInt64(&ae.lastErrTenthsMs, int64(math.Round(res.AcousticErr*10)))
		atomic.StoreInt64(&ae.lastConfidence, int64(math.Round(res.TvConf*1000)))
		atomic.StoreInt64(&ae.lastValidUnixMs, time.Now().UnixMilli())
	} else {
		// No distinct TV peak outside overlap -> TV is likely within overlap zone (HOLD)
		res.InOverlap = true
		res.Status = "HOLD_OVERLAP"
	}
	return res
}

func absLag(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
