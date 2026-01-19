// Copyright 2018 xft. All rights reserved.
// This software may be modified and distributed under the terms
// of the BSD license. See the LICENSE file for details.

package modbus

import (
	"io"
	"net"
	"time"
)

// RTUOverTCPClientHandler implements Packager and Transporter interface.
type RTUOverTCPClientHandler struct {
	rtuPackager
	rtuTCPTransporter
}

// NewRTUOverTCPClientHandler allocates and initializes a RTUOverTCPClientHandler.
func NewRTUOverTCPClientHandler(address string) *RTUOverTCPClientHandler {
	handler := &RTUOverTCPClientHandler{}
	handler.Address = address
	handler.Timeout = tcpTimeout
	handler.IdleTimeout = tcpIdleTimeout
	return handler
}

// RTUOverTCPClient creates RTU over TCP client with default handler and given connect string.
func RTUOverTCPClient(address string) Client {
	handler := NewRTUOverTCPClientHandler(address)
	return NewClient(handler)
}

// rtuTCPTransporter implements Transporter interface.
type rtuTCPTransporter struct {
	tcpTransporter
}

func (mb *rtuTCPTransporter) Send(aduRequest []byte) (aduResponse []byte, err error) {
	mb.tcpTransporter.mu.Lock()
	defer mb.tcpTransporter.mu.Unlock()

	// Establish a new connection if not connected
	if err = mb.tcpTransporter.connect(); err != nil {
		return
	}
	// Set write and read timeout
	var timeout time.Time
	if mb.Timeout > 0 {
		timeout = time.Now().Add(mb.Timeout)
	}
	if err = mb.conn.SetDeadline(timeout); err != nil {
		//slog.Error("Error setting deadline - connection may be broken", "error", err)
		mb.tcpTransporter.close() // Close broken connection
		return
	}

	// Send the request
	mb.tcpTransporter.logf("modbus: sending % x\n", aduRequest)
	if _, err = mb.conn.Write(aduRequest); err != nil {
		// Write errors usually indicate connection is broken
		//slog.Error("Error writing request - connection broken", "error", err)
		mb.tcpTransporter.close() // Close broken connection
		return
	}
	function := aduRequest[1]
	functionFail := aduRequest[1] & 0x80
	bytesToRead := calculateResponseLength(aduRequest)

	var n int
	var n1 int
	var data [rtuMaxSize]byte
	//We first read the minimum length and then read either the full package
	//or the error package, depending on the error status (byte 2 of the response)
	n, err = io.ReadAtLeast(mb.conn, data[:], rtuMinSize)
	if err != nil {
		// Check if this is a timeout (slave might be slow) vs connection error
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			// Timeout: Slave is slow or packet was lost, but connection might still be valid
			// Close connection to be safe (Modbus doesn't handle out-of-order responses well)
			// Caller should retry with a new connection
			//slog.Error("Read timeout - slave may be slow or packet lost", "error", err)
			mb.tcpTransporter.close() // Close connection, caller should retry
			return
		}
		// Connection error: Connection is broken, must close
		//slog.Error("Connection error during read", "error", err)
		mb.tcpTransporter.close() // Close broken connection
		return
	}
	//if the function is correct
	if data[1] == function {
		//we read the rest of the bytes
		if n < bytesToRead {
			if bytesToRead > rtuMinSize && bytesToRead <= rtuMaxSize {
				if bytesToRead > n {
					n1, err = io.ReadFull(mb.conn, data[n:bytesToRead])
					n += n1
					if err != nil {
						//	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						//		slog.Error("Read timeout while reading full response", "error", err)
						// } else {
						//	slog.Error("Connection error while reading full response", "error", err)
						// }
						mb.tcpTransporter.close() // Close broken connection
						return
					}
				}
			}
		}
	} else if data[1] == functionFail {
		//for error we need to read 5 bytes
		if n < rtuExceptionSize {
			n1, err = io.ReadFull(mb.conn, data[n:rtuExceptionSize])
			if err != nil {
				//	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				//	slog.Error("Read timeout while reading exception response", "error", err)
				// } else {
				// 	slog.Error("Connection error while reading exception response", "error", err)
				// }
				mb.tcpTransporter.close() // Close broken connection
				return
			}
		}
		n += n1
	}

	if err != nil {
		//	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		//	slog.Error("Read timeout - slave may be slow", "error", err)
		// } else {
		//	slog.Error("Connection error", "error", err)
		// }
		mb.tcpTransporter.close() // Close broken connection
		return
	}
	aduResponse = data[:n]
	mb.logf("modbus: received % x\n", aduResponse)
	// Update last activity after successful operation
	mb.tcpTransporter.lastActivity = time.Now()
	mb.tcpTransporter.startCloseTimer()
	return
}
