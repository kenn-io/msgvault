// Package daemonauth authenticates a daemon endpoint without transmitting
// the API key or the private runtime secret.
package daemonauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
)

const (
	challengeBytes = 32
	proofDomain    = "msgvault-daemon-identity-v1"
)

// NewChallenge returns a fresh, fixed-size challenge suitable for the daemon
// identity protocol. Challenges are public and single-use.
func NewChallenge() (string, error) {
	challenge := make([]byte, challengeBytes)
	if _, err := rand.Read(challenge); err != nil {
		return "", fmt.Errorf("read daemon identity challenge: %w", err)
	}
	return hex.EncodeToString(challenge), nil
}

// Proof computes a response bound to the private runtime secret, challenge,
// and daemon PID. The secret itself is never sent to the identity endpoint.
func Proof(secret, challenge string, pid int) (string, error) {
	if secret == "" {
		return "", errors.New("empty daemon runtime secret")
	}
	if pid <= 0 {
		return "", errors.New("invalid daemon pid")
	}
	challengeBytes, err := decodeChallenge(challenge)
	if err != nil {
		return "", err
	}

	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(proofDomain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(strconv.Itoa(pid)))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(challengeBytes)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// VerifyProof reports whether proof authenticates the endpoint for the exact
// secret, challenge, and PID tuple.
func VerifyProof(secret, challenge string, pid int, proof string) bool {
	want, err := Proof(secret, challenge, pid)
	if err != nil {
		return false
	}
	wantBytes, err := hex.DecodeString(want)
	if err != nil {
		return false
	}
	gotBytes, err := hex.DecodeString(proof)
	if err != nil {
		return false
	}
	return hmac.Equal(wantBytes, gotBytes)
}

func decodeChallenge(challenge string) ([]byte, error) {
	decoded, err := hex.DecodeString(challenge)
	if err != nil || len(decoded) != challengeBytes {
		return nil, errors.New("invalid daemon identity challenge")
	}
	return decoded, nil
}
