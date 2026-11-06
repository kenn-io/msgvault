package client

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestTimelineNullableFreshnessTimestamps(t *testing.T) {
	t.Run("projection frontier", func(t *testing.T) {
		projection := generated.TimelineProjectionFreshness{Name: "activity", State: "current"}
		require.NoError(t, projection.Validate())
	})
	t.Run("source last success", func(t *testing.T) {
		source := generated.SourceFreshness{SourceType: "gmail", Identifier: "owner@example.com", Scheduling: "manual", Outcome: "never", SyncCommand: "msgvault sync"}
		require.NoError(t, source.Validate())
	})
}
