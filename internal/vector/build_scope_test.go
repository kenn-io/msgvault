package vector

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewBuildScopeNormalizesSourceIDs(t *testing.T) {
	scope := NewBuildScope(nil, []int64{7, 3, 7, 0, -2, 3})
	assert.Equal(t, []int64{3, 7}, scope.SourceIDs,
		"source IDs are deduped, sorted ascending, and non-positive IDs dropped")
	assert.False(t, scope.IsEmpty(), "a sources-only scope is not empty")
}

func TestBuildScopeIsEmpty(t *testing.T) {
	assert := assert.New(t)
	assert.True(NewBuildScope(nil, nil).IsEmpty())
	assert.True(NewBuildScope([]string{""}, []int64{0}).IsEmpty(),
		"normalization dropping every entry leaves the scope empty")
	assert.False(NewBuildScope([]string{"email"}, nil).IsEmpty())
	assert.False(NewBuildScope(nil, []int64{1}).IsEmpty())
}

func TestBuildScopeFingerprint(t *testing.T) {
	assert := assert.New(t)
	assert.Empty(NewBuildScope(nil, nil).Fingerprint())
	assert.Equal("mt-email,sms", NewBuildScope([]string{"sms", "email"}, nil).Fingerprint())
	assert.Equal("src-3,7", NewBuildScope(nil, []int64{7, 3}).Fingerprint())
	assert.Equal("mt-email:src-3,7",
		NewBuildScope([]string{"email"}, []int64{7, 3}).Fingerprint(),
		"both dimensions appear, message types first, each normalized")
}

func TestBuildScopeContainsSource(t *testing.T) {
	assert := assert.New(t)
	scope := NewBuildScope(nil, []int64{3, 7})
	assert.True(scope.ContainsSource(3))
	assert.True(scope.ContainsSource(7))
	assert.False(scope.ContainsSource(4))
	assert.False(NewBuildScope(nil, nil).ContainsSource(3),
		"an unrestricted scope contains no explicit source")
}

func TestBuildScopeAllowsMessageTypesWithSourcesOnlyScope(t *testing.T) {
	scope := NewBuildScope(nil, []int64{3})
	assert.True(t, scope.AllowsMessageTypes([]string{"email"}),
		"a sources-only scope imposes no message-type restriction")
}
