package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"os"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	relayAddr  = "localhost:4440"
	objectSize = 1024 // 1KB objects
	numObjects = 500  // Send 500 objects
)

// GenerateTLSConfig generates a TLS config for testing
func GenerateTLSConfig() *tls.Config {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		panic(err)
	}
	template := x509.Certificate{SerialNumber: big.NewInt(1)}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		panic(err)
	}
	return &tls.Config{
		Certificates:       []tls.Certificate{tlsCert},
		NextProtos:         []string{"moq-00"},
		InsecureSkipVerify: true,
	}
}

// Simple relay for testing
func startRelay() (*quic.Listener, error) {
	tlsConfig := GenerateTLSConfig()

	// Create QUIC config with BBRv3 and statistics
	statsConfig := quic.DefaultBBRv3StatsConfig()
	statsConfig.Enabled = true
	statsConfig.LogInterval = 1 * time.Second
	statsConfig.ConnectionID = "relay"

	quicConfig := &quic.Config{
		EnableDatagrams: true,
		Congestion: func() quic.SendAlgorithmWithDebugInfos {
			return quic.NewBBRv3WithStats(nil, statsConfig)
		},
	}

	listener, err := quic.ListenAddr(relayAddr, tlsConfig, quicConfig)
	if err != nil {
		return nil, err
	}

	go func() {
		for {
			conn, err := listener.Accept(context.Background())
			if err != nil {
				return
			}
			go handleConnection(conn)
		}
	}()

	return listener, nil
}

func handleConnection(conn *quic.Conn) {
	for {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			return
		}
		go handleStream(stream)
	}
}

func handleStream(stream *quic.Stream) {
	defer stream.Close()
	for {
		header := make([]byte, 20)
		if _, err := io.ReadFull(stream, header); err != nil {
			return
		}
		payloadLen := uint32(header[16])<<24 | uint32(header[17])<<16 | uint32(header[18])<<8 | uint32(header[19])
		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(stream, payload); err != nil {
			return
		}
		// Echo back
		ack := make([]byte, 20+payloadLen)
		copy(ack, header)
		copy(ack[20:], payload)
		if _, err := stream.Write(ack); err != nil {
			return
		}
	}
}

func main() {
	fmt.Println("========================================")
	fmt.Println("BBRv3 Statistics Collection Test")
	fmt.Println("========================================")
	fmt.Println()

	// Create logs directory
	os.MkdirAll("logs", 0755)

	// Start relay
	relay, err := startRelay()
	if err != nil {
		log.Fatalf("Failed to start relay: %v", err)
	}
	defer relay.Close()

	fmt.Println("[Test] Relay started with BBRv3 statistics collection")
	fmt.Println("[Test] Statistics will be printed every 1 second")
	fmt.Println()

	time.Sleep(500 * time.Millisecond)

	// Create publisher with BBRv3 and statistics
	statsConfig := quic.DefaultBBRv3StatsConfig()
	statsConfig.Enabled = true
	statsConfig.LogInterval = 1 * time.Second
	statsConfig.ConnectionID = "publisher"

	quicConfig := &quic.Config{
		EnableDatagrams: true,
		Congestion: func() quic.SendAlgorithmWithDebugInfos {
			return quic.NewBBRv3WithStats(nil, statsConfig)
		},
	}

	tlsConfig := GenerateTLSConfig()
	conn, err := quic.DialAddr(context.Background(), relayAddr, tlsConfig, quicConfig)
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer conn.CloseWithError(0, "done")

	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Fatalf("Failed to open stream: %v", err)
	}

	fmt.Println("[Test] Publisher connected with BBRv3 statistics collection")
	fmt.Println("[Test] Starting data transfer...")
	fmt.Println()

	// Generate test payload
	payload := make([]byte, objectSize)
	rand.Read(payload)

	// Send objects
	startTime := time.Now()
	bytesSent := int64(0)

	for i := 0; i < numObjects; i++ {
		// Create header
		header := make([]byte, 20)
		payloadLen := uint32(len(payload))
		header[16] = byte(payloadLen >> 24)
		header[17] = byte(payloadLen >> 16)
		header[18] = byte(payloadLen >> 8)
		header[19] = byte(payloadLen)

		// Send
		if _, err := stream.Write(header); err != nil {
			log.Printf("[Test] Error sending header: %v", err)
			break
		}
		if _, err := stream.Write(payload); err != nil {
			log.Printf("[Test] Error sending payload: %v", err)
			break
		}
		bytesSent += int64(len(header)) + int64(len(payload))

		// Wait for ACK
		ack := make([]byte, 20+payloadLen)
		if _, err := io.ReadFull(stream, ack); err != nil {
			log.Printf("[Test] Error reading ACK: %v", err)
			break
		}

		// Progress
		if i%100 == 0 && i > 0 {
			elapsed := time.Since(startTime).Seconds()
			throughput := float64(bytesSent*8) / elapsed / 1000000
			fmt.Printf("[Progress] Objects: %d/%d, Throughput: %.2f Mbps\n", i, numObjects, throughput)
		}
	}

	duration := time.Since(startTime)
	avgThroughput := float64(bytesSent*8) / duration.Seconds() / 1000000

	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("Test Completed")
	fmt.Println("========================================")
	fmt.Printf("Total objects: %d\n", numObjects)
	fmt.Printf("Total bytes: %d\n", bytesSent)
	fmt.Printf("Duration: %.2f seconds\n", duration.Seconds())
	fmt.Printf("Average throughput: %.2f Mbps\n", avgThroughput)
	fmt.Println()
	fmt.Println("BBRv3 statistics were printed above every 1 second.")
	fmt.Println("Look for lines starting with '[BBRv3-Stats]'")
}
