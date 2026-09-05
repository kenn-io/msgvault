package api

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

const (
	operationTokenVersion    = "op2"
	operationTokenNonceBytes = 12
)

type operationTokenKeyring interface {
	ActiveOperationTokenKey(ctx context.Context) (store.OperationTokenKey, error)
	OperationTokenKey(ctx context.Context, keyID string) (store.OperationTokenKey, error)
}

type operationTokenCodec struct {
	keyring operationTokenKeyring
	random  io.Reader
}

func newOperationTokenCodec(keyring operationTokenKeyring) operationTokenCodec {
	return operationTokenCodec{keyring: keyring, random: rand.Reader}
}

func (codec operationTokenCodec) seal(ctx context.Context, archiveUID string, payload any) (string, error) {
	if err := validateOperationArchiveUID(archiveUID); err != nil {
		return "", err
	}
	if codec.keyring == nil {
		return "", errors.New("operation token keyring is unavailable")
	}
	key, err := codec.keyring.ActiveOperationTokenKey(ctx)
	if err != nil {
		return "", fmt.Errorf("read active operation token key: %w", err)
	}
	if err := validateOperationTokenKey(key, true); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode operation token: %w", err)
	}
	if len(encoded) == 0 || len(encoded) > maxOperationTokenPayloadBytes {
		return "", errors.New("encode operation token: payload is empty or too large")
	}
	aead, err := operationTokenAEAD(key.KeyBytes)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	randomSource := codec.random
	if randomSource == nil {
		randomSource = rand.Reader
	}
	if _, err := io.ReadFull(randomSource, nonce); err != nil {
		return "", fmt.Errorf("generate operation token nonce: %w", err)
	}
	sealed := aead.Seal(nonce, nonce, encoded, operationTokenAAD(archiveUID))
	return operationTokenVersion + "." + key.KeyID + "." +
		base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (codec operationTokenCodec) open(ctx context.Context, archiveUID, raw string) ([]byte, error) {
	if err := validateOperationArchiveUID(archiveUID); err != nil {
		return nil, err
	}
	if codec.keyring == nil {
		return nil, errors.New("operation token keyring is unavailable")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[0] != operationTokenVersion ||
		!validOperationTokenKeyID(parts[1]) || parts[2] == "" {
		return nil, errors.New("operation token has an invalid envelope")
	}
	maxSealedBytes := operationTokenNonceBytes + maxOperationTokenPayloadBytes + aes.BlockSize
	if len(parts[2]) > base64.RawURLEncoding.EncodedLen(maxSealedBytes) {
		return nil, errors.New("operation token payload is too large")
	}
	sealed, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || base64.RawURLEncoding.EncodeToString(sealed) != parts[2] {
		return nil, errors.New("operation token payload encoding is invalid")
	}
	key, err := codec.keyring.OperationTokenKey(ctx, parts[1])
	if err != nil {
		return nil, fmt.Errorf("read operation token key: %w", err)
	}
	if err := validateOperationTokenKey(key, false); err != nil {
		return nil, err
	}
	if key.KeyID != parts[1] {
		return nil, errors.New("operation token key lookup returned the wrong key")
	}
	aead, err := operationTokenAEAD(key.KeyBytes)
	if err != nil {
		return nil, err
	}
	if len(sealed) <= aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("operation token ciphertext is truncated")
	}
	nonce, ciphertext := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
	opened, err := aead.Open(nil, nonce, ciphertext, operationTokenAAD(archiveUID))
	if err != nil || len(opened) == 0 || len(opened) > maxOperationTokenPayloadBytes {
		return nil, errors.New("operation token authentication failed")
	}
	return opened, nil
}

func operationTokenAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create operation token cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create operation token AEAD: %w", err)
	}
	return aead, nil
}

func operationTokenAAD(archiveUID string) []byte {
	return []byte(operationTokenVersion + "\x00" + archiveUID)
}

func validateOperationTokenKey(key store.OperationTokenKey, requireActive bool) error {
	if !validOperationTokenKeyID(key.KeyID) || len(key.KeyBytes) != 32 {
		return errors.New("operation token key is invalid")
	}
	if requireActive {
		if key.State != store.OperationTokenKeyActive {
			return errors.New("operation token key is not active")
		}
		return nil
	}
	if key.State != store.OperationTokenKeyActive && key.State != store.OperationTokenKeyDecryptOnly {
		return errors.New("operation token key cannot decrypt tokens")
	}
	return nil
}

func validOperationTokenKeyID(value string) bool {
	if len(value) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}
