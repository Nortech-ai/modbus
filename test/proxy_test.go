// Copyright 2018 xft. All rights reserved.
// This software may be modified and distributed under the terms
// of the BSD license. See the LICENSE file for details.

package test

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Nortech-ai/modbus"
	"github.com/alecthomas/assert/v2"
	mbserver "github.com/postmannen/modbusgenerator"
)

const (
	moxaSimAddr = "127.0.0.1:1502"
	serverAddr  = "127.0.0.1:5020"
)

// startMOXASimulator starts a TCP server that simulates a MOXA serial-to-TCP gateway:
// it listens on listenAddr and, once two clients have connected, bridges bytes between them.
// In production the MOXA accepts the app (proxy) on one side and exposes serial traffic;
// here we simulate that by accepting both the proxy and the test client and copying data through.
func startMOXASimulator(t *testing.T, listenAddr string) func() {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		t.Fatalf("MOXA sim listen %s: %v", listenAddr, err)
	}

	done := make(chan struct{})

	go func() {
		defer ln.Close()
		for {
			conn1, err := ln.Accept()
			if err != nil {
				select {
				case <-done:
					return
				default:
					t.Logf("MOXA sim accept: %v", err)
				}
				return
			}

			conn2, err := ln.Accept()
			if err != nil {
				conn1.Close()
				select {
				case <-done:
					return
				default:
					t.Logf("MOXA sim accept second: %v", err)
				}
				return
			}

			// Bridge the two connections (simulate MOXA forwarding serial <-> TCP)
			go func() {
				defer conn1.Close()
				defer conn2.Close()
				go io.Copy(conn2, conn1)
				io.Copy(conn1, conn2)
			}()
		}
	}()

	return func() {
		close(done)
		ln.Close()
	}
}

func TestProxyWithMOXASimulator(t *testing.T) {
	// 1. Modbus server (the real device / backend)
	server := mbserver.NewServer()
	err := server.ListenRTUTCP(serverAddr)
	assert.NoError(t, err)
	defer server.Close()

	// 2. MOXA simulator: listens on 1502, accepts proxy + test client, bridges traffic
	stopMOXA := startMOXASimulator(t, moxaSimAddr)
	defer stopMOXA()

	// 3. Proxy: connects to MOXA (1502) and to server (5020), forwards RTU frames
	proxyHandler := modbus.NewProxyTCPClientHandler(serverAddr, moxaSimAddr)
	proxyHandler.Timeout = 5 * time.Second
	err = proxyHandler.Connect()
	assert.NoError(t, err)
	defer proxyHandler.Close()
	err = proxyHandler.StartProxy()
	assert.NoError(t, err)

	// 4. Test client (simulates the PLC/client that writes via MOXA): connects to MOXA sim
	clientHandler := modbus.NewRTUOverTCPClientHandler(moxaSimAddr)
	clientHandler.Timeout = 5 * time.Second
	clientHandler.SlaveId = 1
	err = clientHandler.Connect()
	assert.NoError(t, err)
	defer clientHandler.Close()

	client := modbus.NewClient(clientHandler)
	writeAddr := uint16(0x0001)
	writeValues := []byte{0x00, 0x0A, 0x01, 0x02}
	_, err = client.WriteMultipleRegisters(writeAddr, 2, writeValues)
	assert.NoError(t, err, "write through MOXA sim and proxy failed")

	// 5. Verify on server
	registers := server.HoldingRegisters[writeAddr : writeAddr+2]
	assert.Equal(t, []uint16{0x000A, 0x0102}, registers)

	// 6. Read back through same path (client -> MOXA sim -> proxy -> server)
	results, err := client.ReadHoldingRegisters(writeAddr, 2)
	assert.NoError(t, err, "read through MOXA sim and proxy failed")
	assert.Equal(t, 4, len(results))
	assert.Equal(t, []byte{0x00, 0x0A, 0x01, 0x02}, results)
}

// TestProxyClientDisconnectReconnect verifies that when the client disconnects,
// the proxy detects it (logs "Connection closed" / "reconnecting"), retries, and
// connection is restored so a new client can use the proxy again.
func TestProxyClientDisconnectReconnect(t *testing.T) {
	// 1. Modbus server
	server := mbserver.NewServer()
	err := server.ListenRTUTCP(serverAddr)
	assert.NoError(t, err)
	defer server.Close()

	// 2. MOXA simulator
	stopMOXA := startMOXASimulator(t, moxaSimAddr)
	defer stopMOXA()

	// 3. Capture proxy logs so we can verify disconnect/reconnect messages
	var logBuf bytes.Buffer
	captureHandler := slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})
	originalLogger := slog.Default()
	slog.SetDefault(slog.New(captureHandler))
	defer slog.SetDefault(originalLogger)

	// 4. Proxy
	proxyHandler := modbus.NewProxyTCPClientHandler(serverAddr, moxaSimAddr)
	proxyHandler.Timeout = 5 * time.Second
	err = proxyHandler.Connect()
	assert.NoError(t, err)
	defer proxyHandler.Close()
	err = proxyHandler.StartProxy()
	assert.NoError(t, err)

	// 5. First client: write, then disconnect
	clientHandler1 := modbus.NewRTUOverTCPClientHandler(moxaSimAddr)
	clientHandler1.Timeout = 5 * time.Second
	clientHandler1.SlaveId = 1
	err = clientHandler1.Connect()
	assert.NoError(t, err)

	client1 := modbus.NewClient(clientHandler1)
	writeAddr := uint16(0x0001)
	writeValues := []byte{0x00, 0x0B, 0x01, 0x03}
	_, err = client1.WriteMultipleRegisters(writeAddr, 2, writeValues)
	assert.NoError(t, err, "write before disconnect failed")

	// Disconnect client (closes connection; MOXA bridge will close proxy's local conn too)
	clientHandler1.Close()

	// 6. Wait for proxy to detect disconnect and retry (backoff 1s, then reconnect)
	time.Sleep(2 * time.Second)

	// 7. Verify we saw connection-closed / reconnecting in logs
	logStr := logBuf.String()
	assert.True(t, strings.Contains(logStr, "Connection closed by partner") || strings.Contains(logStr, "connection reset") || strings.Contains(logStr, "closed"),
		"expected proxy to log connection closed; got: %s", logStr)
	assert.True(t, strings.Contains(logStr, "reconnecting"),
		"expected proxy to log reconnecting; got: %s", logStr)

	// 8. New client connects (MOXA sim accepts proxy + new client again)
	clientHandler2 := modbus.NewRTUOverTCPClientHandler(moxaSimAddr)
	clientHandler2.Timeout = 5 * time.Second
	clientHandler2.SlaveId = 1
	err = clientHandler2.Connect()
	assert.NoError(t, err)
	defer clientHandler2.Close()

	client2 := modbus.NewClient(clientHandler2)
	results, err := client2.ReadHoldingRegisters(writeAddr, 2)
	assert.NoError(t, err, "read after reconnect failed - connection not restored")
	assert.Equal(t, 4, len(results))
	assert.Equal(t, []byte{0x00, 0x0B, 0x01, 0x03}, results)

	// 9. Verify on server
	registers := server.HoldingRegisters[writeAddr : writeAddr+2]
	assert.Equal(t, []uint16{0x000B, 0x0103}, registers)
}
