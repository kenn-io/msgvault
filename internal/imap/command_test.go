package imap

import (
	"errors"
	"fmt"
	"testing"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
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
		{"limit fallback", imap.StatusResponseTypeNo, imap.ResponseCodeLimit, "Too many requests", time.Second, true},
		{"zero", imap.StatusResponseTypeNo, "", "retry in 0 seconds", 0, true},
		{"capped", imap.StatusResponseTypeNo, "", "retry in 999999999999999999999999999 seconds", time.Minute, true},
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

func FuzzCommandRetryDelay(f *testing.F) {
	for _, seed := range []string{"retry in 4 seconds", "retry in 0 seconds", "retry in -1 seconds", "retry in 99999999999999999999 seconds", "retry in 1.5 seconds", "arbitrary text"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		delay, _ := commandRetryDelay(&imap.Error{Type: imap.StatusResponseTypeNo, Text: text})
		assert.GreaterOrEqual(t, delay, time.Duration(0))
		assert.LessOrEqual(t, delay, time.Minute)
		_, retry := commandRetryDelay(&imap.Error{Type: imap.StatusResponseTypeBad, Code: imap.ResponseCodeLimit, Text: text})
		assert.False(t, retry)
	})
}
