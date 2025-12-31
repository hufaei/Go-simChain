package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

const KeyFileName = "node_key.json"

type Identity struct {
	NodeID     string
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey
}

type keyFile struct {
	PrivateKey []byte `json:"privateKey"` // ed25519.PrivateKey (64 bytes)
	PublicKey  []byte `json:"publicKey"`  // ed25519.PublicKey (32 bytes)
}

func NodeIDFromPublicKey(pub ed25519.PublicKey) (string, error) {
	if len(pub) != ed25519.PublicKeySize {
		return "", errors.New("invalid public key length")
	}
	sum := sha256.Sum256(pub)
	// Keep it short for logs/paths but stable.
	return hex.EncodeToString(sum[:])[:40], nil
}

func LoadOrCreate(dir string) (*Identity, error) {
	if dir == "" {
		return nil, errors.New("empty dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, KeyFileName)
	if raw, err := os.ReadFile(path); err == nil {
		var kf keyFile
		if err := json.Unmarshal(raw, &kf); err != nil {
			return nil, err
		}
		if len(kf.PrivateKey) != ed25519.PrivateKeySize || len(kf.PublicKey) != ed25519.PublicKeySize {
			return nil, errors.New("invalid key file")
		}
		pub := ed25519.PublicKey(append([]byte(nil), kf.PublicKey...))
		priv := ed25519.PrivateKey(append([]byte(nil), kf.PrivateKey...))
		id, err := NodeIDFromPublicKey(pub)
		if err != nil {
			return nil, err
		}
		return &Identity{NodeID: id, PublicKey: pub, PrivateKey: priv}, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	id, err := NodeIDFromPublicKey(pub)
	if err != nil {
		return nil, err
	}
	kf := keyFile{PrivateKey: []byte(priv), PublicKey: []byte(pub)}
	raw, err := json.MarshalIndent(kf, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, raw, 0o600); err != nil {
		return nil, err
	}
	return &Identity{NodeID: id, PublicKey: pub, PrivateKey: priv}, nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, ".tmp-key")
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
