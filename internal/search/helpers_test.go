package search

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
)

// assertQueryEqual compares two Query structs, treating nil slices and empty
// slices as equivalent. This is appropriate because Query's slice fields
// (TextTerms, FromAddrs, ToAddrs, etc.) have no semantic difference between
// nil and empty - both mean "no filter". All code consuming Query uses len()
// checks which treat nil and empty identically.
func assertQueryEqual(t *testing.T, got, want Query) {
	t.Helper()
	diff := cmp.Diff(want, got, cmpopts.EquateEmpty())
	assert.Empty(t, diff, "Query mismatch (-want +got):\n%s", diff)
}
