package vcard

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vcard/registry"
)

func TestRegistryHandlingIsCompleteAndExact(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	snapshot, err := registry.Load()
	require.NoError(err)
	require.NoError(ValidateRegistryCoverage(snapshot))

	assert.Len(propertyHandling, 52)
	assert.Len(parameterHandling, 27)
}

func TestRegistryCoverageRejectsMissingAndStaleEntries(t *testing.T) {
	require := require.New(t)
	snapshot, err := registry.Load()
	require.NoError(err)

	properties := maps.Clone(propertyHandling)
	parameters := maps.Clone(parameterHandling)
	delete(properties, "FN")
	delete(parameters, "PREF")
	properties["X-STALE"] = Handling{Strategy: HandlingPreserve}
	parameters["X-STALE"] = Handling{Strategy: HandlingPreserve}

	err = validateRegistryCoverage(snapshot, properties, parameters)
	require.Error(err)
	require.ErrorContains(err, "missing parameter PREF")
	require.ErrorContains(err, "missing property FN")
	require.ErrorContains(err, "stale parameter X-STALE")
	require.ErrorContains(err, "stale property X-STALE")
}

func TestRegistryCoverageRejectsInvalidStrategy(t *testing.T) {
	require := require.New(t)
	snapshot, err := registry.Load()
	require.NoError(err)

	properties := maps.Clone(propertyHandling)
	parameters := maps.Clone(parameterHandling)
	properties["FN"] = Handling{}
	parameters["PREF"] = Handling{Strategy: HandlingStrategy("future")}

	err = validateRegistryCoverage(snapshot, properties, parameters)
	require.Error(err)
	require.ErrorContains(err, "property FN has invalid handling strategy")
	require.ErrorContains(err, "parameter PREF has invalid handling strategy")
}

func TestRegistryHandlingLookupIsCaseInsensitive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	property, ok := PropertyHandling("socialprofile")
	require.True(ok)
	assert.Equal(HandlingNative, property.Strategy)

	parameter, ok := ParameterHandling("service-type")
	require.True(ok)
	assert.Equal(HandlingPreserve, parameter.Strategy)

	_, ok = PropertyHandling("X-FUTURE")
	assert.False(ok)
	_, ok = ParameterHandling("X-FUTURE")
	assert.False(ok)
}
