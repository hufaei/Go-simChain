package types

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestMessageJSONRoundTrip(t *testing.T) {
	cases := []Message{
		{
			Type:      MsgHello,
			From:      "node-a",
			Timestamp: 1710000000000,
			Payload: HelloPayload{
				Magic:      "simchain-go",
				Version:    1,
				PubKey:     []byte{1, 2, 3},
				ListenAddr: "127.0.0.1:7000",
				Nonce:      []byte{4, 5},
				Timestamp:  1710000000001,
			},
		},
		{
			Type:      MsgInvTx,
			From:      "node-b",
			Timestamp: 1710000000002,
			Payload:   InvTxPayload{TxID: "tx-1"},
		},
		{
			Type:      MsgTip,
			From:      "node-c",
			Timestamp: 1710000000003,
			Payload:   TipPayload{TipHash: Hash{1, 2, 3}, TipHeight: 7, CumWork: "42"},
		},
	}

	for _, tc := range cases {
		raw, err := json.Marshal(&tc)
		if err != nil {
			t.Fatalf("marshal %s: %v", tc.Type, err)
		}
		var got Message
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal %s: %v", tc.Type, err)
		}
		if got.Type != tc.Type || got.From != tc.From || got.Timestamp != tc.Timestamp {
			t.Fatalf("unexpected header after round-trip: got %+v want %+v", got, tc)
		}
		if !reflect.DeepEqual(got.Payload, tc.Payload) {
			t.Fatalf("payload mismatch for %s: got %#v want %#v", tc.Type, got.Payload, tc.Payload)
		}
	}
}

func TestMessageJSONUnknownTypePreservesPayload(t *testing.T) {
	msg := Message{
		Type:      MessageType("Custom"),
		From:      "node-z",
		Timestamp: 1710000000004,
		Payload: map[string]any{
			"foo": "bar",
			"num": 1,
		},
	}

	raw, err := json.Marshal(&msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Message
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	payload, ok := got.Payload.(json.RawMessage)
	if !ok {
		t.Fatalf("expected json.RawMessage for unknown type, got %T", got.Payload)
	}

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode raw payload: %v", err)
	}
	if decoded["foo"] != "bar" {
		t.Fatalf("unexpected decoded payload: %#v", decoded)
	}
}
