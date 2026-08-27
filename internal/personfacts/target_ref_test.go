package personfacts

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonFactTargetRefCodecRoundTripsColonKeyAndSHA256Revision(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	revision := "sha256:" + strings.Repeat("a", 64)
	want := TargetRef{
		Kind: TargetEmployment, Key: "system:employment", Revision: revision,
	}

	encoded, err := EncodeTargetRef(want)
	requirements.NoError(err)
	assertions.Equal("employment:system:employment:"+revision, encoded)

	decoded, err := DecodeTargetRef(encoded)
	requirements.NoError(err)
	assertions.Equal(want, decoded)
}

func TestPersonFactTargetRefCodecRejectsMalformedKindKeyAndRevision(t *testing.T) {
	revision := "sha256:" + strings.Repeat("a", 64)
	for _, test := range []struct {
		name    string
		encoded string
		message string
	}{
		{name: "unknown kind", encoded: "candidate:system:employment:" + revision,
			message: "unknown person fact target kind"},
		{name: "empty key", encoded: "employment::" + revision,
			message: "target key must not be empty"},
		{name: "legacy revision", encoded: "employment:system:employment:revision-1",
			message: "target revision must be sha256"},
		{name: "short digest", encoded: "employment:system:employment:sha256:abc",
			message: "target revision must be sha256"},
		{name: "uppercase digest", encoded: "employment:system:employment:sha256:" + strings.Repeat("A", 64),
			message: "target revision must be sha256"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeTargetRef(test.encoded)
			require.ErrorContains(t, err, test.message)
		})
	}

	_, err := EncodeTargetRef(TargetRef{
		Kind: TargetEmployment, Key: "system:employment", Revision: "revision-1",
	})
	require.ErrorContains(t, err, "target revision must be sha256")
}
