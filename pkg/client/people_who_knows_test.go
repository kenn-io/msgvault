package client_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestWhoKnowsGeneratedClientValidatesNullableAndEmptyValues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, time.October, 18, 0, 0, 0, 0, time.UTC)
	generation := generated.WhoKnowsGeneration{
		Fingerprint: "generation-fingerprint",
		Model:       "example-model",
		State:       "ready",
	}
	evidence := generated.WhoKnowsEvidence{
		Excerpt:         &generated.WhoKnowsExcerpt{Score: 0.8, Snippet: ""},
		Flags:           []string{"subjectless"},
		MessageType:     "email",
		Role:            "from",
		Snippet:         "",
		SourceMessageID: "", // Archived messages may lack a provider ID.
		Subject:         "",
	}
	breakdown := generated.WhoKnowsBreakdown{
		AskMeAboutMatches:   []string{"cloud"},
		ChannelsAuthored:    []string{"email"},
		OrganizationMatches: []string{"example"},
		ProfileSearch:       "cloud",
	}
	response := generated.WhoKnowsResponse{
		Coverage: generated.WhoKnowsCoverage{
			ContactState: generated.WhoKnowsContactCoverage{},
			Generation:   generation,
			Messages:     generated.WhoKnowsMessageCoverage{}, // Coverage can be unknown when its generation does not match.
			Scope: generated.WhoKnowsScope{
				Accounts:     []int64{1},
				MessageTypes: []string{"email"},
			},
			StaleReasons: []string{"partial"},
		},
		GeneratedAt:   now,
		LimitsApplied: []string{"evidence_cap"},
		Params: generated.WhoKnowsParams{
			AutomatedPatterns:    []string{"noreply@"},
			ExcludedMessageTypes: []string{"automated"},
			Tiebreak:             "score",
		},
		People: []generated.WhoKnowsRanked{{
			Breakdown:   breakdown,
			DisplayName: "",
			Evidence:    []generated.WhoKnowsEvidence{evidence},
			Flags:       []string{"declared"},
			Organizations: []generated.WhoKnowsOrganization{{
				Name:  "Example",
				Title: "",
			}},
			Tiebreak: generated.WhoKnowsTieValue{},
		}},
		Retrieval: generated.WhoKnowsRetrieval{
			Accelerator: "", // PostgreSQL retrieval does not report a SQLite accelerator.
			Generation:  generation,
			Mode:        "lexical",
		},
		Tiebreak: generated.WhoKnowsTiebreak{Applied: "score", Requested: "score"},
		Topic:    "cloud",
		UnsavedCandidates: []generated.WhoKnowsUnsavedCandidate{{
			Breakdown: generated.WhoKnowsUnsavedBreakdown{
				AskMeAboutMatches:   []string{"cloud"},
				ChannelsAuthored:    []string{"email"},
				OrganizationMatches: []string{"example"},
				ProfileSearch:       "cloud",
			},
			DisplayName:   "",
			EmailAddress:  "",
			Evidence:      []generated.WhoKnowsEvidence{evidence},
			Flags:         []string{"declared_only"},
			ParticipantID: 42,
			PromoteHint:   "review",
		}},
	}

	require.NoError(response.Validate())

	data, err := json.Marshal(response)
	require.NoError(err)
	var encoded map[string]any
	require.NoError(json.Unmarshal(data, &encoded))
	params, ok := encoded["params"].(map[string]any)
	require.True(ok)
	coverage, ok := encoded["coverage"].(map[string]any)
	require.True(ok)
	messages, ok := coverage["messages"].(map[string]any)
	require.True(ok)
	contactState, ok := coverage["contact_state"].(map[string]any)
	require.True(ok)
	people, ok := encoded["people"].([]any)
	require.True(ok)
	person, ok := people[0].(map[string]any)
	require.True(ok)
	assert.Contains(person, "display_name")
	assert.Empty(person["display_name"])
	organizations, ok := person["organizations"].([]any)
	require.True(ok)
	organization, ok := organizations[0].(map[string]any)
	require.True(ok)
	assert.Contains(organization, "title")
	assert.Empty(organization["title"])
	personBreakdown, ok := person["breakdown"].(map[string]any)
	require.True(ok)
	personTiebreak, ok := person["tiebreak"].(map[string]any)
	require.True(ok)
	personEvidence, ok := person["evidence"].([]any)
	require.True(ok)
	evidenceFields, ok := personEvidence[0].(map[string]any)
	require.True(ok)
	excerptFields, ok := evidenceFields["excerpt"].(map[string]any)
	require.True(ok)
	unsavedCandidates, ok := encoded["unsaved_candidates"].([]any)
	require.True(ok)
	unsavedCandidate, ok := unsavedCandidates[0].(map[string]any)
	require.True(ok)
	assert.Contains(unsavedCandidate, "display_name")
	assert.Empty(unsavedCandidate["display_name"])
	assert.Contains(unsavedCandidate, "email_address")
	assert.Empty(unsavedCandidate["email_address"])
	unsavedBreakdown, ok := unsavedCandidate["breakdown"].(map[string]any)
	require.True(ok)
	retrieval, ok := encoded["retrieval"].(map[string]any)
	require.True(ok)
	assert.Contains(messages, "coverage_sampled_at")
	assert.Equal(time.Time{}.Format(time.RFC3339Nano), messages["coverage_sampled_at"])
	assert.Contains(retrieval, "accelerator")
	assert.Empty(retrieval["accelerator"])
	assert.Contains(params, "after")
	assert.Nil(params["after"])
	assert.Contains(params, "before")
	assert.Nil(params["before"])
	assert.Contains(contactState, "oldest_computed_at")
	assert.Nil(contactState["oldest_computed_at"])
	assert.Contains(evidenceFields, "sent_at")
	assert.Nil(evidenceFields["sent_at"])
	assert.Contains(evidenceFields, "source_message_id")
	assert.Empty(evidenceFields["source_message_id"])
	assert.Contains(evidenceFields, "vector_score")
	assert.Nil(evidenceFields["vector_score"])
	assert.Contains(evidenceFields, "subject")
	assert.Empty(evidenceFields["subject"])
	assert.Contains(evidenceFields, "snippet")
	assert.Empty(evidenceFields["snippet"])
	assert.Contains(excerptFields, "char_offset")
	assert.Nil(excerptFields["char_offset"])
	assert.Contains(excerptFields, "line")
	assert.Nil(excerptFields["line"])
	assert.Contains(excerptFields, "snippet")
	assert.Empty(excerptFields["snippet"])
	assert.Contains(personBreakdown, "latest_authored_at")
	assert.Nil(personBreakdown["latest_authored_at"])
	assert.Contains(personBreakdown, "profile_score")
	assert.Nil(personBreakdown["profile_score"])
	assert.Contains(personTiebreak, "last_contact_at")
	assert.Nil(personTiebreak["last_contact_at"])
	assert.Contains(personTiebreak, "temperature")
	assert.Nil(personTiebreak["temperature"])
	assert.Contains(unsavedBreakdown, "latest_authored_at")
	assert.Nil(unsavedBreakdown["latest_authored_at"])
	assert.Contains(unsavedBreakdown, "profile_score")
	assert.Nil(unsavedBreakdown["profile_score"])
	assert.Contains(retrieval, "top_vector_score")
	assert.Nil(retrieval["top_vector_score"])
}
