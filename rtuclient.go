// Copyright 2014 Quoc-Viet Nguyen. All rights reserved.
// This software may be modified and distributed under the terms
// of the BSD license. See the LICENSE file for details.

package modbus

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

const (
	rtuMinSize = 4
	rtuMaxSize = 256

	rtuExceptionSize = 5
)

// RTUClientHandler implements Packager and Transporter interface.
type RTUClientHandler struct {
	rtuPackager
	rtuSerialTransporter
}

// NewRTUClientHandler allocates and initializes a RTUClientHandler.
func NewRTUClientHandler(address string) *RTUClientHandler {
	handler := &RTUClientHandler{}
	handler.Address = address
	handler.Timeout = serialTimeout
	handler.IdleTimeout = serialIdleTimeout
	return handler
}

// RTUClient creates RTU client with default handler and given connect string.
func RTUClient(address string) Client {
	handler := NewRTUClientHandler(address)
	return NewClient(handler)
}

// rtuPackager implements Packager interface.
type rtuPackager struct {
	SlaveId byte
}

// Encode encodes PDU in a RTU frame:
//
//	Slave Address   : 1 byte
//	Function        : 1 byte
//	Data            : 0 up to 252 bytes
//	CRC             : 2 byte
func (mb *rtuPackager) Encode(pdu *ProtocolDataUnit) (adu []byte, err error) {
	length := len(pdu.Data) + 4
	if length > rtuMaxSize {
		err = fmt.Errorf("modbus: length of data '%v' must not be bigger than '%v'", length, rtuMaxSize)
		return
	}
	adu = make([]byte, length)

	adu[0] = mb.SlaveId
	adu[1] = pdu.FunctionCode
	copy(adu[2:], pdu.Data)

	// Append crc
	var crc crc
	crc.reset().pushBytes(adu[0 : length-2])
	checksum := crc.value()

	adu[length-1] = byte(checksum >> 8)
	adu[length-2] = byte(checksum)
	return
}

// Verify verifies response length and slave id.
func (mb *rtuPackager) Verify(aduRequest []byte, aduResponse []byte) (err error) {
	length := len(aduResponse)
	// Minimum size (including address, function and CRC)
	if length < rtuMinSize {
		err = fmt.Errorf("modbus: response length '%v' does not meet minimum '%v'", length, rtuMinSize)
		return
	}
	// Slave address must match
	if aduResponse[0] != aduRequest[0] {
		err = fmt.Errorf("modbus: response slave id '%v' does not match request '%v'", aduResponse[0], aduRequest[0])
		return
	}
	return
}

// Decode extracts PDU from RTU frame and verify CRC.
func (mb *rtuPackager) Decode(adu []byte) (pdu *ProtocolDataUnit, err error) {
	length := len(adu)
	// Calculate checksum
	var crc crc
	crc.reset().pushBytes(adu[0 : length-2])
	checksum := uint16(adu[length-1])<<8 | uint16(adu[length-2])
	if checksum != crc.value() {
		err = fmt.Errorf("modbus: response crc '%v' does not match expected '%v'", checksum, crc.value())
		return
	}
	// Function code & data
	pdu = &ProtocolDataUnit{}
	pdu.FunctionCode = adu[1]
	pdu.Data = adu[2 : length-2]
	return
}

func (mb *rtuPackager) TryDecode(buffer *[]byte, frame *[]byte) {

	// Check if we have enough data for a complete frame
	frameLength := mb.findFrameInBuffer(*buffer)
	if frameLength == 0 {
		// Not enough data for a complete frame
		return
	}

	if len(*buffer) < frameLength {
		// Not enough data for complete frame
		return
	}

	// Extract complete frame
	*frame = (*buffer)[:frameLength]
	*buffer = (*buffer)[frameLength:]
}

func (mb *rtuPackager) findFrameInBuffer(data []byte) int {
	if len(data) < 2 {
		return 0
	}
	// For RTU over TCP, the frame structure is:
	// [Slave ID][Function Code][Data...][CRC Low][CRC High]

	_ = data[0] // slaveID
	functionCode := data[1]

	// Calculate expected frame length based on function code
	var expectedLength int

	switch functionCode {
	case 0x01, 0x02, 0x03, 0x04, 0x05, 0x06:
		// Read/Write single register/coil functions
		// [Slave ID][Function Code][Address High][Address Low][Value High][Value Low][CRC Low][CRC High]
		expectedLength = 8
	case 0x0F, 0x10:
		// Write multiple registers/coils
		// [Slave ID][Function Code][Address High][Address Low][Quantity High][Quantity Low][Byte Count][Data...][CRC Low][CRC High]
		if len(data) < 6 {
			return 0 // Need at least 6 bytes to determine length
		}
		byteCount := int(data[6])
		expectedLength = 9 + byteCount // 7 header bytes + data + 2 CRC bytes
	case 0x17:
		// Read/Write Multiple Registers
		// [Slave ID][Function Code][Read Address High][Read Address Low][Read Quantity High][Read Quantity Low][Write Address High][Write Address Low][Write Quantity High][Write Quantity Low][Write Byte Count][Write Data...][CRC Low][CRC High]
		if len(data) < 12 {
			return 0 // Need at least 12 bytes to determine length
		}
		writeByteCount := int(data[11])
		expectedLength = 13 + writeByteCount // 12 header bytes + data + 2 CRC bytes
	default:
		// For unknown function codes, try to find frame boundary by looking for valid CRC
		// This is a fallback method
		return mb.findFrameByCRC(data)
	}

	// Check if we have enough data for the expected frame length
	if len(data) >= expectedLength {
		return expectedLength
	}

	return 0 // Not enough data for complete frame
}

func (mb *rtuPackager) findFrameByCRC(data []byte) int {
	// Try to find frame boundary by validating CRC
	// This is a fallback for unknown function codes
	for i := 4; i <= len(data)-2; i++ {
		if mb.validateCRC(data[:i+2]) {
			return i + 2
		}
	}
	return 0
}

func (mb *rtuPackager) validateCRC(data []byte) bool {
	if len(data) < 3 {
		return false
	}

	// Extract CRC from last 2 bytes
	crcReceived := uint16(data[len(data)-2]) | (uint16(data[len(data)-1]) << 8)

	// Calculate CRC for data without the CRC bytes
	var crc crc
	crc.reset().pushBytes(data[:len(data)-2])
	return crcReceived == crc.value()

}

// rtuSerialTransporter implements Transporter interface.
type rtuSerialTransporter struct {
	serialPort
}

func (mb *rtuSerialTransporter) Send(aduRequest []byte) (aduResponse []byte, err error) {
	// Make sure port is connected
	if err = mb.serialPort.connect(); err != nil {
		return
	}
	// Start the timer to close when idle
	mb.serialPort.lastActivity = time.Now()
	mb.serialPort.startCloseTimer()

	// Send the request
	mb.serialPort.logf("modbus: sending % x\n", aduRequest)
	if _, err = mb.port.Write(aduRequest); err != nil {
		return
	}
	function := aduRequest[1]
	functionFail := aduRequest[1] & 0x80
	bytesToRead := calculateResponseLength(aduRequest)
	time.Sleep(mb.calculateDelay(len(aduRequest) + bytesToRead))

	var n int
	var n1 int
	var data [rtuMaxSize]byte
	//We first read the minimum length and then read either the full package
	//or the error package, depending on the error status (byte 2 of the response)
	n, err = io.ReadAtLeast(mb.port, data[:], rtuMinSize)
	if err != nil {
		return
	}
	//if the function is correct
	if data[1] == function {
		//we read the rest of the bytes
		if n < bytesToRead {
			if bytesToRead > rtuMinSize && bytesToRead <= rtuMaxSize {
				if bytesToRead > n {
					n1, err = io.ReadFull(mb.port, data[n:bytesToRead])
					n += n1
				}
			}
		}
	} else if data[1] == functionFail {
		//for error we need to read 5 bytes
		if n < rtuExceptionSize {
			n1, err = io.ReadFull(mb.port, data[n:rtuExceptionSize])
		}
		n += n1
	}

	if err != nil {
		return
	}
	aduResponse = data[:n]
	mb.serialPort.logf("modbus: received % x\n", aduResponse)
	return
}

// calculateDelay roughly calculates time needed for the next frame.
// See MODBUS over Serial Line - Specification and Implementation Guide (page 13).
func (mb *rtuSerialTransporter) calculateDelay(chars int) time.Duration {
	var characterDelay, frameDelay int // us

	if mb.BaudRate <= 0 || mb.BaudRate > 19200 {
		characterDelay = 750
		frameDelay = 1750
	} else {
		characterDelay = 15000000 / mb.BaudRate
		frameDelay = 35000000 / mb.BaudRate
	}
	return time.Duration(characterDelay*chars+frameDelay) * time.Microsecond
}

func calculateResponseLength(adu []byte) int {
	length := rtuMinSize
	switch adu[1] {
	case FuncCodeReadDiscreteInputs,
		FuncCodeReadCoils:
		count := int(binary.BigEndian.Uint16(adu[4:]))
		length += 1 + count/8
		if count%8 != 0 {
			length++
		}
	case FuncCodeReadInputRegisters,
		FuncCodeReadHoldingRegisters,
		FuncCodeReadWriteMultipleRegisters:
		count := int(binary.BigEndian.Uint16(adu[4:]))
		length += 1 + count*2
	case FuncCodeWriteSingleCoil,
		FuncCodeWriteMultipleCoils,
		FuncCodeWriteSingleRegister,
		FuncCodeWriteMultipleRegisters:
		length += 4
	case FuncCodeMaskWriteRegister:
		length += 6
	case FuncCodeReadFIFOQueue:
		// undetermined
	default:
	}
	return length
}
