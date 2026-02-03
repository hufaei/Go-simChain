package tcp

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"simchain-go/internal/types"
)

func TestFrameEncodeDecodeRoundTrip(t *testing.T) {
	msg := types.Message{
		Type:      types.MsgGetTip,
		From:      "node-a",
		Timestamp: 1710000000000,
		Payload:   types.GetTipPayload{},
	}

	frame, err := encodeFrame(msg, 1024)
	if err != nil {
		t.Fatalf("encodeFrame: %v", err)
	}

	got, err := readFrame(bytes.NewReader(frame), 1024, 0)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if got.Type != msg.Type || got.From != msg.From || got.Timestamp != msg.Timestamp {
		t.Fatalf("unexpected decoded message: got %+v want %+v", got, msg)
	}
}

func TestFrameEncodeRejectsOversize(t *testing.T) {
	msg := types.Message{Type: types.MsgGetTip, Payload: types.GetTipPayload{}}
	_, err := encodeFrame(msg, 2)
	if err == nil {
		t.Fatalf("expected error for oversized message")
	}
}

func TestFrameDecodeRejectsInvalidLength(t *testing.T) {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, uint32(10))
	_, err := readFrame(bytes.NewReader(buf.Bytes()), 4, time.Second)
	if err == nil {
		t.Fatalf("expected invalid length error")
	}
}
