package line

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegisterPreservesRemovedBotIdentity(t *testing.T) {
	must := require.New(t)
	f := newInboxFixture(t)
	ctx := t.Context()
	identities, err := f.st.ListAccountIdentitiesContext(ctx, f.source.ID)
	must.NoError(err)
	must.Len(identities, 1)
	_, err = f.st.RemoveAccountIdentityContext(ctx, f.source.ID, identities[0].Address)
	must.NoError(err)

	_, err = Register(ctx, f.st, f.client, testChannel)
	must.NoError(err)
	identities, err = f.st.ListAccountIdentitiesContext(ctx, f.source.ID)
	must.NoError(err)
	assert.Empty(t, identities, "registering again must preserve the removed bot identity")
}
