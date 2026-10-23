package peoplesweep

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestPacketForProfileDropsIneligibleTargets(t *testing.T) {
	packet := packetTestPacket()
	key := packet.Catalog.Targets[0].Key
	packet.Catalog.InferenceIneligible = []string{key}
	filtered, err := packetForProfile(packet, ProviderProfile{AllowSensitive: true})
	require.NoError(t, err)
	require.Len(t, filtered.Catalog.Targets, 1)
	assert.NotEqual(t, key, filtered.Catalog.Targets[0].Key)
	assert.Empty(t, filtered.CurrentProjection)
	assert.Empty(t, filtered.UnresolvedClaims)
	assert.Empty(t, filtered.Catalog.InferenceIneligible)
	fingerprint, err := personfacts.CatalogFingerprint(filtered.Catalog.Targets)
	require.NoError(t, err)
	assert.Equal(t, fingerprint, filtered.Catalog.Fingerprint)
	_, err = canonicalPacket(filtered)
	assert.NoError(t, err)
	assert.Len(t, packet.Catalog.Targets, 2, "filter must not mutate the full catalog")
	assert.Len(t, packet.CurrentProjection, 2)
	assert.Len(t, packet.UnresolvedClaims, 2)
}
