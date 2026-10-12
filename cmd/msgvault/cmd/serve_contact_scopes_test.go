package cmd

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDaemonAdapterContactScopeNativeConsumers(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	promote := func(address string) *store.Person {
		id, err := st.EnsureParticipant(address, "Synthetic Contact", "example.test")
		requirements.NoError(err)
		person, _, err := st.CreatePersonFromParticipant(id)
		requirements.NoError(err)
		return person
	}
	person, other := promote("contact@example.test"), promote("other@example.test")
	const ownerKey = "synthetic-contact-owner-key"
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: ownerKey, AgentAccess: true}},
		Store:  &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler),
		OperationGate: api.NewSerialOperationGate(),
	})
	request := func(method, path, token, etag string, body any) *httptest.ResponseRecorder {
		data, err := json.Marshal(body)
		requirements.NoError(err)
		r := httptest.NewRequest(method, path, bytes.NewReader(data))
		r.Header.Set("Content-Type", "application/json")
		if token == ownerKey {
			r.Header.Set("Authorization", "Bearer "+token)
		} else {
			r.Header.Set("X-Msgvault-Agent-Token", token)
		}
		r.Header.Set("If-Match", etag)
		r.Header.Set("Idempotency-Key", "synthetic-contact-merge")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w
	}
	issued := request(http.MethodPost, "/api/v1/agent-tokens", ownerKey, "", map[string]any{
		"label": "Synthetic Contacts", "permissions": []string{"person.read", "person.edit", "person.merge"},
		"person_ids": []int64{person.ID, other.ID},
	})
	requirements.Equal(http.StatusCreated, issued.Code, "%s", issued.Body.String())
	var grant struct {
		Secret string `json:"secret"`
	}
	requirements.NoError(json.Unmarshal(issued.Body.Bytes(), &grant))
	path := fmt.Sprintf("/api/v1/people/%d", person.ID)
	read := request(http.MethodGet, path, grant.Secret, "", nil)
	requirements.Equal(http.StatusOK, read.Code, "%s", read.Body.String())
	rename := request(http.MethodPatch, path, grant.Secret, read.Header().Get("ETag"), map[string]any{"display_name": "Changed Synthetic Contact"})
	requirements.Equal(http.StatusOK, rename.Code, "%s", rename.Body.String())
	structured := request(http.MethodPatch, path+"/profile", grant.Secret, rename.Header().Get("ETag"), map[string]any{"names": map[string]any{"add": []any{map[string]any{"name_kind": "formatted", "formatted": "Synthetic Structured Name", "envelope": map[string]any{"source": "user"}}}}})
	requirements.Equal(http.StatusOK, structured.Code, "%s", structured.Body.String())
	attributes := request(http.MethodGet, path+"/attributes", grant.Secret, "", nil)
	requirements.Equal(http.StatusOK, attributes.Code, "%s", attributes.Body.String())
	valuePath := path + "/attributes/primary_channel"
	set := request(http.MethodPut, valuePath, grant.Secret, attributes.Header().Get("ETag"), map[string]any{"value": map[string]any{"type": "text", "text": "email"}, "expected_value_id": 0})
	requirements.Equal(http.StatusOK, set.Code, "%s", set.Body.String())
	var value store.PersonAttributeWrite
	requirements.NoError(json.Unmarshal(set.Body.Bytes(), &value))
	requirements.NotNil(value.Value)
	attributes = request(http.MethodGet, path+"/attributes", grant.Secret, "", nil)
	cleared := request(http.MethodDelete, fmt.Sprintf("%s?expected_value_id=%d", valuePath, value.Value.ID), grant.Secret, attributes.Header().Get("ETag"), nil)
	requirements.Equal(http.StatusOK, cleared.Code, "%s", cleared.Body.String())
	read = request(http.MethodGet, path, grant.Secret, "", nil)
	otherRead := request(http.MethodGet, fmt.Sprintf("/api/v1/people/%d", other.ID), grant.Secret, "", nil)
	merge := request(http.MethodPost, path+"/merge", grant.Secret, read.Header().Get("ETag")+", "+otherRead.Header().Get("ETag"), map[string]any{"absorbed_person_id": other.ID})
	requirements.Equal(http.StatusOK, merge.Code, "%s", merge.Body.String())
	current, err := st.GetPerson(person.ID)
	requirements.NoError(err)
	assertions.Equal(new("Changed Synthetic Contact"), current.DisplayName)
	_, err = st.GetPerson(other.ID)
	assertions.ErrorIs(err, store.ErrPersonNotFound)
}
