package managementbackup

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"

	"filippo.io/age"
	"github.com/btcsuite/btcd/btcutil/bech32"
)

// RecoveryIdentity derives the management archive key from the independently
// exported OpenBao seal key. The versioned domain must remain stable for recovery.
// age owns its identity through a pointer.
func RecoveryIdentity(sealKey string) (*age.X25519Identity, error) {
	root, err := base64.StdEncoding.DecodeString(sealKey)
	if err != nil || len(root) != 32 {
		return nil, errors.New("management backup recovery requires the original OpenBao auto-unseal key from the management keys file")
	}
	raw, err := hkdf.Key(sha256.New, root, nil, "borq/management-backups/age/v1", 32)
	if err != nil {
		return nil, errors.New("derive management backup recovery key")
	}
	data, err := bech32.ConvertBits(raw, 8, 5, true)
	if err != nil {
		return nil, errors.New("encode management backup recovery key")
	}
	encoded, err := bech32.Encode("age-secret-key-", data)
	if err != nil {
		return nil, errors.New("encode management backup recovery key")
	}
	return age.ParseX25519Identity(strings.ToUpper(encoded))
}
