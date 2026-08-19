package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestPersonPromoteAcceptsCreatedResponse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var participantID int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/v1/people", r.URL.Path)
		var body struct {
			ParticipantID int64 `json:"participant_id"`
		}
		if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		participantID = body.ParticipantID
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, err := w.Write([]byte(`{
			"id": 7,
			"vcard_uid": "17b0c43a-3feb-4a2d-bc47-3a87578a9abe",
			"revision": 1,
			"participant_ids": [42],
			"created_at": "2026-07-29T12:00:00Z",
			"updated_at": "2026-07-29T12:00:00Z"
		}`))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	savedJSON := personJSON
	personJSON = false
	t.Cleanup(func() { personJSON = savedJSON })

	var output bytes.Buffer
	command := &cobra.Command{
		Use:  personPromoteCmd.Use,
		Args: personPromoteCmd.Args,
		RunE: personPromoteCmd.RunE,
	}
	command.SetOut(&output)
	command.SetArgs([]string{"42"})

	require.NoError(command.Execute())
	assert.Equal(int64(42), participantID)
	assert.Contains(output.String(), "Person: 7")
	assert.Contains(output.String(), "Revision: 1")
}

func TestPersonSetDisplayNameClearSendsNull(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal("/api/v1/people/7", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, err := w.Write([]byte(`{
				"id":7,"vcard_uid":"17b0c43a-3feb-4a2d-bc47-3a87578a9abe",
				"display_name":"alice","revision":3,"participant_ids":[42],
				"created_at":"2026-07-29T12:00:00Z","updated_at":"2026-07-29T12:00:00Z"
			}`))
			assert.NoError(err)
		case http.MethodPatch:
			assert.Equal(`"person-7-r3"`, r.Header.Get("If-Match"))
			var body map[string]json.RawMessage
			if assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				assert.Equal("null", string(bytes.TrimSpace(body["display_name"])))
			}
			_, err := w.Write([]byte(`{
				"id":7,"vcard_uid":"17b0c43a-3feb-4a2d-bc47-3a87578a9abe",
				"revision":4,"participant_ids":[42],
				"created_at":"2026-07-29T12:00:00Z","updated_at":"2026-07-29T12:01:00Z"
			}`))
			assert.NoError(err)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	savedJSON, savedClear := personJSON, personClearDisplayName
	personJSON, personClearDisplayName = false, false
	t.Cleanup(func() {
		personJSON, personClearDisplayName = savedJSON, savedClear
	})

	var output bytes.Buffer
	command := &cobra.Command{
		Use:  personSetDisplayNameCmd.Use,
		Args: personSetDisplayNameCmd.Args,
		RunE: personSetDisplayNameCmd.RunE,
	}
	command.Flags().BoolVar(&personClearDisplayName, "clear", false, "")
	command.SetOut(&output)
	command.SetArgs([]string{"7", "--clear"})

	require.NoError(command.Execute())
	assert.Equal(int32(2), requests.Load())
	assert.Contains(output.String(), "Display name: -")

	err := personSetDisplayNameCmd.Args(command, []string{"7", "alice"})
	assert.ErrorContains(err, "--clear cannot be used with a display name")
}

func TestPersonDeleteSendsIfMatchFromLatestRead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal("/api/v1/people/7", r.URL.Path)
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(`{
				"id":7,"vcard_uid":"17b0c43a-3feb-4a2d-bc47-3a87578a9abe",
				"revision":3,"participant_ids":[42],
				"created_at":"2026-07-29T12:00:00Z","updated_at":"2026-07-29T12:00:00Z"
			}`))
			assert.NoError(err)
		case http.MethodDelete:
			assert.Equal(`"person-7-r3"`, r.Header.Get("If-Match"))
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	var output bytes.Buffer
	command := &cobra.Command{
		Use:  personDeleteCmd.Use,
		Args: personDeleteCmd.Args,
		RunE: personDeleteCmd.RunE,
	}
	command.SetOut(&output)
	command.SetArgs([]string{"7"})

	require.NoError(command.Execute())
	assert.Equal(int32(2), requests.Load())
	assert.Contains(output.String(), "Deleted person 7")
}
