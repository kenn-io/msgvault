package imap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommandRetryDelay(t *testing.T) {
	for _, tt := range []struct {
		name  string
		typ   imap.StatusResponseType
		code  imap.ResponseCode
		text  string
		want  time.Duration
		retry bool
	}{
		{"hint", imap.StatusResponseTypeNo, imap.ResponseCodeLimit, "Too many requests, retry in 4 seconds", 4 * time.Second, true},
		{"hint without code", imap.StatusResponseTypeNo, "", "Retry IN 3 SECOND", 3 * time.Second, true},
		{"lowercase limit without hint", imap.StatusResponseTypeNo, "limit", "Too many requests", time.Second, true},
		{"limit fallback", imap.StatusResponseTypeNo, imap.ResponseCodeLimit, "Too many requests", time.Second, true},
		{"zero", imap.StatusResponseTypeNo, "", "retry in 0 seconds", 0, true},
		{"largest hint", imap.StatusResponseTypeNo, "", "retry in 9223372036 seconds", 9223372036 * time.Second, true},
		{"duration overflow", imap.StatusResponseTypeNo, imap.ResponseCodeLimit, "retry in 9223372037 seconds", 0, false},
		{"integer overflow", imap.StatusResponseTypeNo, imap.ResponseCodeLimit, "retry in 999999999999999999999999999 seconds", 0, false},
		{"ordinary no", imap.StatusResponseTypeNo, "", "permission denied", 0, false},
		{"bad", imap.StatusResponseTypeBad, imap.ResponseCodeLimit, "retry in 1 seconds", 0, false},
		{"bye", imap.StatusResponseTypeBye, imap.ResponseCodeLimit, "retry in 1 seconds", 0, false},
		{"negative", imap.StatusResponseTypeNo, "", "retry in -3 seconds", 0, false},
		{"fraction", imap.StatusResponseTypeNo, "", "retry in 1.5 seconds", 0, false},
		{"wrong unit", imap.StatusResponseTypeNo, "", "retry in 3 minutes", 0, false},
		{"invalid limit hint", imap.StatusResponseTypeNo, imap.ResponseCodeLimit, "retry in -1 seconds", time.Second, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := fmt.Errorf("command: %w", &imap.Error{Type: tt.typ, Code: tt.code, Text: tt.text})
			delay, retry := commandRetryDelay(err)
			assert.Equal(t, tt.want, delay)
			assert.Equal(t, tt.retry, retry)
		})
	}
	_, retry := commandRetryDelay(errors.New("imap: NO [LIMIT] retry in 1 seconds"))
	assert.False(t, retry, "untyped text cannot authorize a retry")
	_, retry = commandRetryDelay(nil)
	assert.False(t, retry)
}

func TestRunCommandLongDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		client := &Client{logger: slog.Default()}
		attempts := 0
		start := time.Now()
		result, err := runCommand(t.Context(), client, func() (string, error) {
			attempts++
			if attempts == 1 {
				return "partial", &imap.Error{Type: imap.StatusResponseTypeNo, Text: "retry in 1000 seconds"}
			}
			return "complete", nil
		})
		require.NoError(err)
		assert.Equal("complete", result)
		assert.Equal(2, attempts)
		assert.Equal(1000*time.Second, time.Since(start))
	})
}

func TestRunCommandErrorIdentity(t *testing.T) {
	for _, tt := range []struct {
		name, text string
		attempts   int
	}{
		{"permanent", "permission denied", 1},
		{"exhausted", "retry in 0 seconds", 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			commandErr := &imap.Error{Type: imap.StatusResponseTypeNo, Text: tt.text}
			attempts := 0
			result, err := runCommand(t.Context(), &Client{logger: slog.Default()}, func() (string, error) {
				attempts++
				return "partial", commandErr
			})
			assert.Same(commandErr, err)
			assert.Empty(result)
			assert.Equal(tt.attempts, attempts)
		})
	}
}

func TestRunCommandCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		timer := time.AfterFunc(100*time.Second, func() { cancel(errors.New("stop sync")) })
		defer timer.Stop()
		attempts := 0
		result, err := runCommand(ctx, &Client{logger: slog.Default()}, func() (string, error) {
			attempts++
			return "partial", &imap.Error{Type: imap.StatusResponseTypeNo, Text: "retry in 1000 seconds"}
		})
		assert.ErrorIs(err, context.Canceled)
		assert.Empty(result)
		assert.Equal(1, attempts)
	})
}

func FuzzCommandRetryDelay(f *testing.F) {
	for _, seed := range []string{"retry in 4 seconds", "retry in 0 seconds", "retry in -1 seconds", "retry in 99999999999999999999 seconds", "retry in 1.5 seconds", "arbitrary text"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		delay, _ := commandRetryDelay(&imap.Error{Type: imap.StatusResponseTypeNo, Text: text})
		assert.GreaterOrEqual(t, delay, time.Duration(0))
	})
}
