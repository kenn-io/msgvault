package threads

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThreadsOwnershipContradictionBothOrders(t *testing.T) {
	for _, first := range []bool{false, true} {
		for _, sparse := range []string{"omitted", "null"} {
			name := sparse
			if first {
				name += "_owner_first"
			}
			t.Run(name, func(t *testing.T) {
				w, st := writerFixture(t)
				media := writerReply("101", "100", "100")
				media.IsReplyOwnedByMe = &first
				id := applyWriter(t, w, media, ItemOrigin{Kind: "conversation", RootID: "100"})
				sender := writerSender(t, st, id)
				unknown := media
				unknown.IsReplyOwnedByMe = nil
				if sparse == "null" {
					unknown.Present = map[string]bool{"is_reply_owned_by_me": true}
				}
				applyWriter(t, w, unknown, ItemOrigin{Kind: "conversation", RootID: "100"})
				before := writerMetadata(t, st, id)
				media.IsReplyOwnedByMe = new(!first)
				_, err := w.Apply(context.Background(), media, ItemOrigin{Kind: "conversation", RootID: "100"})
				require.Error(t, err)
				assert.Equal(t, sender, writerSender(t, st, id))
				assert.Equal(t, before, writerMetadata(t, st, id))
			})
		}
	}
}
