//go:build windows && arm64

package duckdbdriver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenReportsUnsupportedPlatform(t *testing.T) {
	db, err := Open("")
	require.Error(t, err)
	assert.Nil(t, db)
	assert.ErrorIs(t, err, ErrUnsupportedPlatform)
}
