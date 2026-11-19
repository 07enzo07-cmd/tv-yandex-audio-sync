package main

import (
	"encoding/binary"
	"math"
	"math/rand"
	"testing"
	"time"
)

func TestParseSYN3AndLegacySYNC(t *testing.T) {
	// SYN3 5ms packet = 20B header + 960B PCM = 980B
	pkt := make([]byte, syn3HeaderLen+960)
	binary.BigEndian.PutUint32(pkt[0:4], magicSYN3)
	binary.BigEndian.PutUint32(pkt[4:8], 42)
	binary.BigEndian.PutUint32(pkt[8:12], 42*240)
	binary.BigEndian.PutUint32(pkt[12:16], 123456)
	pkt[16] = 12
	pkt[17] = 80
	pkt[18] = syn3FlagMuted | syn3FlagLowLatency
	pkt[19] = 10 // 5.0 ms

	ap, ok := ParseAudioPacket(pkt)
	if !ok {
		t.Fatalf("ParseAudioPacket failed on SYN3")
	}
	if ap.ProtoVer != 3 || ap.Seq != 42 || ap.TvVol != 12 || ap.StationVolPct != 80 || !ap.IsMuted || !ap.LowLatency || ap.FrameMs != 5.0 || len(ap.PCM) != 960 {
		t.Fatalf("unexpected SYN3 parse: %+v", ap)
	}

	// Legacy Android v2 SYNC packet = 16B header + 1920B PCM = 1936B
	v2 := make([]byte, syncHeaderLen+1920)
	binary.BigEndian.PutUint32(v2[0:4], magicSYNC)
	binary.BigEndian.PutUint32(v2[4:8], 99)
	binary.BigEndian.PutUint32(v2[8:12], 55555)
	v2[12] = 10 | 0x80
	v2[13] = 7
	v2[14] = 0
	ap2, ok2 := ParseAudioPacket(v2)
	if !ok2 {
		t.Fatalf("ParseAudioPacket failed on legacy SYNC")
	}
	if ap2.ProtoVer != 2 || ap2.Seq != 99 || ap2.TvVol != 10 || ap2.StationVolPct != 70 || ap2.IsMuted || ap2.FrameMs != 10.0 || len(ap2.PCM) != 1920 {
		t.Fatalf("unexpected legacy SYNC parse: %+v", ap2)
	}
}

func TestReorderBufferOutOfOrderAndLoss(t *testing.T) {
	rb := NewReorderBuffer(3)
	mk := func(seq uint32) AudioPacket {
		return AudioPacket{ProtoVer: 3, Seq: seq, FrameMs: 5.0, PCM: make([]byte, 960)}
	}

	// In-order 1, then out-of-order 3, then 2
	r1, _ := rb.Push(mk(1))
	if len(r1) != 1 || r1[0].Seq != 1 {
		t.Fatalf("expected seq 1, got %v", r1)
	}
	r3, _ := rb.Push(mk(3))
	if len(r3) != 0 {
		t.Fatalf("expected buffered seq 3, got %v", r3)
	}
	r2, _ := rb.Push(mk(2))
	if len(r2) != 2 || r2[0].Seq != 2 || r2[1].Seq != 3 {
		t.Fatalf("expected seq 2,3, got %v", r2)
	}
	// Duplicate 2 ignored
	rDup, _ := rb.Push(mk(2))
	if len(rDup) != 0 || rb.DupCount != 1 {
		t.Fatalf("expected duplicate ignored, got %v dup=%d", rDup, rb.DupCount)
	}
	// Loss of seq 4: pushing 5, 6, 7 forces PLC for seq 4
	_, _ = rb.Push(mk(5))
	_, _ = rb.Push(mk(6))
	r7, _ := rb.Push(mk(7))
	if len(r7) != 4 || !r7[0].IsPLC || r7[0].Seq != 4 || r7[1].Seq != 5 || r7[3].Seq != 7 {
		t.Fatalf("expected PLC(4)+5+6+7, got len=%d first=%+v", len(r7), r7[0])
	}
}

func TestAcousticCleanAcquireAndSpeechPause(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	// Test clean ACQUIRE accuracy (<1 ms) across multiple true TV lags including inside old blind zone!
	for _, trueLagMs := range []float64{12.5, 45.0, 120.0, 250.0} {
		ae := NewAcousticEstimator()
		lagSamples := int(math.Round(trueLagMs * 16.0))
		totalSamples := 24000 // 1.5 s @ 16 kHz

		// Generate speech-like broadband signal with a 300ms pause in the middle
		sig := make([]float32, totalSamples+lagSamples)
		for i := range sig {
			if i > 8000 && i < 12800 {
				sig[i] = 0 // 300 ms speech pause
			} else {
				sig[i] = float32((rng.Float64()*2 - 1) * 0.25)
			}
		}

		for i := 0; i < totalSamples; i++ {
			idx := uint64(i) & historyMask
			ae.refRing[idx] = sig[i]
			for ch := 0; ch < 4; ch++ {
				var tv float32
				if i >= lagSamples {
					tv = sig[i-lagSamples] * 0.05 // quiet TV + room noise
				}
				noise := float32((rng.Float64()*2 - 1) * 0.002)
				ae.micRing[ch][idx] = tv + noise
			}
		}
		ae.micWritePos = uint64(totalSamples)
		ae.refWritePos = uint64(totalSamples)
		ae.hasRefOffset = true
		ae.refOffsetEma = 0

		res := ae.AnalyzeCleanTvLag(1000, false, 6.0, 1.15)
		if !res.Valid {
			t.Fatalf("lag=%.1f ms: expected Valid clean acquire, got status=%s psr=%.2f contrast=%.2f",
				trueLagMs, res.Status, res.TvPsr, res.Contrast)
		}
		if math.Abs(res.TvLagMs-trueLagMs) > 1.0 {
			t.Fatalf("lag=%.1f ms: expected error <1 ms, got %.2f ms", trueLagMs, res.TvLagMs)
		}
	}
}

func TestAcousticStOnlyNoFalseTvLock(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	ae := NewAcousticEstimator()
	spkLagSamples := int(80.0 * 16.0)
	totalSamples := 24000

	sig := make([]float32, totalSamples+spkLagSamples)
	for i := range sig {
		sig[i] = float32((rng.Float64()*2 - 1) * 0.25)
	}
	for i := 0; i < totalSamples; i++ {
		idx := uint64(i) & historyMask
		ae.refRing[idx] = sig[i]
		var spk float32
		if i >= spkLagSamples {
			spk = sig[i-spkLagSamples] * 0.5
		}
		ae.spkLoopRing[idx] = spk
		for ch := 0; ch < 4; ch++ {
			// ST-only: mics hear only Station near-field (+room noise), NO TV
			noise := float32((rng.Float64()*2 - 1) * 0.003)
			ae.micRing[ch][idx] = spk*0.8 + noise
		}
	}
	ae.micWritePos = uint64(totalSamples)
	ae.refWritePos = uint64(totalSamples)
	ae.hasRefOffset = true

	res := ae.AnalyzeTrackSync(1000, 80.0)
	if res.Valid {
		t.Fatalf("ST-only must NOT falsely lock TV outside overlap, got tv=%.2f psr=%.2f", res.TvLagMs, res.TvPsr)
	}
	if !res.InOverlap {
		t.Fatalf("ST-only expected InOverlap=true (HOLD), got status=%s", res.Status)
	}
}

func TestControllerStateMachineAndNoOscillation(t *testing.T) {
	now := time.Now()
	ctrl := NewSyncController(115, true, true) // dual-actuator enabled for policy test
	ctrl.EnterAcquire("start")

	// 1. A 10 ms TV lag requires a 20 ms TV delay at the 33 ms hardware floor.
	ok, dst, reqDtv, seq := ctrl.HandleAcquireResult(AcousticResult{Valid: true, TvLagMs: 10.0, TvConf: 0.9}, now)
	if !ok || !ctrl.IsLowLatencyHwMode() || dst != hwMinStationMs || reqDtv != 20 || seq != 1 {
		t.Fatalf("expected hwMode=true dst=%d reqDtv=20 seq=1, got ok=%t hw=%t dst=%d reqDtv=%d seq=%d",
			hwMinStationMs, ok, ctrl.IsLowLatencyHwMode(), dst, reqDtv, seq)
	}
	ctrl.HandleTvap(TvapPacket{AppliedTvDelayMs: 20, Status: tvapStatusOK, CmdSeq: seq})
	ctrl.MarkJoined()
	if ctrl.State() != StateTrack {
		t.Fatalf("expected StateTrack after TVAP, got %s", ctrl.State())
	}

	// 2. When the TV baseline rises to 25 ms, keep its delay within the hysteresis margin.
	_, _, reqDtv2, _ := ctrl.HandleTrackResult(AcousticResult{
		Valid: true, SpkConf: 0.9, SpkLagMs: 30.0, TvLagMs: 45.0, AcousticErr: 15.0, TvConf: 0.9,
	}, now.Add(6*time.Second))
	if reqDtv2 != 20 {
		t.Fatalf("hysteresis violated: D_tv should remain 20 ms when margin < 10 ms, got %d", reqDtv2)
	}

	// 3. A 12 ms TV lag is below the playback floor with the TV actuator disabled.
	ctrlNoTv := NewSyncController(115, true, false)
	ctrlNoTv.EnterAcquire("start")
	ctrlNoTv.HandleAcquireResult(AcousticResult{Valid: true, TvLagMs: 12.0, TvConf: 0.9}, now)
	ctrlNoTv.MarkJoined()
	if !ctrlNoTv.IsLowLatencyHwMode() {
		t.Fatalf("expected auto lowLatencyHwMode=true for L_tv=12ms")
	}
	if ctrlNoTv.State() != StateUnreachable || ctrlNoTv.TargetDelayMs() != hwMinStationMs {
		t.Fatalf("expected UNREACHABLE at dst=%d, got state=%s dst=%d", hwMinStationMs, ctrlNoTv.State(), ctrlNoTv.TargetDelayMs())
	}
}

func TestVolumePctToLinearGainAndSoftVolume(t *testing.T) {
	if g := VolumePctToLinearGain(0); g != 0.0 {
		t.Fatalf("expected 0%% -> 0.0, got %f", g)
	}
	if g := VolumePctToLinearGain(100); g != 1.0 {
		t.Fatalf("expected 100%% -> 1.0, got %f", g)
	}
	// 47% -> -51*(1-0.47) = -27.03 dB -> ~0.0445 (matches amixer Master 120/255 [-27.00dB])
	g47 := VolumePctToLinearGain(47)
	expected47 := math.Pow(10.0, (-51.0*0.53)/20.0)
	if math.Abs(g47-expected47) > 1e-6 {
		t.Fatalf("expected 47%% gain %.6f, got %.6f", expected47, g47)
	}

	// Verify smooth click-free ramping in SoftVolume.ScalePCM
	var sv SoftVolume
	sv.Reset(0)
	// 240 frames (5 ms) of constant +20000 stereo PCM
	src := make([]byte, 240*4)
	for i := 0; i < 240; i++ {
		binary.LittleEndian.PutUint16(src[i*4:i*4+2], uint16(int16(20000)))
		binary.LittleEndian.PutUint16(src[i*4+2:i*4+4], uint16(int16(20000)))
	}
	out := sv.ScalePCM(src, nil, 100)
	first := int16(binary.LittleEndian.Uint16(out[0:2]))
	last := int16(binary.LittleEndian.Uint16(out[239*4 : 239*4+2]))
	// Should ramp smoothly upward without jumping immediately to 20000
	if first <= 0 || first >= 200 || last <= first || last >= 20000 {
		t.Fatalf("expected smooth ramp from 0%% to 100%% over >5ms, got first=%d last=%d", first, last)
	}
	// After 3 more chunks (total 20 ms > 15 ms ramp), should reach 20000 cleanly
	for k := 0; k < 3; k++ {
		out = sv.ScalePCM(src, out, 100)
	}
	settled := int16(binary.LittleEndian.Uint16(out[239*4 : 239*4+2]))
	if settled != 20000 {
		t.Fatalf("expected settled gain at 100%% -> 20000, got %d", settled)
	}
}

func TestLowLatencyHwModeAndFallback(t *testing.T) {
	now := time.Now()
	ctrl := NewSyncController(65, true, false)

	// 1. Auto-engage hw:0,0 when ACQUIRE measures PS5 Game Mode L_tv = 30 ms (< 45 ms)
	ctrl.EnterAcquire("hdmi2_start")
	ok, dst, _, _ := ctrl.HandleAcquireResult(AcousticResult{Valid: true, TvLagMs: 30.0, TvConf: 0.92}, now)
	if !ok || !ctrl.IsLowLatencyHwMode() {
		t.Fatalf("expected auto hwMode=true on L_tv=21.5ms, got ok=%t hw=%t", ok, ctrl.IsLowLatencyHwMode())
	}
	// The wanted 32 ms is below the buffered hardware mode floor.
	if dst != hwMinStationMs {
		t.Fatalf("expected hardware floor %d ms, got %d", hwMinStationMs, dst)
	}
	ctrl.MarkJoined()
	if ctrl.State() != StateTrack {
		t.Fatalf("expected StateTrack within 5 ms of the floor, got %s", ctrl.State())
	}

	// 2. Live H_measured calibration works down to ~10 ms in hw:0,0 mode
	ctrl.UpdateMeasuredAlsaBase(20.0, 10.0) // h = 10.0 ms
	if ctrl.TargetQueueMs() < hwMinQMs {
		t.Fatalf("expected TargetQueueMs >= %.1f, got %.1f", hwMinQMs, ctrl.TargetQueueMs())
	}

	// 3. Fallback to default (dmix) if hw:0,0 is busy (Alice speaking)
	ctrl.SetLowLatencyHwMode(false)
	if ctrl.IsLowLatencyHwMode() || ctrl.MinQueueMs() != defaultMinQMs || ctrl.TargetDelayMs() < defaultMinStationMs {
		t.Fatalf("expected fallback to default floor >=%d ms, got hw=%t minQ=%.1f dst=%d",
			defaultMinStationMs, ctrl.IsLowLatencyHwMode(), ctrl.MinQueueMs(), ctrl.TargetDelayMs())
	}

	// 4. Switching back to YouTube (L_tv = 60.5 ms) stays in default dmix mode
	ctrl.EnterAcquire("youtube_switch")
	ok, dst, _, _ = ctrl.HandleAcquireResult(AcousticResult{Valid: true, TvLagMs: 60.5, TvConf: 0.95}, now.Add(5*time.Second))
	if !ok || ctrl.IsLowLatencyHwMode() || dst != 62 {
		t.Fatalf("expected default mode dst=62 ms for YouTube 60.5ms, got ok=%t hw=%t dst=%d", ok, ctrl.IsLowLatencyHwMode(), dst)
	}
}

func TestHDMIAcquireTimeoutKeepsSafeDelay(t *testing.T) {
	ctrl := NewSyncController(65, true, false)
	now := time.Now()
	ctrl.EnterAcquire("stream_start")
	ctrl.HandleAcquireResult(AcousticResult{Valid: true, TvLagMs: 65}, now)
	ctrl.SetLowLatencyHwMode(true)
	ctrl.EnterAcquire("session_reset")
	dst, tvDelay, _ := ctrl.HandleAcquireTimeout(now.Add(time.Second))
	if dst != 67 || tvDelay != 0 || ctrl.Snapshot(0, now).CacheUsed {
		t.Fatalf("expected safe target without a stale TV lock or actuator, got dst=%d tv=%d", dst, tvDelay)
	}
}

func TestHDMIClockCorrectionHandlesCaptureRate(t *testing.T) {
	ctrl := NewSyncController(65, true, false)
	ctrl.SetLowLatencyHwMode(true)
	var ppm float64
	for i := 0; i < 12; i++ {
		ppm = ctrl.UpdateClockDriftPI(-4, 1)
	}
	if ppm > -13000 || ppm < -20000 {
		t.Fatalf("expected correction for ~197 frames/s, got %.0f ppm", ppm)
	}
}

func TestSoftVolumePreservesFullScaleSignedPCM(t *testing.T) {
	pcm := []byte{0xff, 0x7f, 0x00, 0x80}
	var volume SoftVolume
	volume.Reset(100)
	if out := volume.ScalePCM(pcm, nil, 100); string(out) != string(pcm) {
		t.Fatalf("expected signed PCM endpoints unchanged, got %x", out)
	}
}

func TestHardwareDelayCalibrationUsesCleanTVTargetAndLoopbackConsensus(t *testing.T) {
	ctrl := NewSyncController(60, true, false)
	ctrl.SetLowLatencyHwMode(true)
	now := time.Now()
	ctrl.EnterAcquire("stream_start")
	ctrl.HandleAcquireResult(AcousticResult{Valid: true, TvLagMs: 52.8, TvConf: .8}, now)
	ctrl.MarkJoined()
	before := ctrl.TargetQueueMs()
	for i, lag := range []float64{55, 58, 57, 33, 56, 76, 58} {
		_, changed := ctrl.CalibrateHardwareDelay(AcousticResult{
			SpkLagMs: lag, SpkConf: .92, InOverlap: true,
		}, 24, now.Add(time.Duration(i)*time.Second/2))
		if changed != (i == 6) {
			t.Fatalf("sample %d: unexpected calibration change %t", i, changed)
		}
	}
	if q := ctrl.TargetQueueMs(); q != before-3 {
		t.Fatalf("expected to smoothly reduce queue by 3 ms, got %.1f -> %.1f", before, q)
	}

	ctrl.EnterAcquire("session_reset")
	for i := 0; i < 10; i++ {
		if _, changed := ctrl.CalibrateHardwareDelay(AcousticResult{SpkLagMs: 55, SpkConf: .99}, 24, now); changed {
			t.Fatal("cannot calibrate against a prior source's TV measurement")
		}
	}
}

func TestHardwareDelayCalibrationRejectsDisagreement(t *testing.T) {
	ctrl := NewSyncController(60, true, false)
	ctrl.SetLowLatencyHwMode(true)
	now := time.Now()
	ctrl.EnterAcquire("stream_start")
	ctrl.HandleAcquireResult(AcousticResult{Valid: true, TvLagMs: 52.8}, now)
	for i, lag := range []float64{32, 38, 44, 50, 56, 62, 68} {
		if _, changed := ctrl.CalibrateHardwareDelay(AcousticResult{
			SpkLagMs: lag, SpkConf: .98,
		}, 24, now.Add(time.Duration(i)*time.Second/2)); changed {
			t.Fatal("disputed estimates must leave the playback target unchanged")
		}
	}
}

func TestAcousticWindowsStayAlignedWhenPacketsArriveLate(t *testing.T) {
	ae := NewAcousticEstimator()
	ae.micWritePos = 6000
	ae.refWritePos = 5800
	for i := 0; i < 6000; i++ {
		ae.refRing[i] = float32(i)
		ae.micRing[0][i] = float32(i)
		ae.spkLoopRing[i] = float32(i)
	}
	ref, spk, mic, _, _, ok := ae.extractWindow(4096)
	if !ok || ref[0] != mic[0][0] || ref[4095] != spk[4095] {
		t.Fatal("receive batching must not shift the mic/loopback windows against the reference")
	}
}

func TestHardwareTrackHoldsCleanLockAgainstSingleMixedPeak(t *testing.T) {
	ctrl := NewSyncController(60, true, false)
	ctrl.SetLowLatencyHwMode(true)
	now := time.Now()
	ctrl.EnterAcquire("stream_start")
	ctrl.HandleAcquireResult(AcousticResult{Valid: true, TvLagMs: 52.8}, now)
	ctrl.MarkJoined()
	shift := AcousticResult{Valid: true, TvLagMs: 38, AcousticErr: -18, TvConf: .9}
	ctrl.HandleTrackResult(shift, now.Add(time.Second))
	if got := ctrl.Snapshot(0, now.Add(time.Second)).LtvMs; got != 52.8 {
		t.Fatalf("single mixed-window peak must preserve prior TV lock, got %.1f", got)
	}
	ctrl.HandleTrackResult(shift, now.Add(2*time.Second))
	if got := ctrl.Snapshot(0, now.Add(2*time.Second)).LtvMs; got != 38 {
		t.Fatalf("confirmed change must update acoustic TV lag, got %.1f", got)
	}
}

func TestConfirmCleanTVRejectsIsolatedModeratePeak(t *testing.T) {
	moderate := AcousticResult{Valid: true, TvLagMs: 52, TvPsr: 6.2, Contrast: 1.19, RefRms: .012}
	if _, ok := confirmCleanTV(moderate, AcousticResult{}); ok {
		t.Fatal("one moderate peak must not set the acoustic target")
	}
	support := AcousticResult{TvLagMs: 46, TvPsr: 5.6, Contrast: 1.24, RefRms: .007}
	if got, ok := confirmCleanTV(support, moderate); !ok || got.TvLagMs != 52 {
		t.Fatalf("expected supporting peak to confirm earlier clean measurement, got %+v", got)
	}
}
