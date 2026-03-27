package congestion

import (
	"log"
	"math/rand"
	"time"

	"github.com/quic-go/quic-go/internal/monotime"
	"github.com/quic-go/quic-go/internal/protocol"
)

// BBRv3 Constants based on draft-cardwell-iccrg-bbr-congestion-control-02
const (
	// Startup pacing gain (4*ln(2) ~= 2.77)
	bbr3StartupPacingGain = 2.77

	// Pacing margin percent (1% discount)
	bbr3PacingMarginPercent = 0.01

	// Loss threshold (2%)
	bbr3LossThreshold = 0.02

	// Full loss count - number of loss events to exit startup
	bbr3FullLossCount = 6

	// Beta - multiplicative decrease factor (0.7)
	bbr3Beta = 0.7

	// Headroom - multiplicative factor for inflight headroom (0.85)
	bbr3Headroom = 0.85

	// Min pipe cwnd in SMSS (4 packets)
	bbr3MinPipeCwnd = 4

	// Extra acked filter length (10 RTTs)
	bbr3ExtraAckedFilterLen = 10

	// Min RTT filter length (10 seconds)
	bbr3MinRttFilterLen = 10 * time.Second

	// Probe RTT duration (200ms)
	bbr3ProbeRttDuration = 200 * time.Millisecond

	// Probe RTT interval (5 seconds)
	bbr3ProbeRttInterval = 5 * time.Second

	// Full bandwidth count threshold
	bbr3FullBwCountThreshold = 3

	// Full bandwidth growth rate (25%)
	bbr3FullBwGrowthRate = 0.25

	// Probe BW max rounds
	bbr3ProbeBwMaxRounds = 63

	// Probe BW random rounds
	bbr3ProbeBwRandRounds = 2

	// Probe BW min wait time (2 seconds)
	bbr3ProbeBwMinWaitTime = 2 * time.Second

	// Probe BW max wait time (3 seconds)
	bbr3ProbeBwMaxWaitTime = 3 * time.Second

	// Send quantum threshold pacing rate (1.2 Mbps)
	bbr3SendQuantumThreshold = 1200000 / 8

	// Max send quantum (64KB)
	bbr3MaxSendQuantum = 64 * 1024
)

// BBRv3 States
type bbr3State int8

const (
	bbr3StateStartup bbr3State = iota
	bbr3StateDrain
	bbr3StateProbeBwDown
	bbr3StateProbeBwCruise
	bbr3StateProbeBwRefill
	bbr3StateProbeBwUp
	bbr3StateProbeRtt
)

// AckProbePhase for tracking ACK probing state
type ackProbePhase int8

const (
	ackProbeInit ackProbePhase = iota
	ackProbeStopping
	ackProbeRefilling
	ackProbeStarting
	ackProbeFeedback
)

// bbr3AckState tracks accumulated information from a single ACK
type bbr3AckState struct {
	now                       monotime.Time
	newlyLostBytes            protocol.ByteCount
	newlyAckedBytes           protocol.ByteCount
	packetDelivered           uint64
	lastAckPacketSentTime     monotime.Time
	priorBytesInFlight        protocol.ByteCount
	txInFlight                protocol.ByteCount
	lost                      protocol.ByteCount
}

// roundTripCounter tracks packet-timed round trips
type roundTripCounter struct {
	roundCount       uint64
	isRoundStart     bool
	nextRoundDelivered uint64
}

// fullPipeEstimator tracks whether the pipe is filled
type fullPipeEstimator struct {
	isFilledPipe bool
	fullBw       uint64
	fullBwCount  uint64
}

// minMaxFilter is a simple windowed max filter
type minMaxFilter struct {
	windowSize uint64
	samples    map[uint64]uint64
	windowStart uint64
}

func newMinMaxFilter(windowSize uint64) *minMaxFilter {
	return &minMaxFilter{
		windowSize: windowSize,
		samples:    make(map[uint64]uint64),
	}
}

func (f *minMaxFilter) update(round uint64, value uint64) {
	f.samples[round] = max(f.samples[round], value)

	// Remove old samples outside the window
	for k := range f.samples {
		if k+ f.windowSize < round {
			delete(f.samples, k)
		}
	}
	f.windowStart = round
}

func (f *minMaxFilter) get() uint64 {
	maxVal := uint64(0)
	for _, v := range f.samples {
		if v > maxVal {
			maxVal = v
		}
	}
	return maxVal
}

// BBRv3Sender implements BBRv3 congestion control
type BBRv3Sender struct {
	// Configuration
	initialCwnd        protocol.ByteCount
	minCwnd            protocol.ByteCount
	maxDatagramSize    protocol.ByteCount

	// State
	state              bbr3State
	round              roundTripCounter
	fullPipe           fullPipeEstimator
	ackPhase           ackProbePhase

	// Pacing
	pacingRate         uint64
	pacingGain         float64
	cwndGain           float64
	minPacingRate      uint64
	sendQuantum        uint64
	packetConservation bool

	// Congestion window
	cwnd               protocol.ByteCount
	priorCwnd          protocol.ByteCount

	// Bandwidth estimates
	maxBw              uint64
	bwHi               uint64
	bwLo               uint64
	bw                 uint64
	bwLatest           uint64

	// RTT estimates
	minRtt             time.Duration
	minRttStamp        monotime.Time
	probeRttMinDelay   time.Duration
	probeRttMinStamp   monotime.Time
	probeRttExpired    bool
	probeRttDoneStamp  monotime.Time
	probeRttRoundDone  bool

	// Inflight estimates
	bdpCache           uint64
	extraAcked         uint64
	offloadBudget      uint64
	maxInflight        uint64
	inflightHi         uint64
	inflightLo         uint64
	inflightLatest     uint64

	// Filters
	extraAckedFilter   *minMaxFilter

	// Cycle tracking
	cycleCount         uint64
	cycleStamp         monotime.Time

	// Bandwidth probing
	roundsSinceBwProbe uint64
	bwProbeWait        time.Duration
	bwProbeUpCnt       uint64
	bwProbeUpAcks      uint64
	bwProbeUpRounds    uint64
	bwProbeSamples     bool

	// Loss tracking
	lossRoundStart     bool
	lossInRound        bool
	lossRoundDelivered uint64
	lossEventsInRound  uint64

	// Recovery
	inRecovery         bool
	recoveryEpochStart monotime.Time

	// Delivery tracking
	delivered          uint64
	extraAckedDelivered uint64
	extraAckedIntervalStart monotime.Time

	// Idle tracking
	idleRestart        bool

	// Sent packet tracking
	sentTimes          map[protocol.PacketNumber]monotime.Time

	// Initial time
	initTime           monotime.Time

	// Statistics collection
	stats              *BBRv3Stats
	statsEnabled       bool
}

// Verify BBRv3Sender implements the interface
var _ SendAlgorithm = (*BBRv3Sender)(nil)
var _ SendAlgorithmWithDebugInfos = (*BBRv3Sender)(nil)

// NewBBRv3Sender creates a new BBRv3 congestion controller
func NewBBRv3Sender(initialMaxDatagramSize protocol.ByteCount) *BBRv3Sender {
	now := monotime.Now()

	initialCwnd := 80 * initialMaxDatagramSize
	minCwnd := 4 * initialMaxDatagramSize

	b := &BBRv3Sender{
		initialCwnd:             initialCwnd,
		minCwnd:                 minCwnd,
		maxDatagramSize:         initialMaxDatagramSize,
		state:                   bbr3StateStartup,
		pacingGain:              bbr3StartupPacingGain,
		cwndGain:                2.0,
		cwnd:                    initialCwnd,
		minRtt:                  time.Duration(^uint64(0) >> 1), // Max duration
		minRttStamp:             now,
		probeRttMinDelay:        time.Duration(^uint64(0) >> 1),
		probeRttMinStamp:        now,
		inflightHi:              ^uint64(0),
		inflightLo:              ^uint64(0),
		bwHi:                    ^uint64(0),
		bwLo:                    ^uint64(0),
		extraAckedFilter:        newMinMaxFilter(bbr3ExtraAckedFilterLen),
		cycleStamp:              now,
		extraAckedIntervalStart: now,
		initTime:                now,
		sentTimes:               make(map[protocol.PacketNumber]monotime.Time),
		bwProbeWait:             time.Duration(^uint64(0) >> 1),
		bwProbeUpCnt:            ^uint64(0),
	}

	b.initPacingRate()
	log.Printf("[BBRv3] Created new BBRv3 sender: initialCwnd=%d, minCwnd=%d, maxDatagramSize=%d", initialCwnd, minCwnd, initialMaxDatagramSize)
	return b
}

// NewBBRv3SenderWithStats creates a new BBRv3 congestion controller with statistics collection
func NewBBRv3SenderWithStats(initialMaxDatagramSize protocol.ByteCount, statsConfig *BBRv3StatsConfig) *BBRv3Sender {
	b := NewBBRv3Sender(initialMaxDatagramSize)

	if statsConfig != nil && statsConfig.Enabled {
		b.stats = NewBBRv3Stats(statsConfig)
		b.statsEnabled = true
		b.stats.UpdateMinCwnd(b.minCwnd)
		b.stats.UpdateCwnd(b.cwnd)
		b.stats.UpdateSsthresh(protocol.ByteCount(^uint64(0) >> 1)) // Initially undefined
		b.stats.Start()
	}

	return b
}

// SetStatsConfig sets or updates the statistics configuration
func (b *BBRv3Sender) SetStatsConfig(config *BBRv3StatsConfig) {
	if b.stats != nil {
		b.stats.Stop()
	}

	if config != nil && config.Enabled {
		b.stats = NewBBRv3Stats(config)
		b.statsEnabled = true
		b.stats.UpdateMinCwnd(b.minCwnd)
		b.stats.UpdateCwnd(b.cwnd)
		b.stats.UpdateSsthresh(protocol.ByteCount(^uint64(0) >> 1))
		b.stats.UpdateMaxBandwidth(b.maxBw)
		b.stats.UpdatePacingRate(b.pacingRate)
		b.stats.UpdateState(b.stateName())
		b.stats.Start()
	} else {
		b.stats = nil
		b.statsEnabled = false
	}
}

// GetStats returns the current statistics snapshot
func (b *BBRv3Sender) GetStats() BBRv3StatsSnapshot {
	if b.stats != nil {
		return b.stats.GetStats()
	}
	return BBRv3StatsSnapshot{}
}

// StopStats stops the statistics collection
func (b *BBRv3Sender) StopStats() {
	if b.stats != nil {
		b.stats.Stop()
	}
}

// Name returns the name of the congestion control algorithm
func (b *BBRv3Sender) Name() string {
	return "BBRv3"
}

// initPacingRate initializes the pacing rate based on initial cwnd and RTT
func (b *BBRv3Sender) initPacingRate() {
	srtt := time.Millisecond // Default 1ms until we have real RTT
	nominalBandwidth := float64(b.initialCwnd) / srtt.Seconds()
	b.pacingRate = uint64(b.pacingGain * nominalBandwidth)
}

// setPacingRate sets the pacing rate with the current gain
func (b *BBRv3Sender) setPacingRate() {
	b.setPacingRateWithGain(b.pacingGain)
}

// setPacingRateWithGain sets the pacing rate with a specific gain
func (b *BBRv3Sender) setPacingRateWithGain(gain float64) {
	rate := uint64(gain * float64(b.bw) * (1.0 - bbr3PacingMarginPercent))

	if b.fullPipe.isFilledPipe || rate > b.pacingRate {
		b.pacingRate = rate
	}
}

// setSendQuantum sets the send quantum based on pacing rate
func (b *BBRv3Sender) setSendQuantum() {
	floor := b.maxDatagramSize
	if b.pacingRate >= bbr3SendQuantumThreshold {
		floor = 2 * b.maxDatagramSize
	}

	quantum := b.pacingRate / 1000 // 1ms of data
	if quantum < uint64(floor) {
		quantum = uint64(floor)
	}
	if quantum > bbr3MaxSendQuantum {
		quantum = bbr3MaxSendQuantum
	}
	b.sendQuantum = quantum
}

// bdp calculates BDP = bw * min_rtt
func (b *BBRv3Sender) bdp(bw uint64) uint64 {
	if b.minRtt == time.Duration(^uint64(0)>>1) {
		return uint64(b.initialCwnd)
	}
	return uint64(float64(bw) * b.minRtt.Seconds())
}

// bdpMultiple calculates BDP * gain
func (b *BBRv3Sender) bdpMultiple(bw uint64, gain float64) uint64 {
	return uint64(float64(b.bdp(bw)) * gain)
}

// inflight calculates target inflight with given gain
func (b *BBRv3Sender) inflight(gain float64) uint64 {
	return b.bdpMultiple(b.maxBw, gain)
}

// inflightWithHeadroom returns inflight with headroom for other flows
func (b *BBRv3Sender) inflightWithHeadroom() uint64 {
	if b.inflightHi == ^uint64(0) {
		return ^uint64(0)
	}
	headroom := uint64(bbr3Headroom * float64(b.inflightHi))
	if headroom < 1 {
	headroom = 1
	}
	result := b.inflightHi - headroom
	if result < uint64(b.minCwnd) {
		result = uint64(b.minCwnd)
	}
	return result
}

// targetInflight returns the target inflight
func (b *BBRv3Sender) targetInflight() uint64 {
	bdp := b.bdp(b.bw)
	cwnd := uint64(b.cwnd)
	if bdp < cwnd {
		return bdp
	}
	return cwnd
}

// setCwnd sets the congestion window
func (b *BBRv3Sender) setCwnd(bytesInFlight protocol.ByteCount) {
	b.updateMaxInflight()
	b.modulateCwndForRecovery(bytesInFlight)

	if !b.packetConservation {
		if b.fullPipe.isFilledPipe {
			maxInflight := b.maxInflight
			if uint64(b.cwnd)+uint64(b.ackState().newlyAckedBytes) < maxInflight {
				b.cwnd = protocol.ByteCount(uint64(b.cwnd) + uint64(b.ackState().newlyAckedBytes))
			} else {
				b.cwnd = protocol.ByteCount(maxInflight)
			}
		} else if uint64(b.cwnd) < b.maxInflight || b.delivered < uint64(b.initialCwnd) {
			b.cwnd += b.ackState().newlyAckedBytes
		}
		if b.cwnd < b.minCwnd {
			b.cwnd = b.minCwnd
		}
	}

	b.boundCwndForProbeRtt()
	b.boundCwndForModel()
}

// updateMaxInflight updates the max inflight estimate
func (b *BBRv3Sender) updateMaxInflight() {
	b.updateOffloadBudget()

	inflight := b.bdpMultiple(b.maxBw, b.cwndGain)
	inflight += b.extraAckedFilter.get()

	// Quantization budget
	inflight = max(inflight, uint64(b.offloadBudget))
	inflight = max(inflight, uint64(b.minCwnd))

	if b.state == bbr3StateProbeBwUp {
		inflight += 2 * uint64(b.maxDatagramSize)
	}

	b.maxInflight = inflight
}

// updateOffloadBudget estimates offload budget for TSO/GSO/LRO/GRO
func (b *BBRv3Sender) updateOffloadBudget() {
	b.offloadBudget = 3 * b.sendQuantum
}

// modulateCwndForRecovery modulates cwnd during loss recovery
func (b *BBRv3Sender) modulateCwndForRecovery(bytesInFlight protocol.ByteCount) {
	if b.ackState().newlyLostBytes > 0 {
		newCwnd := b.cwnd - b.ackState().newlyLostBytes
		if newCwnd < b.minCwnd {
			newCwnd = b.minCwnd
		}
		b.cwnd = newCwnd
	}

	if b.packetConservation {
		b.cwnd = max(b.cwnd, bytesInFlight+b.ackState().newlyAckedBytes)
	}
}

// boundCwndForProbeRtt bounds cwnd during ProbeRTT
func (b *BBRv3Sender) boundCwndForProbeRtt() {
	if b.state == bbr3StateProbeRtt {
		probeRttCwnd := b.bdpMultiple(b.bw, 0.5)
		if probeRttCwnd < uint64(b.minCwnd) {
			probeRttCwnd = uint64(b.minCwnd)
		}
		if uint64(b.cwnd) > probeRttCwnd {
			b.cwnd = protocol.ByteCount(probeRttCwnd)
		}
	}
}

// boundCwndForModel bounds cwnd based on recent congestion
func (b *BBRv3Sender) boundCwndForModel() {
	var cap uint64 = ^uint64(0)

	if b.isInProbeBwState() && b.state != bbr3StateProbeBwCruise {
		cap = b.inflightHi
	} else if b.state == bbr3StateProbeRtt || b.state == bbr3StateProbeBwCruise {
		cap = b.inflightWithHeadroom()
	}

	if b.inflightLo < cap {
		cap = b.inflightLo
	}
	if cap < uint64(b.minCwnd) {
		cap = uint64(b.minCwnd)
	}
	if uint64(b.cwnd) > cap {
		b.cwnd = protocol.ByteCount(cap)
	}
}

// isInProbeBwState returns true if in any ProbeBW state
func (b *BBRv3Sender) isInProbeBwState() bool {
	return b.state == bbr3StateProbeBwDown ||
		b.state == bbr3StateProbeBwCruise ||
		b.state == bbr3StateProbeBwRefill ||
		b.state == bbr3StateProbeBwUp
}

// isProbingBw returns true if actively probing bandwidth
func (b *BBRv3Sender) isProbingBw() bool {
	return b.state == bbr3StateStartup || b.state == bbr3StateProbeBwRefill || b.state == bbr3StateProbeBwUp
}

// stateName returns the string name of a state
func (b *BBRv3Sender) stateName() string {
	switch b.state {
	case bbr3StateStartup:
		return "Startup"
	case bbr3StateDrain:
		return "Drain"
	case bbr3StateProbeBwDown:
		return "ProbeBW_Down"
	case bbr3StateProbeBwCruise:
		return "ProbeBW_Cruise"
	case bbr3StateProbeBwRefill:
		return "ProbeBW_Refill"
	case bbr3StateProbeBwUp:
		return "ProbeBW_Up"
	case bbr3StateProbeRtt:
		return "ProbeRTT"
	default:
		return "Unknown"
	}
}

// updateGains updates pacing and cwnd gains based on state
func (b *BBRv3Sender) updateGains() {
	oldState := b.state
	switch b.state {
	case bbr3StateStartup:
		b.pacingGain = 2.77
		b.cwndGain = 2.0
	case bbr3StateDrain:
		b.pacingGain = 0.5
		b.cwndGain = 2.0
	case bbr3StateProbeBwDown:
		b.pacingGain = 0.9
		b.cwndGain = 2.0
	case bbr3StateProbeBwCruise:
		b.pacingGain = 1.0
		b.cwndGain = 2.0
	case bbr3StateProbeBwRefill:
		b.pacingGain = 1.0
		b.cwndGain = 2.0
	case bbr3StateProbeBwUp:
		b.pacingGain = 1.25
		b.cwndGain = 2.25
	case bbr3StateProbeRtt:
		b.pacingGain = 1.0
		b.cwndGain = 0.5
	}
	if oldState != b.state {
		log.Printf("[BBRv3] State: %s -> %s, pacingGain=%.2f, cwndGain=%.2f", b.stateName(), b.stateName(), b.pacingGain, b.cwndGain)
	}
}

// updateRound updates round counting
func (b *BBRv3Sender) updateRound() {
	if b.ackState().packetDelivered >= b.round.nextRoundDelivered {
		b.startRound()
		b.round.roundCount++
		b.roundsSinceBwProbe++
		b.round.isRoundStart = true
		b.packetConservation = false
	} else {
		b.round.isRoundStart = false
	}
}

// startRound starts a new round
func (b *BBRv3Sender) startRound() {
	b.round.nextRoundDelivered = b.delivered
}

// updateMaxBw updates the max bandwidth estimate
func (b *BBRv3Sender) updateMaxBw(deliveryRate uint64) {
	b.updateRound()

	if deliveryRate >= b.maxBw {
		b.maxBw = deliveryRate
	}
}

// checkStartupDone checks if startup should exit
func (b *BBRv3Sender) checkStartupDone(deliveryRate uint64) {
	b.checkStartupFullBandwidth(deliveryRate)
	b.checkStartupHighLoss()
	if b.state == bbr3StateStartup && b.fullPipe.isFilledPipe {
		b.enterDrain()
	}
}

// checkStartupFullBandwidth checks if bandwidth has stopped growing
func (b *BBRv3Sender) checkStartupFullBandwidth(deliveryRate uint64) {
	if b.fullPipe.isFilledPipe || !b.round.isRoundStart {
		return
	}

	if deliveryRate >= uint64(float64(b.fullPipe.fullBw)*(1.0+bbr3FullBwGrowthRate)) {
		b.fullPipe.fullBw = deliveryRate
		b.fullPipe.fullBwCount = 0
		return
	}

	b.fullPipe.fullBwCount++
	if b.fullPipe.fullBwCount >= bbr3FullBwCountThreshold {
		b.fullPipe.isFilledPipe = true
	}
}

// checkStartupHighLoss checks if loss rate is too high in startup
func (b *BBRv3Sender) checkStartupHighLoss() {
	if b.lossRoundStart && b.inRecovery && b.lossEventsInRound >= bbr3FullLossCount && b.isInflightTooHigh() {
		b.handleQueueTooHighInStartup()
	}

	if b.lossRoundStart {
		b.lossEventsInRound = 0
	}
}

// isInflightTooHigh checks if inflight is too high based on loss
func (b *BBRv3Sender) isInflightTooHigh() bool {
	return uint64(b.ackState().lost) > uint64(float64(b.ackState().txInFlight)*bbr3LossThreshold)
}

// handleQueueTooHighInStartup handles high queue in startup
func (b *BBRv3Sender) handleQueueTooHighInStartup() {
	b.fullPipe.isFilledPipe = true
	b.inflightHi = max(b.inflight(1.0), b.inflightLatest)
}

// enterDrain enters the drain state
func (b *BBRv3Sender) enterDrain() {
	oldState := b.stateName()
	b.state = bbr3StateDrain
	b.updateGains()
	log.Printf("[BBRv3] State transition: %s -> Drain, cwnd=%d, maxBw=%d", oldState, b.cwnd, b.maxBw)
}

// checkDrain checks if drain is complete
func (b *BBRv3Sender) checkDrain(bytesInFlight protocol.ByteCount) {
	if b.state == bbr3StateDrain && uint64(bytesInFlight) <= b.inflight(1.0) {
		b.enterProbeBw(monotime.Now())
	}
}

// enterProbeBw enters probe bandwidth state
func (b *BBRv3Sender) enterProbeBw(now monotime.Time) {
	b.startProbeBwDown(now)
}

// startProbeBwDown starts ProbeBW_DOWN phase
func (b *BBRv3Sender) startProbeBwDown(now monotime.Time) {
	oldState := b.stateName()
	b.resetCongestionSignals()
	b.bwProbeUpCnt = ^uint64(0)
	b.pickProbeWait()
	b.cycleStamp = now
	b.ackPhase = ackProbeStopping
	b.startRound()
	b.state = bbr3StateProbeBwDown
	log.Printf("[BBRv3] State transition: %s -> ProbeBW_DOWN, pacingGain=0.9", oldState)
}

// startProbeBwCruise starts ProbeBW_CRUISE phase
func (b *BBRv3Sender) startProbeBwCruise() {
	oldState := b.stateName()
	b.state = bbr3StateProbeBwCruise
	log.Printf("[BBRv3] State transition: %s -> ProbeBW_CRUISE, pacingGain=1.0", oldState)
}

// startProbeBwRefill starts ProbeBW_REFILL phase
func (b *BBRv3Sender) startProbeBwRefill() {
	oldState := b.stateName()
	b.resetLowerBounds()
	b.bwProbeUpRounds = 0
	b.bwProbeUpAcks = 0
	b.ackPhase = ackProbeRefilling
	b.startRound()
	b.state = bbr3StateProbeBwRefill
	log.Printf("[BBRv3] State transition: %s -> ProbeBW_REFILL, pacingGain=1.0", oldState)
}

// startProbeBwUp starts ProbeBW_UP phase
func (b *BBRv3Sender) startProbeBwUp(now monotime.Time) {
	oldState := b.stateName()
	b.ackPhase = ackProbeStarting
	b.startRound()
	b.fullPipe.fullBw = b.bw
	b.cycleStamp = now
	b.state = bbr3StateProbeBwUp
	b.raiseInflightHiSlope()
	log.Printf("[BBRv3] State transition: %s -> ProbeBW_UP, pacingGain=1.25, cwndGain=2.25", oldState)
}

// pickProbeWait picks a random wait time for next bandwidth probe
func (b *BBRv3Sender) pickProbeWait() {
	b.roundsSinceBwProbe = uint64(rand.Intn(bbr3ProbeBwRandRounds))
	waitMs := bbr3ProbeBwMinWaitTime.Milliseconds() + rand.Int63n(bbr3ProbeBwMaxWaitTime.Milliseconds()-bbr3ProbeBwMinWaitTime.Milliseconds())
	b.bwProbeWait = time.Duration(waitMs) * time.Millisecond
}

// isRenoCoexistenceProbeTime checks if it's time to probe for Reno fairness
func (b *BBRv3Sender) isRenoCoexistenceProbeTime() bool {
	renoRounds := b.targetInflight()
	rounds := renoRounds
	if rounds > bbr3ProbeBwMaxRounds {
		rounds = bbr3ProbeBwMaxRounds
	}
	return b.roundsSinceBwProbe >= rounds
}

// checkTimeToProbeBw checks if it's time to probe bandwidth
func (b *BBRv3Sender) checkTimeToProbeBw(now monotime.Time) bool {
	if b.hasElapsedInPhase(now, b.bwProbeWait) || b.isRenoCoexistenceProbeTime() {
		b.startProbeBwRefill()
		return true
	}
	return false
}

// checkTimeToCruise checks if it's time to transition from DOWN to CRUISE
func (b *BBRv3Sender) checkTimeToCruise(bytesInFlight protocol.ByteCount) bool {
	if uint64(bytesInFlight) > b.inflightWithHeadroom() {
		return false
	}
	return uint64(bytesInFlight) <= b.inflight(1.0)
}

// hasElapsedInPhase checks if enough time has elapsed in current phase
func (b *BBRv3Sender) hasElapsedInPhase(now monotime.Time, interval time.Duration) bool {
	return now > b.cycleStamp+monotime.Time(interval)
}

// updateProbeBwCyclePhase updates the ProbeBW cycle phase
func (b *BBRv3Sender) updateProbeBwCyclePhase(now monotime.Time, bytesInFlight protocol.ByteCount) {
	if !b.fullPipe.isFilledPipe {
		return
	}

	b.adaptUpperBounds(now)

	if !b.isInProbeBwState() {
		return
	}

	switch b.state {
	case bbr3StateProbeBwDown:
		if b.checkTimeToProbeBw(now) {
			return
		}
		if b.checkTimeToCruise(bytesInFlight) {
			b.startProbeBwCruise()
		}

	case bbr3StateProbeBwCruise:
		b.checkTimeToProbeBw(now)

	case bbr3StateProbeBwRefill:
		if b.round.isRoundStart {
			b.bwProbeSamples = true
			b.startProbeBwUp(now)
		}

	case bbr3StateProbeBwUp:
		if b.hasElapsedInPhase(now, b.minRtt) && uint64(bytesInFlight) > b.inflight(b.pacingGain) {
			b.startProbeBwDown(now)
		}
	}
}

// adaptUpperBounds adapts upper bounds based on ACKs
func (b *BBRv3Sender) adaptUpperBounds(now monotime.Time) {
	if b.ackPhase == ackProbeStarting && b.round.isRoundStart {
		b.ackPhase = ackProbeFeedback
	}

	if b.ackPhase == ackProbeStopping && b.round.isRoundStart {
		b.bwProbeSamples = false
		b.ackPhase = ackProbeInit

		if b.isInProbeBwState() {
			b.advanceMaxBwFilter()
		}
	}

	if !b.checkInflightTooHigh(now) {
		if b.inflightHi == ^uint64(0) {
			return
		}

		if uint64(b.ackState().txInFlight) > b.inflightHi {
			b.inflightHi = uint64(b.ackState().txInFlight)
		}

		if b.bw > b.bwHi {
			b.bwHi = b.bw
		}

		if b.state == bbr3StateProbeBwUp {
			b.probeInflightHiUpward(true)
		}
	}
}

// checkInflightTooHigh checks and handles high inflight
func (b *BBRv3Sender) checkInflightTooHigh(now monotime.Time) bool {
	if b.isInflightTooHigh() {
		if b.bwProbeSamples {
			b.handleInflightTooHigh(now)
		}
		return true
	}
	return false
}

// handleInflightTooHigh handles high inflight by reducing bounds
func (b *BBRv3Sender) handleInflightTooHigh(now monotime.Time) {
	b.bwProbeSamples = false

	if !b.isAppLimited() {
		newInflightHi := uint64(float64(b.targetInflight()) * bbr3Beta)
		if newInflightHi < uint64(b.ackState().txInFlight) {
			newInflightHi = uint64(b.ackState().txInFlight)
		}
		b.inflightHi = newInflightHi
	}

	if b.state == bbr3StateProbeBwUp {
		b.startProbeBwDown(now)
	}
}

// isAppLimited returns true if application is limiting throughput
func (b *BBRv3Sender) isAppLimited() bool {
	// Simplified - in full implementation would track app-limited state
	return false
}

// raiseInflightHiSlope raises the slope for inflight_hi growth
func (b *BBRv3Sender) raiseInflightHiSlope() {
	growthThisRound := uint64(1) << b.bwProbeUpRounds
	if b.bwProbeUpRounds < 30 {
		b.bwProbeUpRounds++
	}
	b.bwProbeUpCnt = uint64(b.cwnd) / growthThisRound
	if b.bwProbeUpCnt < 1 {
		b.bwProbeUpCnt = 1
	}
}

// probeInflightHiUpward increases inflight_hi when probing upward
func (b *BBRv3Sender) probeInflightHiUpward(isCwndLimited bool) {
	if !isCwndLimited || uint64(b.cwnd) < b.inflightHi {
		return
	}

	b.bwProbeUpAcks += uint64(b.ackState().newlyAckedBytes)
	if b.bwProbeUpAcks >= b.bwProbeUpCnt {
		delta := b.bwProbeUpAcks / b.bwProbeUpCnt
		b.bwProbeUpAcks -= delta * b.bwProbeUpCnt
		b.inflightHi += delta * uint64(b.maxDatagramSize)
	}

	if b.round.isRoundStart {
		b.raiseInflightHiSlope()
	}
}

// advanceMaxBwFilter advances the max bandwidth filter window
func (b *BBRv3Sender) advanceMaxBwFilter() {
	b.cycleCount++
	// Simplified - in full implementation would track two-cycle window
}

// updateMinRtt updates the min RTT estimate
func (b *BBRv3Sender) updateMinRtt(now monotime.Time, sampleRtt time.Duration) {
	b.probeRttExpired = time.Duration(now-b.probeRttMinStamp) > bbr3ProbeRttInterval

	if sampleRtt > 0 && (sampleRtt <= b.probeRttMinDelay || b.probeRttExpired) {
		b.probeRttMinDelay = sampleRtt
		b.probeRttMinStamp = now
	}

	minRttExpired := time.Duration(now-b.minRttStamp) > bbr3MinRttFilterLen
	if b.probeRttMinDelay < b.minRtt || minRttExpired {
		b.minRtt = b.probeRttMinDelay
		b.minRttStamp = b.probeRttMinStamp
	}
}

// checkProbeRtt checks if we should enter ProbeRTT
func (b *BBRv3Sender) checkProbeRtt(now monotime.Time, bytesInFlight protocol.ByteCount) {
	if b.state != bbr3StateProbeRtt && b.probeRttExpired && !b.idleRestart {
		b.enterProbeRtt()
		b.saveCwnd()
		b.probeRttDoneStamp = 0
		b.ackPhase = ackProbeStopping
		b.startRound()
	}

	if b.state == bbr3StateProbeRtt {
		b.handleProbeRtt(now, bytesInFlight)
	}

	if b.delivered > 0 {
		b.idleRestart = false
	}
}

// enterProbeRtt enters ProbeRTT state
func (b *BBRv3Sender) enterProbeRtt() {
	oldState := b.stateName()
	b.state = bbr3StateProbeRtt
	b.updateGains()
	log.Printf("[BBRv3] State transition: %s -> ProbeRTT, pacingGain=1.0, cwndGain=0.5", oldState)
}

// handleProbeRtt handles ProbeRTT state
func (b *BBRv3Sender) handleProbeRtt(now monotime.Time, bytesInFlight protocol.ByteCount) {
	if b.probeRttDoneStamp > 0 {
		if b.round.isRoundStart {
			b.probeRttRoundDone = true
		}
		if b.probeRttRoundDone {
			b.checkProbeRttDone(now)
		}
	} else if uint64(bytesInFlight) <= b.probeRttCwnd() {
		b.probeRttDoneStamp = now + monotime.Time(bbr3ProbeRttDuration)
		b.probeRttRoundDone = false
		b.startRound()
	}
}

// checkProbeRttDone checks if ProbeRTT is done
func (b *BBRv3Sender) checkProbeRttDone(now monotime.Time) {
	if b.probeRttDoneStamp > 0 && now > b.probeRttDoneStamp {
		b.probeRttMinStamp = now
		b.restoreCwnd()
		b.exitProbeRtt(now)
	}
}

// exitProbeRtt exits ProbeRTT state
func (b *BBRv3Sender) exitProbeRtt(now monotime.Time) {
	b.resetLowerBounds()
	if b.fullPipe.isFilledPipe {
		b.startProbeBwDown(now)
		b.startProbeBwCruise()
	} else {
		b.state = bbr3StateStartup
		b.updateGains()
	}
}

// probeRttCwnd returns the cwnd for ProbeRTT
func (b *BBRv3Sender) probeRttCwnd() uint64 {
	cwnd := b.bdpMultiple(b.bw, 0.5)
	if cwnd < uint64(b.minCwnd) {
		cwnd = uint64(b.minCwnd)
	}
	return cwnd
}

// resetCongestionSignals resets congestion signals
func (b *BBRv3Sender) resetCongestionSignals() {
	b.lossInRound = false
	b.bwLatest = 0
	b.inflightLatest = 0
}

// resetLowerBounds resets lower bounds
func (b *BBRv3Sender) resetLowerBounds() {
	b.bwLo = ^uint64(0)
	b.inflightLo = ^uint64(0)
}

// saveCwnd saves the current cwnd
func (b *BBRv3Sender) saveCwnd() {
	if !b.inRecovery && b.state != bbr3StateProbeRtt {
		b.priorCwnd = b.cwnd
	} else {
		if b.cwnd > b.priorCwnd {
			b.priorCwnd = b.cwnd
		}
	}
}

// restoreCwnd restores the saved cwnd
func (b *BBRv3Sender) restoreCwnd() {
	if b.priorCwnd > b.cwnd {
		b.cwnd = b.priorCwnd
	}
}

// updateLatestDeliverySignals updates latest delivery signals
func (b *BBRv3Sender) updateLatestDeliverySignals(deliveryRate uint64, delivered uint64) {
	b.lossRoundStart = false
	b.bwLatest = max(b.bwLatest, deliveryRate)
	b.inflightLatest = max(b.inflightLatest, delivered)

	if delivered >= b.lossRoundDelivered {
		b.lossRoundDelivered = b.delivered
		b.lossRoundStart = true
	}
}

// advanceLatestDeliverySignals advances latest delivery signals
func (b *BBRv3Sender) advanceLatestDeliverySignals(deliveryRate uint64, delivered uint64) {
	if b.lossRoundStart {
		b.bwLatest = deliveryRate
		b.inflightLatest = delivered
	}
}

// updateCongestionSignals updates congestion signals
func (b *BBRv3Sender) updateCongestionSignals(deliveryRate uint64) {
	b.updateMaxBw(deliveryRate)

	if b.ackState().lost > 0 {
		b.lossInRound = true
	}

	if !b.lossRoundStart {
		return
	}

	b.adaptLowerBoundsFromCongestion()
	b.lossInRound = false
}

// adaptLowerBoundsFromCongestion adapts lower bounds based on congestion
func (b *BBRv3Sender) adaptLowerBoundsFromCongestion() {
	if b.isProbingBw() {
		return
	}

	if b.lossInRound {
		b.initLowerBounds()
		b.lossLowerBounds()
	}
}

// initLowerBounds initializes lower bounds
func (b *BBRv3Sender) initLowerBounds() {
	if b.bwLo == ^uint64(0) {
		b.bwLo = b.maxBw
	}
	if b.inflightLo == ^uint64(0) {
		b.inflightLo = uint64(b.cwnd)
	}
}

// lossLowerBounds updates lower bounds based on loss
func (b *BBRv3Sender) lossLowerBounds() {
	bwLo := uint64(float64(b.bwLo) * bbr3Beta)
	if b.bwLatest > bwLo {
		bwLo = b.bwLatest
	}
	b.bwLo = bwLo

	inflightLo := uint64(float64(b.inflightLo) * bbr3Beta)
	if b.inflightLatest > inflightLo {
		inflightLo = b.inflightLatest
	}
	b.inflightLo = inflightLo
}

// boundBwForModel bounds bw for the model
func (b *BBRv3Sender) boundBwForModel() {
	b.bw = b.maxBw
	if b.bwLo < b.bw {
		b.bw = b.bwLo
	}
	if b.bwHi < b.bw {
		b.bw = b.bwHi
	}
}

// updateAckAggregation updates ACK aggregation estimate
func (b *BBRv3Sender) updateAckAggregation(now monotime.Time) {
	interval := time.Duration(now - b.extraAckedIntervalStart)
	expectedDelivered := uint64(float64(b.bw) * interval.Seconds())

	if b.extraAckedDelivered <= expectedDelivered {
		b.extraAckedDelivered = 0
		b.extraAckedIntervalStart = now
		expectedDelivered = 0
	}

	b.extraAckedDelivered += uint64(b.ackState().newlyAckedBytes)
	extra := b.extraAckedDelivered - expectedDelivered
	if extra > uint64(b.cwnd) {
		extra = uint64(b.cwnd)
	}

	b.extraAckedFilter.update(b.round.roundCount, extra)
}

// enterRecovery enters loss recovery
func (b *BBRv3Sender) enterRecovery(now monotime.Time) {
	b.saveCwnd()
	b.recoveryEpochStart = now
	b.cwnd = b.cwnd + b.ackState().newlyAckedBytes
	b.packetConservation = true
	b.inRecovery = true
	b.startRound()
	log.Printf("[BBRv3] Entering Recovery: state=%s, cwnd=%d, priorCwnd=%d", b.stateName(), b.cwnd, b.priorCwnd)
}

// exitRecovery exits loss recovery
func (b *BBRv3Sender) exitRecovery() {
	b.recoveryEpochStart = 0
	b.packetConservation = false
	b.inRecovery = false
	b.restoreCwnd()
	log.Printf("[BBRv3] Exiting Recovery: state=%s, cwnd=%d", b.stateName(), b.cwnd)
}

// handleRestartFromIdle handles restart from idle
func (b *BBRv3Sender) handleRestartFromIdle(now monotime.Time, bytesInFlight protocol.ByteCount) {
	if bytesInFlight == 0 {
		b.idleRestart = true
		b.extraAckedIntervalStart = now

		if b.isInProbeBwState() {
			b.setPacingRateWithGain(1.0)
		} else if b.state == bbr3StateProbeRtt {
			b.checkProbeRttDone(now)
		}
	}
}

// ackState returns the current ack state (simplified)
func (b *BBRv3Sender) ackState() *bbr3AckState {
	// In a full implementation, this would be per-ACK state
	// For now, return a zero state
	return &bbr3AckState{}
}

// updateModelAndState updates model and state on ACK
func (b *BBRv3Sender) updateModelAndState(now monotime.Time, bytesInFlight protocol.ByteCount, deliveryRate uint64) {
	b.updateLatestDeliverySignals(deliveryRate, b.delivered)
	b.updateCongestionSignals(deliveryRate)
	b.updateAckAggregation(now)
	b.checkStartupDone(deliveryRate)
	b.checkDrain(bytesInFlight)
	b.updateProbeBwCyclePhase(now, bytesInFlight)
	b.updateMinRtt(now, time.Duration(0)) // RTT would come from sample
	b.checkProbeRtt(now, bytesInFlight)
	b.advanceLatestDeliverySignals(deliveryRate, b.delivered)
	b.boundBwForModel()
}

// SendAlgorithm interface implementation

// TimeUntilSend returns the next allowed send time
func (b *BBRv3Sender) TimeUntilSend(bytesInFlight protocol.ByteCount) monotime.Time {
	// Calculate pacing delay based on packet size and pacing rate
	if b.pacingRate == 0 {
		return monotime.Now()
	}

	nextTime := b.initTime
	return nextTime
}

// HasPacingBudget checks if there's budget to send now
func (b *BBRv3Sender) HasPacingBudget(now monotime.Time) bool {
	if b.state == bbr3StateProbeRtt {
		return true
	}
	return true // Simplified
}

// OnPacketSent is called when a packet is sent
func (b *BBRv3Sender) OnPacketSent(sentTime monotime.Time, bytesInFlight protocol.ByteCount, packetNumber protocol.PacketNumber, bytes protocol.ByteCount, isRetransmittable bool) {
	b.sentTimes[packetNumber] = sentTime

	// Update statistics
	if b.statsEnabled && b.stats != nil {
		b.stats.OnPacketSent(bytes)
		b.stats.UpdateBytesInFlight(bytesInFlight)
		b.stats.UpdatePacingRate(b.pacingRate)
		b.stats.UpdateMaxBandwidth(b.maxBw)
		b.stats.SetSlowStart(b.InSlowStart())
		b.stats.SetRecovery(b.InRecovery())
	}

	// Calculate next send time based on pacing rate
	if b.pacingRate > 0 && bytes > 0 {
		pacingDelay := time.Duration(float64(bytes) * float64(time.Second) / float64(b.pacingRate))
		b.initTime = sentTime + monotime.Time(pacingDelay)
	}

	// Log periodically (every 100 packets to avoid spam)
	if uint64(packetNumber)%100 == 0 {
		log.Printf("[BBRv3] OnPacketSent: packet=%d, bytes=%d, bytesInFlight=%d, state=%s, cwnd=%d, pacingRate=%d",
			packetNumber, bytes, bytesInFlight, b.stateName(), b.cwnd, b.pacingRate)
	}
}

// CanSend checks if we can send with current cwnd
func (b *BBRv3Sender) CanSend(bytesInFlight protocol.ByteCount) bool {
	return bytesInFlight < b.cwnd
}

// MaybeExitSlowStart is a no-op for BBR
func (b *BBRv3Sender) MaybeExitSlowStart() {}

// OnPacketAcked is called when a packet is ACKed
func (b *BBRv3Sender) OnPacketAcked(number protocol.PacketNumber, ackedBytes protocol.ByteCount, priorInFlight protocol.ByteCount, eventTime monotime.Time) {
	sentTime, ok := b.sentTimes[number]
	var rtt time.Duration
	if ok {
		rtt = time.Duration(eventTime - sentTime)
		if rtt > 0 {
			b.updateMinRtt(eventTime, rtt)
		}
		delete(b.sentTimes, number)
	}

	b.delivered += uint64(ackedBytes)

	// Log periodically (every 100 acks to avoid spam)
	if number%100 == 0 {
		log.Printf("[BBRv3] OnPacketAcked: packet=%d, ackedBytes=%d, priorInFlight=%d, state=%s, cwnd=%d, minRTT=%v, maxBw=%d",
			number, ackedBytes, priorInFlight, b.stateName(), b.cwnd, b.minRtt, b.maxBw)
	}

	// Update statistics
	if b.statsEnabled && b.stats != nil {
		b.stats.OnPacketAcked(ackedBytes)
		b.stats.UpdateBytesInFlight(priorInFlight)
		b.stats.UpdateCwnd(b.cwnd)
		b.stats.UpdatePacingRate(b.pacingRate)
		b.stats.UpdateMaxBandwidth(b.maxBw)
		b.stats.UpdateCurrentBandwidth(b.bw)
		b.stats.UpdateState(b.stateName())
		if rtt > 0 {
			b.stats.UpdateRtt(rtt)
		}
		b.stats.SetSlowStart(b.InSlowStart())
		b.stats.SetRecovery(b.InRecovery())
	}

	// Update ack state (simplified)
	// In full implementation, would track ackState properly
}

// OnCongestionEvent is called on loss detection
func (b *BBRv3Sender) OnCongestionEvent(number protocol.PacketNumber, lostBytes protocol.ByteCount, priorInFlight protocol.ByteCount) {
	if lostBytes > 0 && b.lossEventsInRound < 0xf {
		b.lossEventsInRound++
	}

	// Update statistics for lost bytes
	if b.statsEnabled && b.stats != nil {
		b.stats.OnBytesLost(lostBytes)
		b.stats.UpdateBytesInFlight(priorInFlight)
		b.stats.SetRecovery(b.InRecovery())
	}

	// Handle high loss in startup
	if b.state == bbr3StateStartup && b.inRecovery {
		b.checkStartupHighLoss()
	}
}

// OnRetransmissionTimeout handles RTO
func (b *BBRv3Sender) OnRetransmissionTimeout(packetsRetransmitted bool) {
	if packetsRetransmitted {
		// Update statistics
		if b.statsEnabled && b.stats != nil {
			b.stats.OnRetransmission()
		}

		// Reset but preserve key state
		oldMinRtt := b.minRtt
		oldMinRttStamp := b.minRttStamp
		*b = *NewBBRv3Sender(b.maxDatagramSize)
		b.minRtt = oldMinRtt
		b.minRttStamp = oldMinRttStamp
	}
}

// SetMaxDatagramSize updates the max datagram size
func (b *BBRv3Sender) SetMaxDatagramSize(maxDatagramSize protocol.ByteCount) {
	b.maxDatagramSize = maxDatagramSize
}

// GetCongestionWindow returns the current cwnd
func (b *BBRv3Sender) GetCongestionWindow() protocol.ByteCount {
	if b.cwnd < b.minCwnd {
		return b.minCwnd
	}
	return b.cwnd
}

// InRecovery returns true if in recovery
func (b *BBRv3Sender) InRecovery() bool {
	return b.inRecovery
}

// InSlowStart returns true if in startup
func (b *BBRv3Sender) InSlowStart() bool {
	return b.state == bbr3StateStartup
}
