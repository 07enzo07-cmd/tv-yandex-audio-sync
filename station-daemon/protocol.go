package main

import (
	"encoding/binary"
	"math"
)

const (
	magicSYNC = 0x53594E43 // 'SYNC' legacy v2 (16B header + 1920B PCM)
	magicSYN3 = 0x53594E33 // 'SYN3' v3 (20B header + 960B PCM = 980B UDP, no IPv4 frag)
	magicSACK = 0x5341434B // 'SACK' legacy 12B telemetry
	magicSAK2 = 0x53414B32 // 'SAK2' v3 18B telemetry
	magicTVAP = 0x54564150 // 'TVAP' v3 8B TV delay apply ack

	syn3HeaderLen = 20
	syncHeaderLen = 16

	syn3FlagMuted      = 1 << 0
	syn3FlagReset      = 1 << 1
	syn3FlagLowLatency = 1 << 2

	sak2FlagClosedLoop = 1 << 0
	sak2FlagValid      = 1 << 1
	sak2FlagCacheUsed  = 1 << 2

	tvapStatusOK          = 0
	tvapStatusClamped     = 1
	tvapStatusUnsupported = 2
	tvapStatusError       = 3
)

// SyncState represents the station-rx finite state machine.
type SyncState uint8

const (
	StateIdle SyncState = iota
	StateAcquire
	StateJoin
	StateTrack
	StateReacquire
	StateHold
	StateUnreachable
)

func (s SyncState) String() string {
	switch s {
	case StateIdle:
		return "IDLE"
	case StateAcquire:
		return "ACQUIRE"
	case StateJoin:
		return "JOIN"
	case StateTrack:
		return "TRACK"
	case StateReacquire:
		return "REACQUIRE"
	case StateHold:
		return "HOLD"
	case StateUnreachable:
		return "UNREACHABLE"
	default:
		return "UNKNOWN"
	}
}

type AudioPacket struct {
	ProtoVer      uint8 // 2 or 3
	Seq           uint32
	SampleIndex   uint32
	CaptureTsMs   uint32
	TvVol         int
	StationVolPct int // 0..100 exact percentage
	IsMuted       bool
	SessionReset  bool
	LowLatency    bool
	FrameMs       float64
	IsPLC         bool
	PCM           []byte
}

type TvapPacket struct {
	AppliedTvDelayMs uint16
	Status           uint8
	CmdSeq           uint8
}

type Sak2Packet struct {
	State          SyncState
	Flags          uint8
	ReqTvDelayMs   uint16
	StationDelayMs uint16
	LtvTenths      uint16
	ErrTenths      int16
	ConfPermille   uint16
	CmdSeq         uint8
}

// ParseAudioPacket parses either SYN3 (v3) or SYNC (legacy Android v2) packets.
func ParseAudioPacket(buf []byte) (AudioPacket, bool) {
	if len(buf) < 4 {
		return AudioPacket{}, false
	}
	magic := binary.BigEndian.Uint32(buf[0:4])
	switch magic {
	case magicSYN3:
		if len(buf) < syn3HeaderLen+192 { // at least 1ms PCM
			return AudioPacket{}, false
		}
		seq := binary.BigEndian.Uint32(buf[4:8])
		sampleIdx := binary.BigEndian.Uint32(buf[8:12])
		capTs := binary.BigEndian.Uint32(buf[12:16])
		tvVol := int(buf[16])
		stPct := int(buf[17])
		if stPct > 100 {
			stPct = 100
		}
		flags := buf[18]
		frameMsX2 := buf[19]
		frameMs := float64(frameMsX2) / 2.0
		if frameMs <= 0 {
			frameMs = float64(len(buf)-syn3HeaderLen) / (48.0 * 4.0)
		}
		pcm := make([]byte, len(buf)-syn3HeaderLen)
		copy(pcm, buf[syn3HeaderLen:])
		return AudioPacket{
			ProtoVer:      3,
			Seq:           seq,
			SampleIndex:   sampleIdx,
			CaptureTsMs:   capTs,
			TvVol:         tvVol,
			StationVolPct: stPct,
			IsMuted:       (flags & syn3FlagMuted) != 0,
			SessionReset:  (flags & syn3FlagReset) != 0,
			LowLatency:    (flags & syn3FlagLowLatency) != 0,
			FrameMs:       frameMs,
			PCM:           pcm,
		}, true

	case magicSYNC:
		if len(buf) < syncHeaderLen+192 {
			return AudioPacket{}, false
		}
		seq := binary.BigEndian.Uint32(buf[4:8])
		capTs := binary.BigEndian.Uint32(buf[8:12])
		tvVol := int(buf[12] & 0x7F)
		stVolStep := int(buf[13])
		stPct := stVolStep * 10
		if stPct > 100 {
			stPct = 100
		}
		isMuted := buf[14] != 0
		if isMuted || stVolStep == 0 {
			stPct = 0
		}
		pcm := make([]byte, len(buf)-syncHeaderLen)
		copy(pcm, buf[syncHeaderLen:])
		frames48 := uint32(len(pcm) / 4)
		return AudioPacket{
			ProtoVer:      2,
			Seq:           seq,
			SampleIndex:   seq * frames48,
			CaptureTsMs:   capTs,
			TvVol:         tvVol,
			StationVolPct: stPct,
			IsMuted:       isMuted,
			FrameMs:       float64(frames48) / 48.0,
			PCM:           pcm,
		}, true
	default:
		return AudioPacket{}, false
	}
}

func ParseTvapPacket(buf []byte) (TvapPacket, bool) {
	if len(buf) < 8 || binary.BigEndian.Uint32(buf[0:4]) != magicTVAP {
		return TvapPacket{}, false
	}
	return TvapPacket{
		AppliedTvDelayMs: binary.BigEndian.Uint16(buf[4:6]),
		Status:           buf[6],
		CmdSeq:           buf[7],
	}, true
}

func EncodeSak2(p Sak2Packet) [18]byte {
	var b [18]byte
	binary.BigEndian.PutUint32(b[0:4], magicSAK2)
	b[4] = uint8(p.State)
	b[5] = p.Flags
	binary.BigEndian.PutUint16(b[6:8], p.ReqTvDelayMs)
	binary.BigEndian.PutUint16(b[8:10], p.StationDelayMs)
	binary.BigEndian.PutUint16(b[10:12], p.LtvTenths)
	binary.BigEndian.PutUint16(b[12:14], uint16(p.ErrTenths))
	binary.BigEndian.PutUint16(b[14:16], p.ConfPermille)
	b[16] = p.CmdSeq
	b[17] = 0
	return b
}

func EncodeLegacySack(reqTvDelayMs, stationDelayMs uint16, errTenths int16, confPermille uint16) [12]byte {
	var b [12]byte
	binary.BigEndian.PutUint32(b[0:4], magicSACK)
	binary.BigEndian.PutUint16(b[4:6], reqTvDelayMs)
	binary.BigEndian.PutUint16(b[6:8], stationDelayMs)
	binary.BigEndian.PutUint16(b[8:10], uint16(errTenths))
	binary.BigEndian.PutUint16(b[10:12], confPermille)
	return b
}

// ReorderBuffer handles up to windowSize out-of-order UDP packets, drops duplicates,
// and emits PLC silence markers on packet loss.
type ReorderBuffer struct {
	windowSize int
	hasInit    bool
	expected   uint32
	slots      map[uint32]AudioPacket
	LossCount  uint64
	ReorderCnt uint64
	DupCount   uint64
}

func NewReorderBuffer(windowSize int) *ReorderBuffer {
	if windowSize < 1 {
		windowSize = 3
	}
	return &ReorderBuffer{
		windowSize: windowSize,
		slots:      make(map[uint32]AudioPacket, windowSize+2),
	}
}

func (rb *ReorderBuffer) Reset() {
	rb.hasInit = false
	rb.expected = 0
	for k := range rb.slots {
		delete(rb.slots, k)
	}
}

// Push inserts a packet and returns zero or more ordered packets ready for playback/DSP.
func (rb *ReorderBuffer) Push(pkt AudioPacket) (ready []AudioPacket, majorReset bool) {
	if !rb.hasInit || pkt.SessionReset {
		rb.Reset()
		rb.hasInit = true
		rb.expected = pkt.Seq + 1
		return []AudioPacket{pkt}, pkt.SessionReset
	}

	diff := int32(pkt.Seq - rb.expected)
	if diff < 0 {
		// Duplicate or late packet behind expected
		if diff < -50 {
			// Wrap/restart
			rb.Reset()
			rb.hasInit = true
			rb.expected = pkt.Seq + 1
			return []AudioPacket{pkt}, true
		}
		rb.DupCount++
		return nil, false
	}

	if diff > 50 {
		// Major seek / discontinuity
		rb.Reset()
		rb.hasInit = true
		rb.expected = pkt.Seq + 1
		return []AudioPacket{pkt}, true
	}

	if diff == 0 {
		ready = append(ready, pkt)
		rb.expected++
		for {
			next, ok := rb.slots[rb.expected]
			if !ok {
				break
			}
			delete(rb.slots, rb.expected)
			ready = append(ready, next)
			rb.expected++
		}
		return ready, false
	}

	// Future packet (diff > 0): buffer it
	if _, exists := rb.slots[pkt.Seq]; exists {
		rb.DupCount++
		return nil, false
	}
	rb.slots[pkt.Seq] = pkt
	rb.ReorderCnt++

	// If gap exceeds windowSize, advance expected and emit PLC markers for missing packets
	for int32(pkt.Seq-rb.expected) >= int32(rb.windowSize) {
		if existing, ok := rb.slots[rb.expected]; ok {
			delete(rb.slots, rb.expected)
			ready = append(ready, existing)
		} else {
			rb.LossCount++
			plc := AudioPacket{
				ProtoVer: pkt.ProtoVer,
				Seq:      rb.expected,
				FrameMs:  pkt.FrameMs,
				IsPLC:    true,
				PCM:      make([]byte, len(pkt.PCM)),
			}
			ready = append(ready, plc)
		}
		rb.expected++
	}
	// Drain any contiguous buffered packets
	for {
		next, ok := rb.slots[rb.expected]
		if !ok {
			break
		}
		delete(rb.slots, rb.expected)
		ready = append(ready, next)
		rb.expected++
	}
	return ready, false
}

// PCMRMS computes RMS (0..1) of 16-bit LE stereo PCM.
func PCMRMS(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sumSq float64
	for i := 0; i+1 < len(pcm); i += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[i:i+2]))) / 32768.0
		sumSq += v * v
	}
	return math.Sqrt(sumSq / float64(n))
}
