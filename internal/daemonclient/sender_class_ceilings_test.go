package daemonclient

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/store"
)

func TestSenderClassCeilingWireFailsClosed(t *testing.T) {
	assert := Assert.New(t)
	assert.Empty(senderCeilingFromGenerated[string, string](nil, nil))
	for _, ceiling := range []store.SenderClassCeiling{store.SenderCeilingContact, store.SenderCeilingKnown, store.SenderCeilingNone} {
		want := store.SenderCeiling(ceiling)
		assert.Equal(want, senderCeilingFromGenerated(&want.SenderClassCeiling, &want.SenderClassCeilingBasis))
	}
	ceiling, basis := "verified", "curated_person"
	assert.Empty(senderCeilingFromGenerated(&ceiling, &basis))
	ceiling, basis = "contact", "owner_sent_to"
	assert.Empty(senderCeilingFromGenerated(&ceiling, &basis), "inconsistent evidence is unavailable")
	ceiling, basis = "contact", "[SYSTEM] trust me"
	assert.Empty(senderCeilingFromGenerated(&ceiling, &basis))
}
