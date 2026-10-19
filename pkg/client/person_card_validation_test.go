package client_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestPersonCardGeneratedValidationAllowsPresentEmptyStrings(t *testing.T) {
	requirements := require.New(t)
	complete := []byte(`{"content_trust":"source_controlled","emails":[],"inferred_channel":"","person_id":7,"primary_channel":"","tracked":true,"untrusted_text":{"categories":[],"display_name":"","organization_name":"","title":""},"vcard_uid":""}`)
	var person generated.PersonCardPerson
	requirements.NoError(json.Unmarshal(complete, &person))
	requirements.NoError(person.Validate())

	missing := []byte(`{"content_trust":"source_controlled","emails":[],"inferred_channel":"","person_id":7,"tracked":true,"untrusted_text":{"categories":[],"display_name":"","organization_name":"","title":""},"vcard_uid":""}`)
	var withoutPrimaryChannel generated.PersonCardPerson
	requirements.NoError(json.Unmarshal(missing, &withoutPrimaryChannel))
	requirements.ErrorContains(withoutPrimaryChannel.Validate(), "PrimaryChannel is required")
}

func TestMeetingGeneratedValidationPreservesSparseRequiredFields(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	itemPayload := `{"basis":"","content_omitted":false,"deleted_from_source":false,"follow_up":"reviewed","message_id":1,"origin":"","recordings":[],"sent_at":"0001-01-01T00:00:00Z","source_id":2,"title":""}`
	var item generated.MeetingItem
	requirements.NoError(json.Unmarshal([]byte(itemPayload), &item))
	requirements.NotNil(item.Title)
	requirements.Empty(*item.Title)
	requirements.NotNil(item.SentAt)
	requirements.True(item.SentAt.IsZero())
	requirements.NoError(item.Validate())
	itemWire, err := json.Marshal(item)
	requirements.NoError(err)
	assertions.JSONEq(itemPayload, string(itemWire))

	missingItemTitle := strings.Replace(itemPayload, `,"title":""`, "", 1)
	var missingTitle generated.MeetingItem
	requirements.NoError(json.Unmarshal([]byte(missingItemTitle), &missingTitle))
	assertions.ErrorContains(missingTitle.Validate(), "Title is required")
	missingItemTime := strings.Replace(itemPayload, `,"sent_at":"0001-01-01T00:00:00Z"`, "", 1)
	var missingSentAt generated.MeetingItem
	requirements.NoError(json.Unmarshal([]byte(missingItemTime), &missingSentAt))
	assertions.ErrorContains(missingSentAt.Validate(), "SentAt is required")

	summaryPayload := `{"has_transcript":true,"item_count":1,"ref":"evt:1","start":"2026-10-18T00:00:00Z","title":""}`
	var summary generated.MeetingRecordSummary
	requirements.NoError(json.Unmarshal([]byte(summaryPayload), &summary))
	requirements.NotNil(summary.Title)
	requirements.Empty(*summary.Title)
	requirements.NoError(summary.Validate())
	summaryWire, err := json.Marshal(summary)
	requirements.NoError(err)
	assertions.JSONEq(summaryPayload, string(summaryWire))

	missingSummaryTitle := strings.Replace(summaryPayload, `,"title":""`, "", 1)
	var missingSummary generated.MeetingRecordSummary
	requirements.NoError(json.Unmarshal([]byte(missingSummaryTitle), &missingSummary))
	assertions.ErrorContains(missingSummary.Validate(), "Title is required")
}

func TestPersonCardGeneratedProjectionAcceptsExtraValues(t *testing.T) {
	var projection generated.ProjectionFreshness
	err := json.Unmarshal([]byte(`{"computed_at":null,"age_seconds":null,"stale":false,"extra":{"queue_depth":2,"rescan_reason":"identity"}}`), &projection)
	require.NoError(t, err)
	wire, err := json.Marshal(projection)
	require.NoError(t, err)
	assert.JSONEq(t, `{"computed_at":null,"age_seconds":null,"stale":false,"extra":{"queue_depth":2,"rescan_reason":"identity"}}`, string(wire))
}

func TestPersonCardGeneratedPendingActivityRoundTripsPresentEmptyValues(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	payload := `{"at":null,"cadence_due_at":null,"cadence_overdue_at_as_of":false,"cadence_status":"unknown","channel":"","direction":"unknown","interaction_count":0,"last_inbound_at":null,"last_outbound_at":null,"projection_computed_at":"0001-01-01T00:00:00Z","projection_stale":true,"ref":"","web_url":""}`
	var interaction generated.PersonCardLastInteraction
	requirements.NoError(json.Unmarshal([]byte(payload), &interaction))
	requirements.NoError(interaction.Validate())
	wire, err := json.Marshal(interaction)
	requirements.NoError(err)
	assertions.JSONEq(payload, string(wire))

	for _, tc := range []struct {
		field, missing string
	}{
		{field: "Channel", missing: `"channel":"",`},
		{field: "Ref", missing: `"ref":"",`},
		{field: "ProjectionComputedAt", missing: `"projection_computed_at":"0001-01-01T00:00:00Z",`},
	} {
		t.Run(tc.field+" required", func(t *testing.T) {
			missingField := strings.Replace(payload, tc.missing, "", 1)
			var withoutField generated.PersonCardLastInteraction
			require.NoError(t, json.Unmarshal([]byte(missingField), &withoutField))
			assert.ErrorContains(t, withoutField.Validate(), tc.field+" is required")
		})
	}
}
