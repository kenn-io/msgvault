package wakeups

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/kit/atomicfile"
)

// EventKey encrypts subscription secrets, binding them to their archive and ID.
type EventKey [32]byte

func LoadEventKey(home string, create bool) (EventKey, error) {
	var key EventKey
	path := filepath.Join(home, "mcp-events.key")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && create {
		if _, err = rand.Read(key[:]); err != nil {
			return EventKey{}, err
		}
		if err = atomicfile.WriteNew(path, key[:], atomicfile.WithPrivate()); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return EventKey{}, fmt.Errorf("create events key: %w", err)
			}
			data, err = os.ReadFile(path)
		} else {
			return key, nil
		}
	}
	if err != nil {
		return EventKey{}, fmt.Errorf("read events key: %w", err)
	}
	if len(data) != len(key) {
		return EventKey{}, errors.New("invalid events key")
	}
	copy(key[:], data)
	return key, nil
}

func eventAAD(archive, id string) []byte {
	buf := make([]byte, 8, 8+len(archive)+len(id))
	binary.BigEndian.PutUint64(buf, uint64(len(archive)))
	buf = append(buf, archive...)
	return append(buf, id...)
}

func (k EventKey) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (k EventKey) Seal(archive, id string, secret []byte) ([]byte, error) {
	aead, err := k.aead()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, secret, eventAAD(archive, id)), nil
}

func (k EventKey) Open(archive, id string, sealed []byte) ([]byte, error) {
	aead, err := k.aead()
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.NonceSize() {
		return nil, errors.New("invalid sealed events secret")
	}
	return aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], eventAAD(archive, id))
}
