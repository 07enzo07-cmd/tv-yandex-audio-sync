package main

import (
	"math"
	"sync"
	"time"
)

const (
	targetErrMs     = -1.5 // Haas window [-3, 0] ms: Station 1.5 ms behind TV keeps image on screen
	deadbandMs      = 4.0  // Do not move actuators when |e - e*| <= 4 ms
	maxTvDelayMs    = 50   // Lip-sync safe ceiling for TV speaker delay actuator
	tvStepMs        = 5    // Minimum actuator step for TV delay
	tvHysteresisMs  = 10.0 // Positive margin required before returning D_tv -> 0
	tvHysteresisDur = 10 * time.Second
	tvMinInterval   = 5 * time.Second
	ltvCacheTTL     = 10 * time.Minute

	// Standard dmix (1920/480 = 40ms buf, 10ms period) + aplay (1440/480 = 30ms buf) + 4KB pipe:
	// H_min ≈ 25..40 ms (auto-calibrated live from SpkLag - q_ms), Q_min = 15 ms -> L_st_min = 40 ms
	defaultAlsaBaseMs   = 25.0
	defaultMinQMs       = 15.0
	defaultMinStationMs = 40

	// Direct hw:0,0 low-latency mode (720/240 = 15ms buf, 5ms period = exact SYN3 frame) + 4KB pipe:
	// H_min ≈ 13 ms (auto-calibrated live from SpkLag - q_ms), Q_min = 8 ms -> L_st_min = 21 ms (covers PS5 ~21.5 ms with 0 XRUN!)
	hwAlsaBaseMs       = 13.0
	hwMinQMs           = 8.0
	hwMinStationMs     = 21
	hwAutoTriggerLtvMs = 45.0

	maxStationMs = 950
)

type TelemetrySnapshot struct {
	State          SyncState
	ClosedLoop     bool
	HwMode         bool
	CacheUsed      bool
	LtvMs          float64
	LstMs          float64
	ErrMs          float64
	Conf           float64
	LastValidAgeMs int64
	ValidCount     uint64
	TotalChecks    uint64
	Reason         string
	DstMs          int
	DtvMs          int
	ReqDtvMs       int
	QMs            float64
	DriftPpm       float64
}

type SyncController struct {
	mu sync.Mutex

	state             SyncState
	closedLoop        bool
	tvActuatorEnabled bool
	lowLatencyHwMode  bool

	alsaBaseMs   float64
	minQMs       float64
	minStationMs int

	// Current actuator setpoints
	dstMs       int // Station total target delay (ms)
	dtvMs       int // Confirmed TV speaker delay (ms)
	reqDtvMs    int // Requested TV speaker delay (ms)
	tvCmdSeq    uint8
	lastTvCmdAt time.Time

	// Hysteresis timer for returning D_tv -> 0
	zeroDtvMarginSince time.Time

	// Measured & cached L_tv (referenced to D_tv = 0)
	hasLtv      bool
	ltvBaseMs   float64 // L_tv at D_tv = 0
	ltvAt       time.Time
	lastLstMs   float64
	lastErrMs   float64
	lastConf    float64
	lastValidAt time.Time
	cacheUsed   bool
	reason      string

	validCount  uint64
	totalChecks uint64

	// Consecutive outside-overlap drift detections in TRACK before triggering REACQUIRE
	driftConfirmCount int
	lastDriftErr      float64

	// Separated Clock Drift PI state (±200 ppm)
	driftIntegral float64
	driftPpm      float64
}

func NewSyncController(initialDstMs int, closedLoop bool, tvActuatorEnabled bool) *SyncController {
	if initialDstMs < defaultMinStationMs {
		initialDstMs = defaultMinStationMs
	}
	if initialDstMs > maxStationMs {
		initialDstMs = maxStationMs
	}
	return &SyncController{
		state:             StateIdle,
		closedLoop:        closedLoop,
		tvActuatorEnabled: tvActuatorEnabled,
		alsaBaseMs:        defaultAlsaBaseMs,
		minQMs:            defaultMinQMs,
		minStationMs:      defaultMinStationMs,
		dstMs:             initialDstMs,
		reason:            "boot",
	}
}

func (c *SyncController) SetClosedLoop(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closedLoop = enabled
}

func (c *SyncController) SetTvActuatorEnabled(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tvActuatorEnabled = enabled
}

func (c *SyncController) SetStationDelayManual(delayMs int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if delayMs < c.minStationMs {
		delayMs = c.minStationMs
	}
	if delayMs > maxStationMs {
		delayMs = maxStationMs
	}
	c.dstMs = delayMs
	c.reason = "manual_step"
}

func (c *SyncController) SetLowLatencyHwMode(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setLowLatencyHwModeLocked(enabled)
}

func (c *SyncController) setLowLatencyHwModeLocked(enabled bool) {
	if c.lowLatencyHwMode == enabled {
		return
	}
	c.lowLatencyHwMode = enabled
	if enabled {
		c.alsaBaseMs = hwAlsaBaseMs
		c.minQMs = hwMinQMs
		c.minStationMs = hwMinStationMs
	} else {
		c.alsaBaseMs = defaultAlsaBaseMs
		c.minQMs = defaultMinQMs
		c.minStationMs = defaultMinStationMs
	}
	if c.dstMs < c.minStationMs {
		c.dstMs = c.minStationMs
	}
	if c.hasLtv {
		c.allocateActuatorsLocked(time.Now())
	}
}

func (c *SyncController) IsLowLatencyHwMode() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lowLatencyHwMode
}

func (c *SyncController) MinQueueMs() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.minQMs
}

func (c *SyncController) TargetDelayMs() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dstMs
}

func (c *SyncController) TargetQueueMs() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	q := float64(c.dstMs) - c.alsaBaseMs
	if q < c.minQMs {
		q = c.minQMs
	}
	return q
}

// UpdateMeasuredAlsaBase continuously auto-calibrates H_measured = SpkLagMs - curQMs
// so TargetQueueMs() always achieves exact lst == dstMs with sub-millisecond accuracy.
func (c *SyncController) UpdateMeasuredAlsaBase(spkLagMs, curQMs float64) {
	h := spkLagMs - curQMs
	if h < 4.0 || h > 95.0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.alsaBaseMs = 0.20*h + 0.80*c.alsaBaseMs
	c.minStationMs = int(math.Ceil(c.alsaBaseMs + c.minQMs))
}

func (c *SyncController) State() SyncState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *SyncController) EnterIdle(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = StateIdle
	c.driftConfirmCount = 0
	c.reason = reason
}

func (c *SyncController) EnterAcquire(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = StateAcquire
	c.driftConfirmCount = 0
	c.cacheUsed = false
	c.reason = reason
}

// HandleAcquireResult processes a clean-probe measurement during ACQUIRE and computes
// the optimal (Dst, Dtv) before transitioning to JOIN.
func (c *SyncController) HandleAcquireResult(res AcousticResult, now time.Time) (readyToJoin bool, newDstMs int, newReqDtvMs int, cmdSeq uint8) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.totalChecks++

	if !res.Valid {
		c.reason = "acquire_" + res.Status
		return false, c.dstMs, c.reqDtvMs, c.tvCmdSeq
	}

	c.validCount++
	c.hasLtv = true
	c.ltvBaseMs = res.TvLagMs - float64(c.dtvMs)
	c.ltvAt = now
	c.lastValidAt = now
	c.lastConf = res.TvConf
	c.cacheUsed = false

	// Auto-engage direct hw:0,0 low-latency mode if measured L_tv is below dmix floor (< 45 ms)
	if c.ltvBaseMs < hwAutoTriggerLtvMs && !c.lowLatencyHwMode {
		c.lowLatencyHwMode = true
		c.alsaBaseMs = hwAlsaBaseMs
		c.minQMs = hwMinQMs
		c.minStationMs = hwMinStationMs
	}

	c.allocateActuatorsLocked(now)
	c.state = StateJoin
	c.reason = "acquire_locked"
	return true, c.dstMs, c.reqDtvMs, c.tvCmdSeq
}

// HandleAcquireTimeout falls back to fresh L_tv cache (<= 10 min) or current calibrated delay.
func (c *SyncController) HandleAcquireTimeout(now time.Time) (newDstMs int, newReqDtvMs int, cmdSeq uint8) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hasLtv && now.Sub(c.ltvAt) <= ltvCacheTTL {
		c.cacheUsed = true
		c.allocateActuatorsLocked(now)
		c.reason = "acquire_timeout_cache"
	} else {
		c.cacheUsed = false
		c.reason = "acquire_timeout_fallback"
	}
	c.state = StateJoin
	return c.dstMs, c.reqDtvMs, c.tvCmdSeq
}

func (c *SyncController) MarkJoined() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == StateJoin {
		// Check if we are in UNREACHABLE deficit
		if c.hasLtv && (c.ltvBaseMs+float64(c.dtvMs)) < float64(c.minStationMs)-5.0 {
			c.state = StateUnreachable
			c.reason = "deficit_below_lst_min"
		} else {
			c.state = StateTrack
			c.reason = "joined"
		}
	}
}

// HandleTrackResult processes periodic TRACK measurements without destructive zero-lag resets.
// Returns triggerReacquire=true if a persistent large shift (>15 ms) is detected outside overlap.
func (c *SyncController) HandleTrackResult(res AcousticResult, now time.Time) (triggerReacquire bool, newDstMs int, newReqDtvMs int, cmdSeq uint8) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.totalChecks++

	if res.SpkConf >= 0.55 {
		c.lastLstMs = res.SpkLagMs
	}

	if res.InOverlap {
		c.driftConfirmCount = 0
		if c.hasLtv && (c.ltvBaseMs+float64(c.dtvMs)) < float64(c.minStationMs)-5.0 {
			c.state = StateUnreachable
			c.reason = "hold_unreachable"
		} else {
			c.state = StateHold
			c.reason = "locked_in_overlap"
		}
		return false, c.dstMs, c.reqDtvMs, c.tvCmdSeq
	}

	if !res.Valid {
		c.reason = "track_" + res.Status
		return false, c.dstMs, c.reqDtvMs, c.tvCmdSeq
	}

	c.validCount++
	c.lastValidAt = now
	c.lastConf = res.TvConf
	c.lastErrMs = res.AcousticErr
	c.hasLtv = true
	c.ltvBaseMs = res.TvLagMs - float64(c.dtvMs)
	c.ltvAt = now

	if !c.closedLoop {
		c.state = StateTrack
		c.reason = "observe_only"
		return false, c.dstMs, c.reqDtvMs, c.tvCmdSeq
	}

	errFromTarget := res.AcousticErr - targetErrMs
	if math.Abs(errFromTarget) <= deadbandMs {
		c.driftConfirmCount = 0
		c.state = StateTrack
		c.reason = "in_deadband"
		return false, c.dstMs, c.reqDtvMs, c.tvCmdSeq
	}

	// Persistent shift > 15 ms outside overlap -> confirm with 2 independent windows
	if math.Abs(errFromTarget) > 15.0 {
		if c.driftConfirmCount > 0 && math.Abs(res.AcousticErr-c.lastDriftErr) <= 12.0 {
			c.driftConfirmCount++
		} else {
			c.driftConfirmCount = 1
		}
		c.lastDriftErr = res.AcousticErr
		if c.driftConfirmCount >= 2 {
			// Check if we can adjust smoothly directly, or need clean REACQUIRE (>35 ms)
			if math.Abs(errFromTarget) > 35.0 {
				c.driftConfirmCount = 0
				c.state = StateReacquire
				c.reason = "large_shift_reacquire"
				return true, c.dstMs, c.reqDtvMs, c.tvCmdSeq
			}
			c.driftConfirmCount = 0
			c.allocateActuatorsLocked(now)
			return false, c.dstMs, c.reqDtvMs, c.tvCmdSeq
		}
		c.reason = "confirming_shift"
		return false, c.dstMs, c.reqDtvMs, c.tvCmdSeq
	}

	// Medium error (4..15 ms): allocate actuators smoothly
	c.driftConfirmCount = 0
	c.allocateActuatorsLocked(now)
	return false, c.dstMs, c.reqDtvMs, c.tvCmdSeq
}

// HandleTvap updates confirmed TV speaker delay from TV's TVAP ack.
func (c *SyncController) HandleTvap(tvap TvapPacket) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if tvap.Status == tvapStatusUnsupported || tvap.Status == tvapStatusError {
		c.tvActuatorEnabled = false
		c.reqDtvMs = 0
		c.dtvMs = 0
		c.reason = "tv_actuator_unsupported"
		return
	}
	c.dtvMs = int(tvap.AppliedTvDelayMs)
}

// allocateActuatorsLocked implements the non-oscillating two-actuator allocation policy:
// Priority 1: Station delay (Dst).
// Priority 2: TV speaker delay (Dtv) ONLY if tvActuatorEnabled and need0 < 0.
// Returns Dtv -> 0 ONLY when forecast at Dtv=0 has >= 10 ms margin for >= 10 s.
func (c *SyncController) allocateActuatorsLocked(now time.Time) {
	if !c.closedLoop || !c.hasLtv {
		return
	}
	// Desired Station total delay to achieve e* = targetErrMs (<0 means Station slightly behind TV):
	// e = L_tv - L_st = targetErrMs  =>  L_st_desired = L_tv - targetErrMs = L_tv + |targetErrMs|
	need0 := (c.ltvBaseMs - targetErrMs) - float64(c.minStationMs)

	if need0 >= 0 {
		// Station alone can cover the required delay
		if c.reqDtvMs > 0 {
			if need0 >= tvHysteresisMs {
				if c.zeroDtvMarginSince.IsZero() {
					c.zeroDtvMarginSince = now
				}
				if now.Sub(c.zeroDtvMarginSince) >= tvHysteresisDur && now.Sub(c.lastTvCmdAt) >= tvMinInterval {
					c.reqDtvMs = 0
					c.tvCmdSeq++
					c.lastTvCmdAt = now
					c.zeroDtvMarginSince = time.Time{}
				}
			} else {
				c.zeroDtvMarginSince = time.Time{}
			}
		}
		desiredDst := int(math.Round(c.ltvBaseMs + float64(c.dtvMs) - targetErrMs))
		if desiredDst < c.minStationMs {
			desiredDst = c.minStationMs
		}
		if desiredDst > maxStationMs {
			desiredDst = maxStationMs
		}
		c.dstMs = desiredDst
		c.state = StateTrack
		c.reason = "station_allocated"
		return
	}

	// need0 < 0: TV is faster than Station's physical floor L_st_min
	c.zeroDtvMarginSince = time.Time{}
	c.dstMs = c.minStationMs

	if !c.tvActuatorEnabled {
		c.reqDtvMs = 0
		if -need0 > 5.0 {
			c.state = StateUnreachable
			c.reason = "tv_faster_than_lst_min"
		} else {
			c.state = StateTrack
			c.reason = "near_lst_min"
		}
		return
	}

	// Request minimum TV delay step to close deficit
	rawReq := int(math.Round((-need0)/float64(tvStepMs))) * tvStepMs
	if rawReq < 0 {
		rawReq = 0
	}
	if rawReq > maxTvDelayMs {
		rawReq = maxTvDelayMs
	}
	if rawReq != c.reqDtvMs && now.Sub(c.lastTvCmdAt) >= tvMinInterval {
		c.reqDtvMs = rawReq
		c.tvCmdSeq++
		c.lastTvCmdAt = now
	}
	remainingDeficit := -need0 - float64(c.reqDtvMs)
	if remainingDeficit > 5.0 {
		c.state = StateUnreachable
		c.reason = "deficit_exceeds_dtv_max"
	} else {
		c.state = StateTrack
		c.reason = "dual_actuator_allocated"
	}
}

// UpdateClockDriftPI updates the slow crystal drift loop (±200 ppm max) from smoothed queue error (ms).
func (c *SyncController) UpdateClockDriftPI(qErrMs float64, dtSec float64) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if dtSec <= 0 || dtSec > 5.0 {
		return c.driftPpm
	}
	// Deadband ±1.5 ms for clock drift
	if math.Abs(qErrMs) < 1.5 {
		return c.driftPpm
	}
	const kp = 4.0  // ppm per ms
	const ki = 0.25 // ppm per (ms*s)
	const maxPpm = 200.0

	c.driftIntegral += qErrMs * ki * dtSec
	if c.driftIntegral > maxPpm {
		c.driftIntegral = maxPpm
	} else if c.driftIntegral < -maxPpm {
		c.driftIntegral = -maxPpm
	}
	ppm := kp*qErrMs + c.driftIntegral
	if ppm > maxPpm {
		ppm = maxPpm
	} else if ppm < -maxPpm {
		ppm = -maxPpm
	}
	c.driftPpm = ppm
	return ppm
}

// Snapshot returns an atomic, consistent view of telemetry without zeroing conf while keeping stale err.
func (c *SyncController) Snapshot(curQMs float64, now time.Time) TelemetrySnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ageMs int64 = -1
	if !c.lastValidAt.IsZero() {
		ageMs = now.Sub(c.lastValidAt).Milliseconds()
	}
	return TelemetrySnapshot{
		State:          c.state,
		ClosedLoop:     c.closedLoop,
		HwMode:         c.lowLatencyHwMode,
		CacheUsed:      c.cacheUsed,
		LtvMs:          c.ltvBaseMs + float64(c.dtvMs),
		LstMs:          c.lastLstMs,
		ErrMs:          c.lastErrMs,
		Conf:           c.lastConf,
		LastValidAgeMs: ageMs,
		ValidCount:     c.validCount,
		TotalChecks:    c.totalChecks,
		Reason:         c.reason,
		DstMs:          c.dstMs,
		DtvMs:          c.dtvMs,
		ReqDtvMs:       c.reqDtvMs,
		QMs:            curQMs,
		DriftPpm:       c.driftPpm,
	}
}
