package modbus

import (
	"context"
	"io"
	log "log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jellydator/ttlcache/v3"
)

const (
	proxyReconnectBackoffInitial = 1 * time.Second
	proxyReconnectBackoffMax     = 30 * time.Second
	// Resync buffer limits: max total bytes across fragments, TTL per fragment
	maxResyncBufferSize = 65536
	resyncBufferTTL     = 30 * time.Second
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
			h.closeConnections()
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
			h.closeConnections()
			// Wait for the other goroutine to notice and exit
			<-done
			log.Info("Proxy connection lost, reconnecting", "backoff", backoff)
			h.sleepOrExit(ctx, backoff)
			backoff = h.nextBackoff(backoff)
		case <-ctx.Done():
			// Close connections so forwardData goroutines exit
			h.closeConnections()
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

// closeConnections closes the remote connection (if any) and the local tcpTransporter.
// Caller must not hold h.mu or tcpTransporter.mu.
func (h *ProxyTCPClientHandler) closeConnections() {
	h.mu.Lock()
	if h.remoteConn != nil {
		h.remoteConn.Close()
		h.remoteConn = nil
	}
	h.mu.Unlock()
	h.TCPClientHandler.tcpTransporter.mu.Lock()
	h.TCPClientHandler.tcpTransporter.close()
	h.TCPClientHandler.tcpTransporter.mu.Unlock()
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

// forwardData copies bytes from src to dst.
// For remote->local we resync on start: buffer until we have one complete RTU frame with valid CRC,
// then write it and switch to raw passthrough. That way we never forward a leading partial frame
// (e.g. after reconnect) which would cause CRC errors in the connector.
func (h *ProxyTCPClientHandler) forwardData(ctx context.Context, src, dst net.Conn, direction string) {
	readBuf := make([]byte, 4096)
	if direction == "remote->local" {
		h.forwardRemoteToLocalResync(ctx, src, dst, readBuf)
		return
	}
	// local->remote: raw passthrough
	for {
		select {
		case <-ctx.Done():
			log.Info("Stopping TCP proxy", "direction", direction)
			return
		default:
		}
		src.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, err := src.Read(readBuf)
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
			h.TCPClientHandler.tcpTransporter.RefreshIdle()
			if _, err := dst.Write(readBuf[:n]); err != nil {
				log.Error("Error writing data", "direction", direction, "error", err)
				return
			}
		}
	}
}

// forwardRemoteToLocalResync buffers TCP fragments with TTL and max size, builds a contiguous
// buffer in order, and forwards only complete CRC-valid Modbus RTU frames. Consumed bytes
// (garbage + frame) are removed from fragment storage; remainder stays for next iteration.
func (h *ProxyTCPClientHandler) forwardRemoteToLocalResync(ctx context.Context, src, dst net.Conn, readBuf []byte) {
	fragmentsCache := ttlcache.New(
		ttlcache.WithTTL[uint64, []byte](resyncBufferTTL),
		ttlcache.WithMaxCost[uint64, []byte](maxResyncBufferSize, func(item ttlcache.CostItem[uint64, []byte]) uint64 {
			return uint64(len(item.Value))
		}),
	)
	defer fragmentsCache.Stop()
	go fragmentsCache.Start()

	var nextSeq uint64
	for {
		select {
		case <-ctx.Done():
			log.Info("Stopping TCP proxy", "direction", "remote->local")
			return
		default:
		}
		src.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, err := src.Read(readBuf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			errStr := err.Error()
			if strings.Contains(errStr, "use of closed network connection") ||
				strings.Contains(errStr, "read: connection reset by peer") ||
				strings.Contains(errStr, "broken pipe") {
				log.Info("Connection closed by partner", "direction", "remote->local", "error", err)
				return
			}
			if err != io.EOF {
				log.Error("Error reading data", "direction", "remote->local", "error", err)
			}
			return
		}
		if n == 0 {
			continue
		}
		h.TCPClientHandler.tcpTransporter.RefreshIdle()

		h.evictOldestFragmentsUntilUnderLimit(fragmentsCache, maxResyncBufferSize-n)
		nextSeq++
		fragCopy := make([]byte, n)
		copy(fragCopy, readBuf[:n])
		fragmentsCache.Set(nextSeq, fragCopy, resyncBufferTTL)

		resyncBuf := h.buildBufferFromFragments(fragmentsCache)
		for len(resyncBuf) >= 2 {
			found := false
			for offset := 0; offset <= len(resyncBuf)-2; offset++ {
				bufSlice := resyncBuf[offset:]
				var frame []byte
				h.rtuPackager.TryDecode(&bufSlice, &frame)
				if len(frame) == 0 {
					continue
				}
				if h.rtuPackager.validateCRC(frame) {
					if _, err := dst.Write(frame); err != nil {
						log.Error("Error writing data", "direction", "remote->local", "error", err)
						return
					}
					h.consumeFromFragments(fragmentsCache, offset+len(frame))
					found = true
					break
				}
			}
			if !found {
				break
			}
			resyncBuf = h.buildBufferFromFragments(fragmentsCache)
		}
	}
}

// evictOldestFragmentsUntilUnderLimit deletes fragments with smallest keys until total bytes <= maxBytes.
func (h *ProxyTCPClientHandler) evictOldestFragmentsUntilUnderLimit(cache *ttlcache.Cache[uint64, []byte], maxBytes int) {
	for {
		keys := cache.Keys()
		if len(keys) == 0 {
			return
		}
		slices.Sort(keys)
		var total int
		for _, k := range keys {
			item := cache.Get(k, ttlcache.WithDisableTouchOnHit[uint64, []byte]())
			if item != nil && !item.IsExpired() {
				total += len(item.Value())
			}
		}
		if total <= maxBytes {
			return
		}
		for _, k := range keys {
			item := cache.Get(k, ttlcache.WithDisableTouchOnHit[uint64, []byte]())
			if item != nil && !item.IsExpired() {
				cache.Delete(k)
				break
			}
		}
	}
}

// buildBufferFromFragments concatenates all non-expired fragments in key order.
func (h *ProxyTCPClientHandler) buildBufferFromFragments(cache *ttlcache.Cache[uint64, []byte]) []byte {
	keys := cache.Keys()
	slices.Sort(keys)
	var buf []byte
	for _, k := range keys {
		item := cache.Get(k, ttlcache.WithDisableTouchOnHit[uint64, []byte]())
		if item == nil || item.IsExpired() {
			continue
		}
		buf = append(buf, item.Value()...)
	}
	return buf
}

// consumeFromFragments removes the first bytesToRemove bytes from the fragment cache by
// deleting fully consumed fragments and trimming the fragment that spans the boundary.
func (h *ProxyTCPClientHandler) consumeFromFragments(cache *ttlcache.Cache[uint64, []byte], bytesToRemove int) {
	if bytesToRemove <= 0 {
		return
	}
	keys := cache.Keys()
	slices.Sort(keys)
	for _, k := range keys {
		item := cache.Get(k, ttlcache.WithDisableTouchOnHit[uint64, []byte]())
		if item == nil || item.IsExpired() {
			continue
		}
		v := item.Value()
		fragLen := len(v)
		if bytesToRemove >= fragLen {
			cache.Delete(k)
			bytesToRemove -= fragLen
			if bytesToRemove == 0 {
				return
			}
			continue
		}
		trimmed := make([]byte, fragLen-bytesToRemove)
		copy(trimmed, v[bytesToRemove:])
		ttl := time.Until(item.ExpiresAt())
		if ttl <= 0 {
			ttl = resyncBufferTTL
		}
		cache.Set(k, trimmed, ttl)
		return
	}
}
