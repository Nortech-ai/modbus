package modbus

import (
	"io"
	log "log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

type ProxyTCPClientHandler struct {
	TCPClientHandler
	rtuPackager

	// Proxy-specific fields
	proxyTargetAddress string

	// Connection management
	mu           sync.Mutex
	remoteConn   net.Conn
	proxyRunning bool
	stopChan     chan struct{}
	wg           sync.WaitGroup
	// Frame reassembly buffers
	remoteBuffer []byte
	localBuffer  []byte
}

func NewProxyTCPClientHandler(proxyTargetAddress string, localAddress string) *ProxyTCPClientHandler {
	handler := &ProxyTCPClientHandler{
		TCPClientHandler:   TCPClientHandler{},
		proxyTargetAddress: proxyTargetAddress,
		stopChan:           make(chan struct{}),
	}
	// Set the local address for the embedded TCPClientHandler
	handler.TCPClientHandler.tcpTransporter.Address = localAddress
	return handler
}

func (h *ProxyTCPClientHandler) Send(aduRequest []byte) (aduResponse []byte, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// If proxy is not running, start it
	if !h.proxyRunning {
		if err = h.StartProxy(); err != nil {
			return nil, err
		}
	}

	// Use the standard TCP client handler to send to localhost:1502
	return h.TCPClientHandler.Send(aduRequest)
}

func (h *ProxyTCPClientHandler) StartProxy() error {
	// Connect to the remote target
	remoteConn, err := net.Dial("tcp", h.proxyTargetAddress)
	if err != nil {
		return err
	}
	h.remoteConn = remoteConn

	// Start the proxy forwarding goroutine
	h.proxyRunning = true
	h.wg.Add(1)
	go h.runProxy()

	return nil
}

func (h *ProxyTCPClientHandler) runProxy() {
	defer h.wg.Done()
	defer func() {
		h.mu.Lock()
		h.proxyRunning = false
		if h.remoteConn != nil {
			h.remoteConn.Close()
			h.remoteConn = nil
		}
		h.mu.Unlock()
	}()

	// Get the local connection from the TCP client handler
	h.mu.Lock()
	localConn := h.TCPClientHandler.tcpTransporter.conn
	h.mu.Unlock()

	if localConn == nil {
		return
	}

	// Create channels for coordination
	done := make(chan struct{}, 2)

	// Forward data from localhost:1502 to remote
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer func() { done <- struct{}{} }()
		h.forwardData(localConn, h.remoteConn, "local->remote")
		//io.Copy(h.remoteConn, localConn)
	}()

	// Forward data from remote to localhost:1502
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer func() { done <- struct{}{} }()
		h.forwardData(h.remoteConn, localConn, "remote->local")
		//io.Copy(localConn, h.remoteConn)
	}()

	// Wait for either direction to finish or stop signal
	select {
	case <-done:
		// One direction finished, stop the other
	case <-h.stopChan:
		// Stop signal received
	}
}

func (h *ProxyTCPClientHandler) Connect() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Connect to localhost:1502 using the standard TCP client handler
	return h.TCPClientHandler.Connect()
}

func (h *ProxyTCPClientHandler) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Stop the proxy
	if h.proxyRunning {
		close(h.stopChan)
		h.proxyRunning = false

		// Wait for proxy goroutines to finish
		h.mu.Unlock()
		h.wg.Wait()
		h.mu.Lock()
	}

	// Close remote connection
	if h.remoteConn != nil {
		h.remoteConn.Close()
		h.remoteConn = nil
	}

	// Close local connection
	return h.TCPClientHandler.Close()
}

func (h *ProxyTCPClientHandler) forwardData(src, dst net.Conn, direction string) {

	buffer := make([]byte, 4096)
	var frameBuffer *[]byte

	// Choose the appropriate buffer based on direction
	if direction == "remote->local" {
		frameBuffer = &h.remoteBuffer
	} else {
		frameBuffer = &h.localBuffer
	}

	for {
		// Check for shutdown signal first
		select {
		case <-h.stopChan:
			log.Info("Stopping TCP proxy", "direction", direction)
			return
		default:
		}

		// Set read deadline to allow periodic shutdown checks
		src.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

		n, err := src.Read(buffer)
		if err != nil {
			// Check if it's a timeout error (expected for periodic doneCh checks)
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				// Timeout is expected, continue to check doneCh
				continue
			}

			// Check for "use of closed network connection" and similar connection errors. If so return
			// by cleaning up
			errStr := err.Error()
			if strings.Contains(errStr, "use of closed network connection") ||
				strings.Contains(errStr, "read: connection reset by peer") ||
				strings.Contains(errStr, "broken pipe") {
				log.Info("Connection closed by partner", "direction", direction, "error", err)
				return
			}

			if err != io.EOF {
				log.Error("Error reading data", "direction", direction, "error", err)

			}
			return
		}

		if n > 0 {
			// Add to frame buffer
			*frameBuffer = append(*frameBuffer, buffer[:n]...)

			// Process complete frames
			h.processFrames(frameBuffer, dst, direction)
		}
	}
}

func (h *ProxyTCPClientHandler) processFrames(frameBuffer *[]byte, dst net.Conn, direction string) {
	for len(*frameBuffer) > 0 {
		frame := make([]byte, 0)
		h.rtuPackager.TryDecode(frameBuffer, &frame)
		if len(frame) == 0 {
			break
		}

		// Write the complete frame to the destination
		_, err := dst.Write(frame)
		if err != nil {
			log.Error("Error writing complete frame", "direction", direction, "error", err)
			return
		}
	}
}
