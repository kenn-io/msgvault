package client

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMeetingRecordGeneratedNullableBounds(t *testing.T) {
	var record generated.MeetingRecord
	require.NoError(t, json.Unmarshal([]byte(`{"links":[{"ref":"evt:1","start":null,"end":null,"status":"cancelled","deleted_from_source":false}],"message_ids":[],"unexpandable":[]}`), &record))
	require.NoError(t, record.Validate())
}
