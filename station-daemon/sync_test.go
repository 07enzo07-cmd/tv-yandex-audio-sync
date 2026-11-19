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

	// 1. Acquire with L_tv = 20 ms (< L_st_min 40 ms) -> Station to 40 ms, TV delay requested ~20 ms
	ok, dst, reqDtv, seq := ctrl.HandleAcquireResult(AcousticResult{Valid: true, TvLagMs: 20.0, TvConf: 0.9}, now)
	if !ok || dst != 40 || reqDtv != 20 || seq != 1 {
		t.Fatalf("expected dst=40 reqDtv=20 seq=1, got ok=%t dst=%d reqDtv=%d seq=%d", ok, dst, reqDtv, seq)
	}
	ctrl.HandleTvap(TvapPacket{AppliedTvDelayMs: 20, Status: tvapStatusOK, CmdSeq: seq})
	ctrl.MarkJoined()
	if ctrl.State() != StateTrack {
		t.Fatalf("expected StateTrack after TVAP, got %s", ctrl.State())
	}

	// 2. Suppose L_tv rises slightly to 45 ms (margin only +6.5 ms < 10 ms hysteresis) -> MUST NOT drop D_tv to 0!
	_, _, reqDtv2, _ := ctrl.HandleTrackResult(AcousticResult{
		Valid: true, SpkConf: 0.9, SpkLagMs: 40.0, TvLagMs: 65.0, AcousticErr: 25.0, TvConf: 0.9,
	}, now.Add(6*time.Second))
	if reqDtv2 != 20 {
		t.Fatalf("hysteresis violated: D_tv should remain 20 ms when margin < 10 ms, got %d", reqDtv2)
	}

	// 3. Test UNREACHABLE when TV actuator disabled and L_tv = 15 ms < L_st_min 40 ms
	ctrlNoTv := NewSyncController(115, true, false)
	ctrlNoTv.EnterAcquire("start")
	ctrlNoTv.HandleAcquireResult(AcousticResult{Valid: true, TvLagMs: 15.0, TvConf: 0.9}, now)
	ctrlNoTv.MarkJoined()
	if ctrlNoTv.State() != StateUnreachable || ctrlNoTv.TargetDelayMs() != 40 {
		t.Fatalf("expected UNREACHABLE at dst=40, got state=%s dst=%d", ctrlNoTv.State(), ctrlNoTv.TargetDelayMs())
	}
}
