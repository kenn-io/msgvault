package opserr

import (
	"errors"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestKindOf(t *testing.T) {
	assert := assertpkg.New(t)
	err := Wrap(KindNotFound, errors.New("missing"))

	assert.Equal(KindNotFound, KindOf(err), "wrapped kind")
	assert.Equal(KindInternal, KindOf(errors.New("plain")), "plain errors default to internal")
}

func TestWrapPreservesCause(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	cause := errors.New("database unavailable")
	err := Wrap(KindInternal, cause)

	var wrapped *Error
	require.ErrorAs(err, &wrapped, "wrapped error")
	assert.Equal(KindInternal, wrapped.Kind, "kind")
	require.ErrorIs(err, cause, "cause")
	assert.Equal(cause.Error(), err.Error(), "message")
}

func TestConstructorsClassifyErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Kind
	}{
		{name: "invalid", err: Invalid(errors.New("bad input")), want: KindInvalid},
		{name: "not found", err: NotFound(errors.New("missing")), want: KindNotFound},
		{name: "internal", err: Internal(errors.New("database unavailable")), want: KindInternal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertpkg.Equal(t, tt.want, KindOf(tt.err), "kind")
		})
	}
}
