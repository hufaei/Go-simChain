package crypto

import (
	"testing"

	"simchain-go/internal/types"
)

func TestLeadingZeroBits(t *testing.T) {
	var zero types.Hash
	if got := LeadingZeroBits(zero); got != 256 {
		t.Fatalf("expected 256, got %d", got)
	}

	var h types.Hash
	h[0] = 0x0F // 0000 1111
	if got := LeadingZeroBits(h); got != 4 {
		t.Fatalf("expected 4, got %d", got)
	}
}

func TestMeetsDifficulty(t *testing.T) {
	var h types.Hash
	h[0] = 0x00
	h[1] = 0x10 // 0001 0000, so leading zeros = 11
	if MeetsDifficulty(h, 8) != true {
		t.Fatalf("expected to meet difficulty 8")
	}
	if MeetsDifficulty(h, 12) != false {
		t.Fatalf("expected not to meet difficulty 12")
	}
}
