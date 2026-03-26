package congestion

import (
	"testing"

	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/stretchr/testify/assert"
)

func TestBBRv3SenderCreation(t *testing.T) {
	sender := NewBBRv3Sender(1200)
	assert.NotNil(t, sender)
	assert.True(t, sender.InSlowStart())
	assert.Equal(t, bbr3StateStartup, sender.state)
	assert.Greater(t, sender.GetCongestionWindow(), protocol.ByteCount(0))
}

func TestBBRv3GetCongestionWindow(t *testing.T) {
	sender := NewBBRv3Sender(1200)
	cwnd := sender.GetCongestionWindow()
	assert.Greater(t, cwnd, protocol.ByteCount(0))
	assert.GreaterOrEqual(t, cwnd, sender.minCwnd)
}

func TestBBRv3CanSend(t *testing.T) {
	sender := NewBBRv3Sender(1200)

	// Should be able to send when bytes in flight is less than cwnd
	assert.True(t, sender.CanSend(0))

	// Should not be able to send when at cwnd limit
	cwnd := sender.GetCongestionWindow()
	assert.False(t, sender.CanSend(cwnd))
}

func TestBBRv3InSlowStart(t *testing.T) {
	sender := NewBBRv3Sender(1200)
	assert.True(t, sender.InSlowStart())

	// After state change, should not be in slow start
	sender.state = bbr3StateDrain
	assert.False(t, sender.InSlowStart())
}

func TestBBRv3SetMaxDatagramSize(t *testing.T) {
	sender := NewBBRv3Sender(1200)
	sender.SetMaxDatagramSize(1500)
	assert.Equal(t, protocol.ByteCount(1500), sender.maxDatagramSize)
}

func TestBBRv3OnPacketSent(t *testing.T) {
	sender := NewBBRv3Sender(1200)
	sender.OnPacketSent(0, 0, 1, 1200, true)

	// Check that sent time was recorded
	_, exists := sender.sentTimes[1]
	assert.True(t, exists)
}

func TestBBRv3OnPacketAcked(t *testing.T) {
	sender := NewBBRv3Sender(1200)
	sender.OnPacketSent(0, 0, 1, 1200, true)
	sender.OnPacketAcked(1, 1200, 0, 1000000)

	// Check that sent time was removed
	_, exists := sender.sentTimes[1]
	assert.False(t, exists)

	// Check delivered was updated
	assert.Equal(t, uint64(1200), sender.delivered)
}

func TestBBRv3UpdateMinRtt(t *testing.T) {
	sender := NewBBRv3Sender(1200)
	sender.updateMinRtt(1000000000, 50000000) // 50ms RTT
	assert.Equal(t, 50000000, int(sender.minRtt))
}

func TestBBRv3BdpCalculation(t *testing.T) {
	sender := NewBBRv3Sender(1200)
	sender.minRtt = 100000000 // 100ms
	bdp := sender.bdp(1000000) // 1MB/s = 8Mbps

	// BDP = bandwidth * RTT = 1MB/s * 0.1s = 100KB
	expectedBdp := uint64(100000)
	assert.Equal(t, expectedBdp, bdp)
}

func TestBBRv3UpdateGains(t *testing.T) {
	sender := NewBBRv3Sender(1200)

	testCases := []struct {
		state        bbr3State
		wantPacing   float64
		wantCwndGain float64
	}{
		{bbr3StateStartup, 2.77, 2.0},
		{bbr3StateDrain, 0.5, 2.0},
		{bbr3StateProbeBwDown, 0.9, 2.0},
		{bbr3StateProbeBwCruise, 1.0, 2.0},
		{bbr3StateProbeBwRefill, 1.0, 2.0},
		{bbr3StateProbeBwUp, 1.25, 2.25},
		{bbr3StateProbeRtt, 1.0, 0.5},
	}

	for _, tc := range testCases {
		sender.state = tc.state
		sender.updateGains()
		assert.Equal(t, tc.wantPacing, sender.pacingGain, "state %d pacing gain", tc.state)
		assert.Equal(t, tc.wantCwndGain, sender.cwndGain, "state %d cwnd gain", tc.state)
	}
}
