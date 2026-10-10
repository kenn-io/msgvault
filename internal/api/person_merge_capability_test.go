package api

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// The facade retains real native owner methods without the scoped capability.
func TestDelegatedPersonMergeRequiresNativeCapability(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	promote := func(address, name string) *store.Person {
		participant, err := st.EnsureParticipant(address, name, "example.test")
		requirements.NoError(err)
		person, _, err := st.CreatePersonFromParticipant(participant)
		requirements.NoError(err)
		return person
	}
	survivor := promote("legacy-merge-survivor@example.test", "Legacy Survivor")
	absorbed := promote("legacy-merge-absorbed@example.test", "Legacy Absorbed")
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, unguardedPersonStore{MessageStore: st, PersonProfileStore: st, PersonProfileValueStore: st}, nil, testLogger())
	_, secret, _, err := srv.agentGrants.IssueScoped("Synthetic legacy merge", []agentgrant.Permission{agentgrant.PermissionPersonRead, agentgrant.PermissionPersonMerge}, agentgrant.ResourceScopes{Persons: []agentgrant.PersonRef{{ID: survivor.ID, UID: survivor.VCardUID}, {ID: absorbed.ID, UID: absorbed.VCardUID}}})
	requirements.NoError(err)
	discovery := identityNativeRequest(t, srv, http.MethodGet, "/api/v1/mcp/capabilities", secret, true, nil)
	requirements.Equal(http.StatusOK, discovery.Code, "%s", discovery.Body.String())
	var capabilities apiprotocol.MCPCapabilities
	requirements.NoError(json.Unmarshal(discovery.Body.Bytes(), &capabilities))
	for _, route := range capabilities.Routes {
		assertions.NotEqual("mergePersons", route.OperationID)
	}
	denied := delegatedMergeNativeRequest(t, srv, survivor, absorbed, secret, "synthetic-legacy-merge")
	assertions.Equal(http.StatusNotImplemented, denied.Code, "%s", denied.Body.String())
	for _, before := range []*store.Person{survivor, absorbed} {
		current, err := st.GetPerson(before.ID)
		requirements.NoError(err)
		assertions.Equal(before, current)
	}
	data, err := json.Marshal(MergePersonRequest{AbsorbedPersonID: absorbed.ID})
	requirements.NoError(err)
	owner := personMergeAPIRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/people/%d/merge", survivor.ID), data, map[string]string{"X-Api-Key": agentTokenTestAPIKey, "If-Match": personETag(*survivor) + ", " + personETag(*absorbed), "Idempotency-Key": "synthetic-legacy-owner-merge"})
	requirements.Equal(http.StatusOK, owner.Code, "%s", owner.Body.String())
	var receipt store.PersonMergeResult
	requirements.NoError(json.Unmarshal(owner.Body.Bytes(), &receipt))
	assertions.Equal(apiPersonMergeActor, receipt.Merge.Actor)
}
