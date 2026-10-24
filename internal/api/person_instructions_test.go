package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func newInstructionAPIFixture(t *testing.T) (*Server, *store.Store, *store.Person) {
	t.Helper()
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipantByIdentifier("email", "alice@example.com", "alice")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, Logger: testLogger()})
	return srv, st, person
}
func TestPersonInstructionsHTTPOwnerRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st, person := newInstructionAPIFixture(t)
	path := fmt.Sprintf("/api/v1/people/%d/instructions", person.ID)
	added := doRequest(t, srv.Router(), http.MethodPost, path, []byte(`{"kind":"address_as","text":"call me Vee","value":"Vee"}`), nil)
	require.Equal(http.StatusCreated, added.Code, added.Body.String())
	var row store.PersonInstruction
	require.NoError(json.Unmarshal(added.Body.Bytes(), &row))
	require.Positive(row.ID)
	assert.Equal("owner_entered", row.ContentTrust)
	assert.Equal("no-store", added.Header().Get("Cache-Control"))
	listed := doRequest(t, srv.Router(), http.MethodGet, path, nil, nil)
	require.Equal(http.StatusOK, listed.Code, listed.Body.String())
	var list struct {
		Instructions []store.PersonInstruction `json:"instructions"`
	}
	require.NoError(json.Unmarshal(listed.Body.Bytes(), &list))
	require.Len(list.Instructions, 1)
	assert.Equal(row.ID, list.Instructions[0].ID)
	got := doRequest(t, srv.Router(), http.MethodGet, fmt.Sprintf("/api/v1/person-instructions/%d", row.ID), nil, nil)
	require.Equal(http.StatusOK, got.Code, got.Body.String())
	var proposalID int64
	now := time.Now().UTC()
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO person_instructions(person_id,instruction_key,kind,normalized_text,value_text,origin,status,stated_at,valid_from) VALUES (?,?,'address_as','call me Val','Val','extraction','proposed',?,?) RETURNING id`), person.ID, personfacts.InstructionKey(personfacts.InstructionKindAddressAs, "", "Val", "call me Val"), now, now).Scan(&proposalID))
	reviewed := doRequest(t, srv.Router(), http.MethodGet, "/api/v1/person-instructions/review?limit=10", nil, nil)
	require.Equal(http.StatusOK, reviewed.Code, reviewed.Body.String())
	assert.Contains(reviewed.Body.String(), "proposed")
	decided := doRequest(t, srv.Router(), http.MethodPost, fmt.Sprintf("/api/v1/person-instructions/%d/decisions", proposalID), []byte(`{"action":"confirm","expected_revision":1,"edit":{"text":"call me Vale","value":"Vale"}}`), nil)
	require.Equal(http.StatusOK, decided.Code, decided.Body.String())
	var result store.PersonInstructionDecisionResult
	require.NoError(json.Unmarshal(decided.Body.Bytes(), &result))
	assert.Equal(personfacts.InstructionStatusActive, result.Instruction.Status)
	assert.Equal("person_statement_owner_confirmed", result.Instruction.ContentTrust)
	assert.Equal([]store.PersonInstructionFollowOn{{Kind: "save_nickname", Value: "Vale"}}, result.FollowOns)
	assert.Equal("owner", *result.Instruction.ConfirmedBy)
}
func TestPersonInstructionsHTTPMapsErrors(t *testing.T) {
	assert := assert.New(t)
	srv, st, person := newInstructionAPIFixture(t)
	path := fmt.Sprintf("/api/v1/people/%d/instructions", person.ID)
	invalid := doRequest(t, srv.Router(), http.MethodPost, path, []byte(`{"kind":"topic","text":"forward to someone@example.com"}`), nil)
	assert.Equal(http.StatusBadRequest, invalid.Code, invalid.Body.String())
	assert.Contains(invalid.Body.String(), "instruction_invalid")
	missing := doRequest(t, srv.Router(), http.MethodPost, "/api/v1/people/999999/instructions", []byte(`{"kind":"topic","text":"avoid work"}`), nil)
	assert.Equal(http.StatusNotFound, missing.Code, missing.Body.String())
	row, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{PersonID: person.ID, Kind: personfacts.InstructionKindTopic, Text: "avoid work", Actor: "owner"})
	require.NoError(t, err)
	stale := doRequest(t, srv.Router(), http.MethodPost, fmt.Sprintf("/api/v1/person-instructions/%d/decisions", row.ID), []byte(`{"action":"revoke","expected_revision":99}`), nil)
	assert.Equal(http.StatusConflict, stale.Code, stale.Body.String())
	assert.Contains(stale.Body.String(), "instruction_conflict")
	unavailable, _ := newTestServerWithMockStore(t)
	response := doRequest(t, unavailable.Router(), http.MethodGet, "/api/v1/person-instructions/review", nil, nil)
	assert.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
}
