package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		assert.Equal("/api/v1/persons", r.URL.Path)
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
