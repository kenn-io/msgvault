package opserr

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKindOf(t *testing.T) {
	assert := assert.New(t)
	err := Wrap(KindNotFound, errors.New("missing"))

	assert.Equal(KindNotFound, KindOf(err), "wrapped kind")
	assert.Equal(KindInternal, KindOf(errors.New("plain")), "plain errors default to internal")
}

func TestWrapPreservesCause(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cause := errors.New("database unavailable")
	err := Wrap(KindInternal, cause)

	var wrapped *Error
	require.ErrorAs(err, &wrapped, "wrapped error")
	assert.Equal(KindInternal, wrapped.Kind, "kind")
	require.ErrorIs(err, cause, "cause")
	assert.Equal(cause.Error(), err.Error(), "message")
}
