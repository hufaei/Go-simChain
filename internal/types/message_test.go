package types

import (
	"encoding/json"
	"testing"
)

func TestMessageJSONRoundTripDecodesConcretePayload(t *testing.T) {
	orig := Message{
		Type:      MsgInvTx,
		From:      "node-a",
		Timestamp: 123,
		Payload:   InvTxPayload{TxID: "tx-1"},
	}
	raw, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded Message
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Type != MsgInvTx {
		t.Fatalf("type mismatch: got %q", decoded.Type)
	}

	pl, ok := decoded.Payload.(InvTxPayload)
	if !ok {
		t.Fatalf("payload type mismatch: %T", decoded.Payload)
	}
	if pl.TxID != "tx-1" {
		t.Fatalf("txid mismatch: got %q", pl.TxID)
	}
}

func TestMessageJSONUnknownTypePreservesPayloadAsRaw(t *testing.T) {
	raw := []byte(`{"type":"UnknownType","from":"x","timestamp":1,"payload":{"a":1}}`)
	var decoded Message
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Type != MessageType("UnknownType") {
		t.Fatalf("type mismatch: %q", decoded.Type)
	}
	if _, ok := decoded.Payload.(json.RawMessage); !ok {
		t.Fatalf("expected RawMessage payload, got %T", decoded.Payload)
	}
}
