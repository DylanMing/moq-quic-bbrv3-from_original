package congestion

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/quic-go/quic-go/internal/monotime"
	"github.com/quic-go/quic-go/internal/protocol"
)

// BBRv3StatsConfig configures the statistics collection behavior
type BBRv3StatsConfig struct {
	// Enabled enables or disables statistics collection
	Enabled bool
	// LogInterval is the interval between log prints (default: 1 second)
	LogInterval time.Duration
	// ConnectionID is an identifier for this connection (for logging)
	ConnectionID string
	// LogToFile enables logging to a file (in addition to console)
	LogToFile bool
	// LogFilePath is the path to the log file (if LogToFile is true)
	LogFilePath string
}

// DefaultBBRv3StatsConfig returns a default statistics configuration
func DefaultBBRv3StatsConfig() *BBRv3StatsConfig {
	return &BBRv3StatsConfig{
		Enabled:      true,
		LogInterval:  1 * time.Second,
		ConnectionID: "",
		LogToFile:    false,
		LogFilePath:  "",
	}
}

// BBRv3Stats holds runtime statistics for BBRv3
type BBRv3Stats struct {
	// Configuration
	config *BBRv3StatsConfig

	// Congestion metrics
	cwnd              protocol.ByteCount
	minCwnd           protocol.ByteCount
	maxCwnd           protocol.ByteCount
	ssthresh          protocol.ByteCount

	// Byte counters
	totalBytesSent    uint64
	totalBytesAcked   uint64
	totalBytesLost    uint64
	bytesInFlight     protocol.ByteCount

	// Retransmission counter
	retransmissions   uint64

	// RTT metrics
	minRtt            time.Duration
	avgRtt            time.Duration
	currentRtt        time.Duration
	rttSampleCount    uint64
	rttSum            time.Duration

	// Transmission metrics
	pacingRate        uint64
	maxBandwidth      uint64
	currentBandwidth  uint64

	// State information
	state             string
	inSlowStart       bool
	inRecovery        bool

	// Timing
	startTime         monotime.Time
	lastLogTime       monotime.Time

	// Thread safety
	mutex             sync.RWMutex

	// Stop channel for the logger goroutine
	stopLogger        chan struct{}
	loggerRunning     bool
}

// NewBBRv3Stats creates a new statistics collector
func NewBBRv3Stats(config *BBRv3StatsConfig) *BBRv3Stats {
	if config == nil {
		config = DefaultBBRv3StatsConfig()
	}

	now := monotime.Now()
	return &BBRv3Stats{
		config:        config,
		startTime:     now,
		lastLogTime:   now,
		stopLogger:    make(chan struct{}),
		minRtt:        time.Duration(^uint64(0) >> 1), // Max duration
		cwnd:          0,
		maxCwnd:       0,
		ssthresh:      protocol.ByteCount(^uint64(0) >> 1), // Max uint64 as undefined
	}
}

// Start begins the periodic statistics logging
func (s *BBRv3Stats) Start() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if !s.config.Enabled || s.loggerRunning {
		return
	}

	s.loggerRunning = true
	go s.statsLogger()
	log.Printf("[BBRv3-Stats] Statistics logging started for connection %s (interval: %v)",
		s.config.ConnectionID, s.config.LogInterval)
}

// Stop stops the periodic statistics logging
func (s *BBRv3Stats) Stop() {
	s.mutex.Lock()
	if !s.loggerRunning {
		s.mutex.Unlock()
		return
	}
	s.mutex.Unlock()

	close(s.stopLogger)
	s.mutex.Lock()
	s.loggerRunning = false
	s.mutex.Unlock()

	log.Printf("[BBRv3-Stats] Statistics logging stopped for connection %s", s.config.ConnectionID)
}

// statsLogger periodically logs statistics
func (s *BBRv3Stats) statsLogger() {
	ticker := time.NewTicker(s.config.LogInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.LogStats()
		case <-s.stopLogger:
			return
		}
	}
}

// LogStats prints the current statistics
func (s *BBRv3Stats) LogStats() {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	if !s.config.Enabled {
		return
	}

	elapsed := monotime.Since(s.startTime).Seconds()
	txRate := float64(s.totalBytesSent*8) / elapsed / 1000000 // Mbps

	// Format RTT values
	minRttStr := formatDuration(s.minRtt)
	avgRttStr := formatDuration(s.avgRtt)
	currRttStr := formatDuration(s.currentRtt)

	// Build log message
	connID := s.config.ConnectionID
	if connID == "" {
		connID = "default"
	}

	log.Printf("[BBRv3-Stats] conn=%s | CWND=%d | SSTHRESH=%s | Bytes: sent=%d acked=%d lost=%d inflight=%d | RTT: min=%s avg=%s curr=%s | Rate: pacing=%d bw=%d Mbps | Retrans=%d | State=%s | SlowStart=%t | Recovery=%t",
		connID,
		s.cwnd,
		formatSsthresh(s.ssthresh),
		s.totalBytesSent,
		s.totalBytesAcked,
		s.totalBytesLost,
		s.bytesInFlight,
		minRttStr,
		avgRttStr,
		currRttStr,
		s.pacingRate,
		uint64(txRate),
		s.retransmissions,
		s.state,
		s.inSlowStart,
		s.inRecovery,
	)
}

// Update methods - these are called from BBRv3Sender

// UpdateCwnd updates the congestion window
func (s *BBRv3Stats) UpdateCwnd(cwnd protocol.ByteCount) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.cwnd = cwnd
	if cwnd > s.maxCwnd {
		s.maxCwnd = cwnd
	}
}

// UpdateMinCwnd updates the minimum congestion window
func (s *BBRv3Stats) UpdateMinCwnd(minCwnd protocol.ByteCount) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.minCwnd = minCwnd
}

// UpdateSsthresh updates the slow start threshold
func (s *BBRv3Stats) UpdateSsthresh(ssthresh protocol.ByteCount) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.ssthresh = ssthresh
}

// OnPacketSent records a packet being sent
func (s *BBRv3Stats) OnPacketSent(bytes protocol.ByteCount) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.totalBytesSent += uint64(bytes)
}

// OnPacketAcked records a packet being acknowledged
func (s *BBRv3Stats) OnPacketAcked(bytes protocol.ByteCount) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.totalBytesAcked += uint64(bytes)
}

// OnBytesLost records lost bytes
func (s *BBRv3Stats) OnBytesLost(bytes protocol.ByteCount) {
	s.mutex.Unlock()
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.totalBytesLost += uint64(bytes)
}

// OnRetransmission records a retransmission
func (s *BBRv3Stats) OnRetransmission() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.retransmissions++
}

// UpdateBytesInFlight updates the bytes in flight
func (s *BBRv3Stats) UpdateBytesInFlight(bytesInFlight protocol.ByteCount) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.bytesInFlight = bytesInFlight
}

// UpdateRtt updates RTT measurements
func (s *BBRv3Stats) UpdateRtt(rtt time.Duration) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.currentRtt = rtt

	// Update min RTT
	if rtt < s.minRtt {
		s.minRtt = rtt
	}

	// Update average RTT
	s.rttSampleCount++
	s.rttSum += rtt
	s.avgRtt = s.rttSum / time.Duration(s.rttSampleCount)
}

// UpdatePacingRate updates the pacing rate
func (s *BBRv3Stats) UpdatePacingRate(rate uint64) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.pacingRate = rate
}

// UpdateMaxBandwidth updates the max bandwidth estimate
func (s *BBRv3Stats) UpdateMaxBandwidth(bw uint64) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.maxBandwidth = bw
}

// UpdateCurrentBandwidth updates the current bandwidth
func (s *BBRv3Stats) UpdateCurrentBandwidth(bw uint64) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.currentBandwidth = bw
}

// UpdateState updates the BBRv3 state
func (s *BBRv3Stats) UpdateState(state string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.state = state
}

// SetSlowStart sets whether we're in slow start
func (s *BBRv3Stats) SetSlowStart(inSlowStart bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.inSlowStart = inSlowStart
}

// SetRecovery sets whether we're in recovery
func (s *BBRv3Stats) SetRecovery(inRecovery bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.inRecovery = inRecovery
}

// GetStats returns a copy of the current statistics
func (s *BBRv3Stats) GetStats() BBRv3StatsSnapshot {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	return BBRv3StatsSnapshot{
		Cwnd:             s.cwnd,
		MinCwnd:          s.minCwnd,
		MaxCwnd:          s.maxCwnd,
		Ssthresh:         s.ssthresh,
		TotalBytesSent:   s.totalBytesSent,
		TotalBytesAcked:  s.totalBytesAcked,
		TotalBytesLost:   s.totalBytesLost,
		BytesInFlight:    s.bytesInFlight,
		Retransmissions:  s.retransmissions,
		MinRtt:           s.minRtt,
		AvgRtt:           s.avgRtt,
		CurrentRtt:       s.currentRtt,
		PacingRate:       s.pacingRate,
		MaxBandwidth:     s.maxBandwidth,
		CurrentBandwidth: s.currentBandwidth,
		State:            s.state,
		InSlowStart:      s.inSlowStart,
		InRecovery:       s.inRecovery,
	}
}

// BBRv3StatsSnapshot is a snapshot of BBRv3 statistics
type BBRv3StatsSnapshot struct {
	Cwnd             protocol.ByteCount
	MinCwnd          protocol.ByteCount
	MaxCwnd          protocol.ByteCount
	Ssthresh         protocol.ByteCount
	TotalBytesSent   uint64
	TotalBytesAcked  uint64
	TotalBytesLost   uint64
	BytesInFlight    protocol.ByteCount
	Retransmissions  uint64
	MinRtt           time.Duration
	AvgRtt           time.Duration
	CurrentRtt       time.Duration
	PacingRate       uint64
	MaxBandwidth     uint64
	CurrentBandwidth uint64
	State            string
	InSlowStart      bool
	InRecovery       bool
}

// Helper functions

func formatDuration(d time.Duration) string {
	if d == time.Duration(^uint64(0)>>1) {
		return "N/A"
	}
	return d.String()
}

func formatSsthresh(s protocol.ByteCount) string {
	if s == protocol.ByteCount(^uint64(0)>>1) {
		return "inf"
	}
	return fmt.Sprintf("%d", s)
}
