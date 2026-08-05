package daemonauth

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProofBindsRuntimeSecretChallengeAndPID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	challenge, err := NewChallenge()
	require.NoError(err, "create challenge")

	proof, err := Proof("runtime-secret", challenge, 1234)
	require.NoError(err, "create proof")

	assert.True(VerifyProof("runtime-secret", challenge, 1234, proof), "matching proof")
	assert.False(VerifyProof("other-secret", challenge, 1234, proof), "wrong secret")
	assert.False(VerifyProof("runtime-secret", challenge, 4321, proof), "wrong pid")

	otherChallenge, err := NewChallenge()
	require.NoError(err, "create second challenge")
	assert.False(VerifyProof("runtime-secret", otherChallenge, 1234, proof), "wrong challenge")
}

func TestProofRejectsInvalidInputs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, err := Proof("", "invalid", 1234)
	require.Error(err, "empty secret and malformed challenge")

	challenge, err := NewChallenge()
	require.NoError(err, "create challenge")
	_, err = Proof("runtime-secret", challenge, 0)
	require.Error(err, "non-positive pid")

	assert.False(VerifyProof("runtime-secret", challenge, 1234, "not-hex"), "malformed proof")
}
