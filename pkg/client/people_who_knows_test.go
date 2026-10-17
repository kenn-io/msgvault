package client_test

import (
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
	"testing"
)

func TestWhoKnowsGeneratedClientPreservesNulls(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	var evidence generated.WhoKnowsEvidence
	must.NoError(json.Unmarshal([]byte(`{"vector_score":null,"sent_at":null,"excerpt":null}`), &evidence))
	check.Nil(evidence.VectorScore)
	check.Nil(evidence.SentAt)
	check.Nil(evidence.Excerpt)
	var breakdown generated.WhoKnowsBreakdown
	must.NoError(json.Unmarshal([]byte(`{"profile_score":null,"latest_authored_at":null}`), &breakdown))
	check.Nil(breakdown.ProfileScore)
	check.Nil(breakdown.LatestAuthoredAt)
	var tie generated.WhoKnowsTieValue
	must.NoError(json.Unmarshal([]byte(`{"temperature":null,"last_contact_at":null}`), &tie))
	check.Nil(tie.Temperature)
	check.Nil(tie.LastContactAt)
	var excerpt generated.WhoKnowsExcerpt
	must.NoError(json.Unmarshal([]byte(`{"char_offset":null,"line":null}`), &excerpt))
	check.Nil(excerpt.CharOffset)
	check.Nil(excerpt.Line)
	var unsaved generated.WhoKnowsUnsavedBreakdown
	must.NoError(json.Unmarshal([]byte(`{"profile_score":null}`), &unsaved))
	check.Nil(unsaved.ProfileScore)
	var retrieval generated.WhoKnowsRetrieval
	must.NoError(json.Unmarshal([]byte(`{"top_vector_score":null}`), &retrieval))
	check.Nil(retrieval.TopVectorScore)
}
