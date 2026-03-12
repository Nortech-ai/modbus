package modbus

import (
	"bytes"
	"testing"
)

func TestDecodeTCP(t *testing.T) {
	adu := []byte{0, 1, 0, 0, 0, 6, 17, 3, 0, 120, 0, 3}

	frame, err := DecodeTCP(adu)
	if err != nil {
		t.Fatal(err)
	}

	if frame.TransactionID != 1 {
		t.Fatalf("TransactionID: expected %v, got %v", 1, frame.TransactionID)
	}
	if frame.ProtocolID != 0 {
		t.Fatalf("ProtocolID: expected %v, got %v", 0, frame.ProtocolID)
	}
	if frame.Length != 6 {
		t.Fatalf("Length: expected %v, got %v", 6, frame.Length)
	}
	if frame.UnitID != 17 {
		t.Fatalf("UnitID: expected %v, got %v", 17, frame.UnitID)
	}
	if frame.PDU == nil {
		t.Fatalf("PDU: expected non-nil")
	}
	if frame.PDU.FunctionCode != 3 {
		t.Fatalf("FunctionCode: expected %v, got %v", 3, frame.PDU.FunctionCode)
	}
	expected := []byte{0, 120, 0, 3}
	if !bytes.Equal(expected, frame.PDU.Data) {
		t.Fatalf("Data: expected %v, got %v", expected, frame.PDU.Data)
	}
}

func TestDecodeTCPTooShort(t *testing.T) {
	// Less than MBAP header + function code (7 + 1).
	_, err := DecodeTCP([]byte{0, 1, 0, 0, 0, 6, 17})
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestDecodeTCPTruncated(t *testing.T) {
	// Header says length=7 (6 bytes after header), but we only have 5 bytes (truncated).
	adu := []byte{0, 1, 0, 0, 0, 7, 17, 3, 0, 120, 0}
	_, err := DecodeTCP(adu)
	if err == nil {
		t.Fatalf("expected error for truncated frame")
	}
}

func TestDecodeTCPRelaxedExtraBytes(t *testing.T) {
	// Real-world case: 260 bytes total, MBAP length=253 (252 bytes PDU after unit ID).
	// Strict decode would reject (253 != 252); relaxed decode accepts and uses actual buffer.
	adu := make([]byte, 260)
	adu[0], adu[1] = 0x37, 0x4e // transaction ID
	adu[2], adu[3] = 0, 0        // protocol ID
	adu[4], adu[5] = 0, 253      // length = 253 (unit ID + 252 bytes PDU)
	adu[6] = 1                   // unit ID
	adu[7] = 0x10                // function code (Write Multiple Registers)
	copy(adu[8:], bytes.Repeat([]byte{0xff}, 252))

	frame, err := DecodeTCP(adu)
	if err != nil {
		t.Fatalf("relaxed decode should accept frame with extra byte: %v", err)
	}
	if frame.TransactionID != 0x374e {
		t.Fatalf("TransactionID: expected 0x374e, got %v", frame.TransactionID)
	}
	if frame.Length != 253 {
		t.Fatalf("Length: expected 253, got %v", frame.Length)
	}
	if frame.UnitID != 1 {
		t.Fatalf("UnitID: expected 1, got %v", frame.UnitID)
	}
	if frame.PDU.FunctionCode != 0x10 {
		t.Fatalf("FunctionCode: expected 0x10, got %v", frame.PDU.FunctionCode)
	}
	if len(frame.PDU.Data) != 252 {
		t.Fatalf("PDU data length: expected 252, got %v", len(frame.PDU.Data))
	}
}

