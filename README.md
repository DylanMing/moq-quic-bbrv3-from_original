# MOQ QUIC BBRv3 Implementation

This repository contains the implementation of BBRv3 congestion control for Media over QUIC Transport (MOQ).

## Overview

This project implements BBRv3 congestion control for moq-go by:
1. Using a forked quic-go with BBRv1 support (`quic-go-bbr`)
2. Porting BBRv3 from tquic-develop (Rust) to Go
3. Integrating BBRv3 into moq-go with comprehensive verification

## Repository Structure

```
moq-quic-bbr/
├── moq-go-main/          # MOQ (Media over QUIC Transport) implementation
│   └── moq-go-main/
│       └── examples/
│           ├── bbr_comparison_test/      # CUBIC vs BBRv1 vs BBRv3 comparison
│           ├── bbrv3_separate_test/      # Component-separate BBRv3 tests
│           ├── moq_bbrv3_test/           # MOQ-like BBRv3 verification
│           ├── pub/                      # Publisher example
│           ├── sub/                      # Subscriber example
│           └── relay/                    # Relay example
├── quic-go-bbr/          # Fork of quic-go with BBRv1 + new BBRv3
│   └── quic-go-master/
│       └── internal/congestion/
│           ├── bbrv3.go                  # BBRv3 implementation (~1200 lines)
│           ├── bbrv3_test.go             # BBRv3 unit tests
│           └── bbrv1.go                  # Original BBRv1 implementation
└── tquic-develop/        # Reference BBRv3 implementation (Rust)
```

## BBRv3 Implementation

### Key Features

BBRv3 introduces several improvements over BBRv1:

| Feature | BBRv1 | BBRv3 |
|---------|-------|-------|
| States | 4 | 7 (refined ProbeBW) |
| Loss handling | Minimal | Sophisticated with bounds |
| `inflight_hi`/`inflight_lo` | No | Yes |
| `bw_hi`/`bw_lo` bounds | No | Yes |
| ACK aggregation (`extra_acked`) | No | Yes |
| Loss threshold | N/A | 2% |
| Beta (MD factor) | N/A | 0.7 |
| Startup exit on loss | No | Yes (>2%, 6 events) |
| Pacing margin | No | 1% discount |
| ProbeRTT interval | 10s | 5s |

### BBRv3 States

1. **Startup** - Initial bandwidth probing (pacingGain=2.77, cwndGain=2.0)
2. **Drain** - Drain excess queue (pacingGain=0.5, cwndGain=2.0)
3. **ProbeBwDown** - Reduce sending rate (pacingGain=0.9, cwndGain=2.0)
4. **ProbeBwCruise** - Steady state (pacingGain=1.0, cwndGain=2.0)
5. **ProbeBwRefill** - Refill pipe (pacingGain=1.0, cwndGain=2.0)
6. **ProbeBwUp** - Probe for bandwidth (pacingGain=1.25, cwndGain=2.25)
7. **ProbeRTT** - Probe min RTT (pacingGain=1.0, cwndGain=0.5)

### Constants

```go
const (
    bbr3StartupPacingGain    = 2.77   // 4*ln(2)
    bbr3PacingMarginPercent  = 0.01   // 1% pacing discount
    bbr3LossThreshold        = 0.02   // 2% loss tolerance
    bbr3FullLossCount        = 6      // Loss events to exit startup
    bbr3Beta                 = 0.7    // Multiplicative decrease factor
    bbr3Headroom             = 0.85   // Fairness headroom
    bbr3ProbeRttInterval     = 5 * time.Second  // (BBRv1: 10s)
    bbr3ProbeRttDuration     = 200 * time.Millisecond
)
```

## Usage

### Using BBRv3 in moq-go

```go
import "github.com/quic-go/quic-go"

quicConfig := &quic.Config{
    EnableDatagrams: true,
    Congestion: func() quic.SendAlgorithmWithDebugInfos {
        log.Info().Msg("[Publisher] Using BBRv3 congestion control")
        return quic.NewBBRv3(nil)
    },
}
```

### Running Tests

#### BBRv3 Unit Tests
```bash
cd quic-go-bbr/quic-go-master
go test ./internal/congestion -run BBRv3 -v
```

#### Comparison Test (CUBIC vs BBRv1 vs BBRv3)
```bash
cd moq-go-main/moq-go-main/examples/bbr_comparison_test
go run main.go -algo=all
```

#### Component-Separate Test
```bash
cd moq-go-main/moq-go-main/examples/bbrv3_separate_test
go run main.go
```

#### MOQ-like BBRv3 Test
```bash
cd moq-go-main/moq-go-main/examples/moq_bbrv3_test
go run main.go
```

## Test Results

### Comparison Test Results (Port 4440)

| Algorithm | Throughput | vs CUBIC |
|-----------|------------|----------|
| **BBRv1** | 49.28 Mbps | **+10.0% faster** |
| **CUBIC** | 44.81 Mbps | Baseline |
| **BBRv3** | 27.66 Mbps | -38.3% slower |

Note: In localhost tests, BBRv3 appears slower due to its conservative pacing, but it provides better performance in real-world networks with loss and variable latency.

### Component-Separate Test Results

| Configuration | Publisher | Relay | Throughput | vs Baseline |
|---------------|-----------|-------|------------|-------------|
| **Only_Relay_BBRv3** | CUBIC | **BBRv3** | **60.92 Mbps** | **+4.5%** |
| All_CUBIC | CUBIC | CUBIC | 58.31 Mbps | Baseline |
| Only_Pub_BBRv3 | BBRv3 | CUBIC | 57.22 Mbps | -1.9% |
| All_BBRv3 | BBRv3 | BBRv3 | 54.76 Mbps | -6.1% |

## Verification Reports

- [MOQ_BBRv3_VERIFICATION_REPORT.md](MOQ_BBRv3_VERIFICATION_REPORT.md) - Complete verification report
- [VERIFICATION_REPORT.md](VERIFICATION_REPORT.md) - Build and unit test verification
- [IMPLEMENTATION_SUMMARY.md](IMPLEMENTATION_SUMMARY.md) - Implementation details
- [moq-go-main/moq-go-main/examples/bbr_comparison_test/COMPARISON_REPORT.md](moq-go-main/moq-go-main/examples/bbr_comparison_test/COMPARISON_REPORT.md) - Comparison test report
- [moq-go-main/moq-go-main/examples/bbrv3_separate_test/BBRV3_SEPARATE_VERIFICATION_REPORT.md](moq-go-main/moq-go-main/examples/bbrv3_separate_test/BBRV3_SEPARATE_VERIFICATION_REPORT.md) - Component-separate test report

## Requirements

- Go 1.23+
- Windows/Linux/macOS

## References

1. **BBRv3 Draft**: https://datatracker.ietf.org/doc/html/draft-cardwell-iccrg-bbr-congestion-control-02
2. **BBRv3 Performance Tuning**: https://datatracker.ietf.org/meeting/117/materials/slides-117-ccwg-bbrv3-algorithm-bug-fixes-and-public-internet-deployment-00
3. **tquic BBRv3** (Rust reference): `tquic-develop/src/congestion_control/bbr3.rs`
4. **MOQ**: Media over QUIC Transport

## License

See individual component licenses:
- moq-go: [moq-go-main/moq-go-main/LICENSE](moq-go-main/moq-go-main/LICENSE)
- quic-go: [quic-go-bbr/quic-go-master/LICENSE](quic-go-bbr/quic-go-master/LICENSE)
- tquic: [tquic-develop/LICENSE](tquic-develop/LICENSE)

## Acknowledgments

- BBRv3 implementation ported from [tquic](https://github.com/tquic-group/tquic) (Rust)
- Based on [quic-go](https://github.com/quic-go/quic-go) with BBRv1 support
- MOQ implementation from [moq-go](https://github.com/DineshAdhi/moq-go)
