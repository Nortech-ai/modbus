package modbus

import (
	"encoding/binary"
	"fmt"
)

// TCPHeaderSize is the size of the Modbus TCP MBAP header in bytes.
//
// MBAP header:
//   - Transaction identifier: 2 bytes
//   - Protocol identifier: 2 bytes
//   - Length: 2 bytes
//   - Unit identifier: 1 byte
const TCPHeaderSize = tcpHeaderSize

// TCPFrame contains the decoded Modbus TCP MBAP header fields and PDU.
type TCPFrame struct {
	TransactionID uint16
	ProtocolID    uint16
	// Length is the MBAP length field (bytes following: UnitID + PDU).
	Length uint16
	UnitID byte
	PDU    *ProtocolDataUnit
}

// DecodeTCP decodes a Modbus TCP ADU into MBAP header fields and PDU.
//
// Validation is relaxed to accept real-world captures: the frame must have at least
// unit ID + function code, and at least as many bytes as the MBAP length field indicates.
// Extra trailing bytes (e.g. padding) are allowed.
func DecodeTCP(adu []byte) (*TCPFrame, error) {
	// Need at least MBAP header + function code.
	if len(adu) < tcpHeaderSize+1 {
		return nil, fmt.Errorf("modbus: tcp adu too short: %d", len(adu))
	}

	pdu, err := (&tcpPackager{}).Decode(adu)
	if err != nil {
		return nil, err
	}

	return &TCPFrame{
		TransactionID: binary.BigEndian.Uint16(adu[0:2]),
		ProtocolID:    binary.BigEndian.Uint16(adu[2:4]),
		Length:        binary.BigEndian.Uint16(adu[4:6]),
		UnitID:        adu[6],
		PDU:           pdu,
	}, nil
}

// DecodePDUFromTCP extracts the PDU from a Modbus TCP ADU.
// For MBAP header fields, use DecodeTCP.
func DecodePDUFromTCP(adu []byte) (*ProtocolDataUnit, error) {
	frame, err := DecodeTCP(adu)
	if err != nil {
		return nil, err
	}
	return frame.PDU, nil
}

