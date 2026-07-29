package slack

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLiveThreadsRepliesModifierPin freezes the observed behavior of the
// UNDOCUMENTED threads:replies search modifier against the REAL Slack API.
// The sweep's correctness and its ceiling math both assume the modifier
// returns thread replies exclusively; a silent change in Slack's modifier
// grammar must fail this test rather than degrade the sweep into noise.
//
// Skipped unless both env vars are set (dev workspace only):
//
//	MSGVAULT_SLACK_LIVE_TOKEN    user token with search:read
//	MSGVAULT_SLACK_LIVE_CHANNEL  channel ID containing at least one thread
//	                             (root + replies) and at least one plain
//	                             top-level message
func TestLiveThreadsRepliesModifierPin(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	token := os.Getenv("MSGVAULT_SLACK_LIVE_TOKEN")
	channel := os.Getenv("MSGVAULT_SLACK_LIVE_CHANNEL")
	if token == "" || channel == "" {
		t.Skip("set MSGVAULT_SLACK_LIVE_TOKEN and MSGVAULT_SLACK_LIVE_CHANNEL to run the modifier pin-test")
	}

	c := NewClient("", token)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	nonce := time.Now().UnixNano()

	// Modifier query: every hit must be a reply (permalink carries thread_ts).
	modQ := fmt.Sprintf(`threads:replies in:<#%s> -"zqpin%d"`, channel, nonce)
	mod, err := c.SearchMessagesPage(ctx, modQ, 1)
	require.NoError(err)
	require.NotEmpty(mod.Matches, "fixture channel must contain thread replies")
	for _, m := range mod.Matches {
		assert.NotEmpty(m.RootTS,
			"threads:replies returned a non-reply hit (ts %s) — the modifier's replies-only contract is broken", m.TS)
	}

	// Control query (no modifier): proves the channel also holds non-reply
	// messages, i.e. the modifier is doing the filtering — without this the
	// assertion above would pass vacuously on an all-replies channel.
	ctlQ := fmt.Sprintf(`in:<#%s> -"zqpinctl%d"`, channel, nonce)
	ctl, err := c.SearchMessagesPage(ctx, ctlQ, 1)
	require.NoError(err)
	control := false
	for _, m := range ctl.Matches {
		if m.RootTS == "" {
			control = true
			break
		}
	}
	assert.True(control,
		"control query returned only replies — fixture channel needs a plain top-level message for the pin-test to be meaningful")
}
