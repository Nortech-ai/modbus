package modbus

import (
	"context"
	"io"
	log "log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	proxyReconnectBackoffInitial = 1 * time.Second
	proxyReconnectBackoffMax     = 30 * time.Second
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
	proxyCtx     context.Context
	proxyCancel  context.CancelFunc
	wg           sync.WaitGroup
	// Frame reassembly buffers
	remoteBuffer []byte
	localBuffer  []byte
}

func NewProxyTCPClientHandler(proxyTargetAddress string, localAddress string) *ProxyTCPClientHandler {
	handler := &ProxyTCPClientHandler{
		TCPClientHandler:   TCPClientHandler{},
		proxyTargetAddress: proxyTargetAddress,
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
	h.mu.Lock()
	defer h.mu.Unlock()

	// Create new context for this proxy run (allows restart after Close)
	ctx, cancel := context.WithCancel(context.Background())
	h.proxyCtx = ctx
	h.proxyCancel = cancel

	// Start the proxy forwarding goroutine (it will connect inside the loop)
	h.proxyRunning = true
	h.wg.Add(1)
	go h.runProxy(ctx)

	return nil
}

func (h *ProxyTCPClientHandler) runProxy(ctx context.Context) {
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

	backoff := proxyReconnectBackoffInitial

	for {
		select {
		case <-ctx.Done():
			log.Info("Stopping TCP proxy (shutdown)")
			return
		default:
		}

		// Ensure local connection exists
		h.mu.Lock()
		h.TCPClientHandler.tcpTransporter.mu.Lock()
		err := h.TCPClientHandler.tcpTransporter.connect()
		h.TCPClientHandler.tcpTransporter.mu.Unlock()
		h.mu.Unlock()

		if err != nil {
			log.Warn("Proxy failed to connect to local", "error", err)
			h.sleepOrExit(ctx, backoff)
			backoff = h.nextBackoff(backoff)
			continue
		}

		h.mu.Lock()
		localConn := h.TCPClientHandler.tcpTransporter.conn
		h.mu.Unlock()

		if localConn == nil {
			h.sleepOrExit(ctx, backoff)
			backoff = h.nextBackoff(backoff)
			continue
		}

		// Connect to remote
		remoteConn, err := net.Dial("tcp", h.proxyTargetAddress)
		if err != nil {
			log.Warn("Proxy failed to connect to remote", "address", h.proxyTargetAddress, "error", err)
			h.TCPClientHandler.tcpTransporter.mu.Lock()
			h.TCPClientHandler.tcpTransporter.close()
			h.TCPClientHandler.tcpTransporter.mu.Unlock()
			h.sleepOrExit(ctx, backoff)
			backoff = h.nextBackoff(backoff)
			continue
		}

		h.mu.Lock()
		if h.remoteConn != nil {
			h.remoteConn.Close()
			h.remoteConn = nil
		}
		h.remoteConn = remoteConn
		// Reset buffers to avoid forwarding stale/corrupt data across reconnections
		h.remoteBuffer = nil
		h.localBuffer = nil
		h.mu.Unlock()

		backoff = proxyReconnectBackoffInitial // reset on successful connection

		done := make(chan struct{}, 2)

		// Forward data from localhost to remote
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			defer func() { done <- struct{}{} }()
			h.forwardData(ctx, localConn, remoteConn, "local->remote")
		}()

		// Forward data from remote to localhost
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			defer func() { done <- struct{}{} }()
			h.forwardData(ctx, remoteConn, localConn, "remote->local")
		}()

		// Wait for either direction to finish or shutdown
		select {
		case <-done:
			// One direction failed: close both connections so the other goroutine exits
			h.mu.Lock()
			if h.remoteConn != nil {
				h.remoteConn.Close()
				h.remoteConn = nil
			}
			h.mu.Unlock()
			h.TCPClientHandler.tcpTransporter.mu.Lock()
			h.TCPClientHandler.tcpTransporter.close()
			h.TCPClientHandler.tcpTransporter.mu.Unlock()
			// Wait for the other goroutine to notice and exit
			<-done
			log.Info("Proxy connection lost, reconnecting", "backoff", backoff)
			h.sleepOrExit(ctx, backoff)
			backoff = h.nextBackoff(backoff)
		case <-ctx.Done():
			// Close connections so forwardData goroutines exit
			h.mu.Lock()
			if h.remoteConn != nil {
				h.remoteConn.Close()
				h.remoteConn = nil
			}
			h.mu.Unlock()
			h.TCPClientHandler.tcpTransporter.mu.Lock()
			h.TCPClientHandler.tcpTransporter.close()
			h.TCPClientHandler.tcpTransporter.mu.Unlock()
			<-done
			<-done
			return
		}
	}
}

func (h *ProxyTCPClientHandler) sleepOrExit(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-t.C:
		return
	}
}

func (h *ProxyTCPClientHandler) nextBackoff(current time.Duration) time.Duration {
	next := min(current*2, proxyReconnectBackoffMax)
	return next
}

func (h *ProxyTCPClientHandler) Connect() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Connect to localhost:1502 using the standard TCP client handler
	return h.TCPClientHandler.Connect()
}

func (h *ProxyTCPClientHandler) Close() error {
	h.mu.Lock()
	if h.proxyCancel != nil {
		h.proxyCancel()
		h.proxyCancel = nil
	}
	proxyRunning := h.proxyRunning
	h.mu.Unlock()

	if proxyRunning {
		h.wg.Wait()
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// Close remote connection
	if h.remoteConn != nil {
		h.remoteConn.Close()
		h.remoteConn = nil
	}

	// Close local connection
	return h.TCPClientHandler.Close()
}

func (h *ProxyTCPClientHandler) forwardData(ctx context.Context, src, dst net.Conn, direction string) {
	buffer := make([]byte, 4096)
	var frameBuffer *[]byte

	if direction == "remote->local" {
		frameBuffer = &h.remoteBuffer
	} else {
		frameBuffer = &h.localBuffer
	}

	for {
		select {
		case <-ctx.Done():
			log.Info("Stopping TCP proxy", "direction", direction)
			return
		default:
		}

		src.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

		n, err := src.Read(buffer)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}

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
			// Refresh idle timer when reading from local so connection is not closed during proxy activity
			if direction == "local->remote" {
				h.TCPClientHandler.tcpTransporter.RefreshIdle()
			}
			*frameBuffer = append(*frameBuffer, buffer[:n]...)
			h.processFrames(ctx, frameBuffer, dst, direction)
		}
	}
}

func (h *ProxyTCPClientHandler) processFrames(ctx context.Context, frameBuffer *[]byte, dst net.Conn, direction string) {
	for len(*frameBuffer) > 0 {
		select {
		case <-ctx.Done():
			return
		default:
		}

		frame := make([]byte, 0)
		h.rtuPackager.TryDecode(frameBuffer, &frame)
		if len(frame) == 0 {
			break
		}

		// Validate CRC before forwarding to avoid propagating corrupted frames
		if !h.rtuPackager.validateCRC(frame) {
			log.Warn("Dropping RTU frame with invalid CRC", "direction", direction, "frameLen", len(frame))
			continue
		}

		_, err := dst.Write(frame)
		if err != nil {
			log.Error("Error writing complete frame", "direction", direction, "error", err)
			return
		}
		// Refresh idle timer when writing to local so connection is not closed during proxy activity
		if direction == "remote->local" {
			h.TCPClientHandler.tcpTransporter.RefreshIdle()
		}
	}
}
