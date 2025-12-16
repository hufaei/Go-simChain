package crypto

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"math/big"

	"simchain-go/internal/types"
)

// SerializeHeader deterministically serializes a block header.
func SerializeHeader(h types.BlockHeader) ([]byte, error) {
	return json.Marshal(h)
}

// HashHeaderNonce returns SHA256(Serialize(header) || nonceLE).
func HashHeaderNonce(h types.BlockHeader, nonce uint64) (types.Hash, error) {
	buf, err := SerializeHeader(h)
	if err != nil {
		return types.Hash{}, err
	}
	var nonceBuf [8]byte
	binary.LittleEndian.PutUint64(nonceBuf[:], nonce)
	data := append(buf, nonceBuf[:]...)
	sum := sha256.Sum256(data)
	var out types.Hash
	copy(out[:], sum[:])
	return out, nil
}

// HashTransactions computes a simple root by hashing Tx IDs concatenated with '\n'.
func HashTransactions(txs []types.Transaction) types.Hash {
	h := sha256.New()
	for i, tx := range txs {
		if i > 0 {
			h.Write([]byte{'\n'})
		}
		h.Write([]byte(tx.ID))
	}
	sum := h.Sum(nil)
	var out types.Hash
	copy(out[:], sum)
	return out
}

// LeadingZeroBits counts leading zero bits in a 32-byte hash.
func LeadingZeroBits(h types.Hash) uint32 {
	var count uint32
	for _, b := range h {
		if b == 0 {
			count += 8
			continue
		}
		// Count leading zeros in this byte.
		for i := 7; i >= 0; i-- {
			if (b>>uint(i))&1 == 0 {
				count++
			} else {
				return count
			}
		}
	}
	return count
}

func MeetsDifficulty(h types.Hash, difficulty uint32) bool {
	return LeadingZeroBits(h) >= difficulty
}

// WorkForDifficulty approximates work as 2^difficulty.
func WorkForDifficulty(difficulty uint32) *big.Int {
	if difficulty == 0 {
		// Even with difficulty 0, treat each block as adding minimal work
		// so chain tip can progress.
		return big.NewInt(1)
	}
	return new(big.Int).Lsh(big.NewInt(1), uint(difficulty))
}
