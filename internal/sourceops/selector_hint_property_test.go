//go:build (linux && !android && (amd64 || arm64)) || (darwin && !ios && arm64) || (windows && (amd64 || arm64))

package sourceops

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/opserr"
	"hegel.dev/go/hegel"
)

func TestSelectorHintPreservesDiagnosticProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		message := hegel.Draw(ht, hegel.Text())
		hint := hegel.Draw(ht, hegel.Text())
		cause := errors.New(message)
		err := withSelectorHint(cause, hint)
		assert.Equal(ht, message, err.Error())
		guidance, ok := errors.AsType[interface {
			error
			Hint() string
		}](err)
		if assert.True(ht, ok) {
			assert.Equal(ht, hint, guidance.Hint())
		}
	})
}

func TestSelectorHintPreservesCauseProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		message := hegel.Draw(ht, hegel.Text())
		hint := hegel.Draw(ht, hegel.Text())
		kind := hegel.Draw(ht, hegel.SampledFrom([]opserr.Kind{opserr.KindInvalid, opserr.KindNotFound, opserr.KindInternal}))
		cause := errors.New(message)
		typed := &opserr.Error{Kind: kind, Err: cause}
		err := fmt.Errorf("outer: %w", withSelectorHint(typed, hint))
		assert.ErrorIs(ht, err, cause)
		assert.Equal(ht, kind, opserr.KindOf(err))
		got, ok := errors.AsType[*opserr.Error](err)
		if assert.True(ht, ok) {
			assert.Same(ht, typed, got)
		}
	})
}

func TestSelectorHintNilIsNilProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		hint := hegel.Draw(ht, hegel.Text())
		assert.NoError(ht, withSelectorHint(nil, hint))
	})
}
