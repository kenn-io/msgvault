package peoplesweep

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestPacketForProfileDropsIneligibleTargets(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	packet := packetTestPacket()
	key := packet.Catalog.Targets[0].Key
	packet.Catalog.InferenceIneligible = []string{key}
	filtered, err := packetForProfile(packet, ProviderProfile{AllowSensitive: true})
	requirements.NoError(err)
	requirements.Len(filtered.Catalog.Targets, 1)
	assertions.NotEqual(key, filtered.Catalog.Targets[0].Key)
	assertions.Empty(filtered.CurrentProjection)
	assertions.Empty(filtered.UnresolvedClaims)
	assertions.Empty(filtered.Catalog.InferenceIneligible)
	fingerprint, err := personfacts.CatalogFingerprint(filtered.Catalog.Targets)
	requirements.NoError(err)
	assertions.Equal(fingerprint, filtered.Catalog.Fingerprint)
	_, err = canonicalPacket(filtered)
	requirements.NoError(err)
	assertions.Len(packet.Catalog.Targets, 2, "filter must not mutate the full catalog")
	assertions.Len(packet.CurrentProjection, 2)
	assertions.Len(packet.UnresolvedClaims, 2)
}
