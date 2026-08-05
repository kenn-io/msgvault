package identityops_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identityops"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestResolveSourceRequiresExactlyOneSelector(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("gmail", "shared@example.test")
	requirements.NoError(err)

	_, err = identityops.ResolveSource(st, identityops.SourceSelector{})
	requirements.ErrorContains(err, "account or source ID is required")
	_, err = identityops.ResolveSource(st, identityops.SourceSelector{SourceID: -1})
	requirements.ErrorContains(err, "source ID must be positive")
	_, err = identityops.ResolveSource(st, identityops.SourceSelector{
		Account: "shared@example.test", SourceID: 1,
	})
	requirements.ErrorContains(err, "mutually exclusive")
}

func TestResolveSourceIDDisambiguatesDuplicateIdentifiers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	providerSource, err := st.GetOrCreateSource("gmail", "shared@example.test")
	require.NoError(err)
	_, err = st.GetOrCreateSource("imap", "shared@example.test")
	require.NoError(err)

	_, err = identityops.ResolveSource(st, identityops.SourceSelector{Account: "shared@example.test"})
	require.ErrorContains(err, "ambiguous")
	got, err := identityops.ResolveSource(st, identityops.SourceSelector{SourceID: providerSource.ID})
	require.NoError(err)
	assert.Equal(providerSource.ID, got.ID)
}
