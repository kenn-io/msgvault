package personfacts

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostalAddressTargetRequiresSensitiveCatalog(t *testing.T) {
	local, err := BuildCatalog(nil, CatalogOptions{})
	require := require.New(t)
	require.NoError(err)
	assert := assert.New(t)
	for _, target := range local.Targets {
		assert.NotEqual(TargetPostalAddress, target.Kind)
	}
	allowed, err := BuildCatalog(nil, CatalogOptions{IncludeSensitive: true})
	require.NoError(err)
	target := postalTarget(t, allowed)
	assert.Equal("system:postal_address", target.Key)
	assert.True(target.Sensitive)
	assert.Equal(ValuePostalAddress, target.ValueType)
	ref := TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision}
	encoded, err := EncodeTargetRef(ref)
	require.NoError(err)
	decoded, err := DecodeTargetRef(encoded)
	require.NoError(err)
	assert.Equal(ref, decoded)
}

func postalTarget(t *testing.T, catalog Catalog) TargetDescriptor {
	t.Helper()
	for _, target := range catalog.Targets {
		if target.Kind == TargetPostalAddress {
			return target
		}
	}
	require.FailNow(t, "missing postal target")
	return TargetDescriptor{}
}

func TestPostalAddressNormalizationRetainsSourceText(t *testing.T) {
	catalog, err := BuildCatalog(nil, CatalogOptions{IncludeSensitive: true})
	require := require.New(t)
	require.NoError(err)
	target := postalTarget(t, catalog)
	submitted := json.RawMessage(`{"street_address":"  東京都千代田区\n丸の内１−２  ","extended_address":" ３０１号室 ","country_code":" jp ","source_text":"  raw\n source  "}`)
	normalized, failure, err := NormalizeClaimValue(target, submitted)
	require.NoError(err)
	require.Nil(failure)
	require.NotNil(normalized)
	var got PostalAddressValue
	require.NoError(json.Unmarshal(normalized.JSON, &got))
	assert := assert.New(t)
	assert.Equal("東京都千代田区\n丸の内１−２", got.StreetAddress)
	assert.Equal("３０１号室", got.ExtendedAddress)
	assert.Equal("JP", got.CountryCode)
	assert.Equal("  raw\n source  ", got.SourceText)
	for _, raw := range []string{`{"locality":"Example City","source_text":"city only"}`, `{"street_address":"12 Example St"}`, `{"street_address":"12 Example St","source_text":"raw","validated":true}`} {
		_, failure, err := NormalizeClaimValue(target, json.RawMessage(raw))
		require.NoError(err)
		require.NotNil(failure)
		assert.Equal(ReasonMalformedValue, failure.Reason)
	}
}
