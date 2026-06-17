package a

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
)

func TestNeedsHelper(t *testing.T) {
	Require.NoError(t, nil)
	Require.NotNil(t, &struct{}{}) // want "test has 4 direct testify package calls; create a local require helper with require := require.New\\(t\\) and use it for repeated checks"
	Assert.Equal(t, 1, 1)
	Assert.True(t, true) // want "test has 4 direct testify package calls; create a local assert helper with assert := Assert.New\\(t\\) and use it for repeated checks"
}

func TestHasHelper(t *testing.T) {
	assert := Assert.New(t)
	Require.NoError(t, nil)
	Require.NotNil(t, &struct{}{})
	assert.Equal(1, 1)
	assert.True(true)
}

func TestSubtestNeedsHelper(t *testing.T) {
	t.Run("nested", func(t *testing.T) {
		Require.NoError(t, nil)
		Require.NotNil(t, &struct{}{}) // want "test has 4 direct testify package calls; create a local require helper with require := require.New\\(t\\) and use it for repeated checks"
		Assert.Equal(t, 1, 1)
		Assert.True(t, true) // want "test has 4 direct testify package calls; create a local assert helper with assert := Assert.New\\(t\\) and use it for repeated checks"
	})
}

func TestNeedsRequireHelper(t *testing.T) {
	Require.NoError(t, nil)
	Require.NotNil(t, &struct{}{})
	Require.NoError(t, nil)
	Require.NotNil(t, &struct{}{}) // want "test has 4 direct testify package calls; create a local require helper with require := require.New\\(t\\) and use it for repeated checks"
}

func TestHasRequireHelper(t *testing.T) {
	require := Require.New(t)
	require.NoError(nil)
	require.NotNil(&struct{}{})
	require.NoError(nil)
	require.NotNil(&struct{}{})
}

func TestUnusedRequireHelperStillFails(t *testing.T) {
	require := Require.New(t)
	_ = require
	Require.NoError(t, nil)
	Require.NotNil(t, &struct{}{})
	Require.NoError(t, nil)
	Require.NotNil(t, &struct{}{}) // want "test has 4 direct testify package calls; create a local require helper with require := require.New\\(t\\) and use it for repeated checks"
}

func TestAssertHelperDoesNotHideRequireDrift(t *testing.T) {
	assert := Assert.New(t)
	assert.True(true)
	Require.NoError(t, nil)
	Require.NotNil(t, &struct{}{})
	Require.NoError(t, nil)
	Require.NotNil(t, &struct{}{}) // want "test has 4 direct testify package calls; create a local require helper with require := require.New\\(t\\) and use it for repeated checks"
}

func TestRequireHelperDoesNotHideAssertDrift(t *testing.T) {
	require := Require.New(t)
	require.NoError(nil)
	Assert.Equal(t, 1, 1)
	Assert.True(t, true) // want "test has 4 direct testify package calls; create a local assert helper with assert := Assert.New\\(t\\) and use it for repeated checks"
	Require.NoError(t, nil)
	Require.NotNil(t, &struct{}{})
}

func TestMixedWithAssertHelperFlagsRequire(t *testing.T) {
	assert := Assert.New(t)
	assert.True(true)
	Assert.Equal(t, 1, 1)
	Require.NoError(t, nil)
	Require.NotNil(t, &struct{}{})
	Require.NoError(t, nil) // want "test has 4 direct testify package calls; create a local require helper with require := require.New\\(t\\) and use it for repeated checks"
}

func TestOuterHelperStillCountsAfterShadowing(t *testing.T) {
	assert := Assert.New(t)
	assert.Equal(1, 1)

	{
		assert := Assert.New(t)
		assert.True(true)
	}

	assert.Equal(2, 2)
	Require.NoError(t, nil)
	Require.NotNil(t, &struct{}{})
}
