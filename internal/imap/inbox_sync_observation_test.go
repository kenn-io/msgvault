package imap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInboxIMAPSyncPreservesFlagsPresence(t *testing.T) {
	for _, omit := range []bool{false, true} {
		name := "empty FLAGS"
		if omit {
			name = "omitted FLAGS"
		}
		t.Run(name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			client, _ := newKeywordTestClientFor(t, keywordTestSession{omitFlags: omit})
			raw, err := client.GetMessageRaw(t.Context(), "INBOX|1")
			requirements.NoError(err)
			requirements.NotNil(raw)
			observations := client.ObservedMemberships()
			requirements.NotEmpty(observations)
			flags := observations[len(observations)-1].Flags
			if omit {
				assertions.Nil(flags, "missing FLAGS cannot prove provider unread")
			} else {
				assertions.NotNil(flags, "FLAGS() proves unread")
				assertions.Empty(flags)
			}
		})
	}
}
