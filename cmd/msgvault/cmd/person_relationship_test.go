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

const testPersonRelationshipJSON = `{
	"id": 11, "source_person_id": 3, "target_person_id": 4,
	"relationship_type_id": 9, "type_slug": "parent",
	"forward_label": "parent", "reverse_label": "child",
	"is_symmetric": false, "status": "active", "source": "user",
	"created_by": "user", "updated_by": "user", "revision": 1,
	"created_at": "2026-07-30T12:00:00Z", "updated_at": "2026-07-30T12:00:00Z"
}`

const testRelationshipTypeJSON = `{
	"id": 9, "universal_id": "17b0c43a-3feb-4a2d-bc47-3a87578a9abe",
	"slug": "mentor", "forward_label": "mentor", "reverse_label": "mentee",
	"is_symmetric": false, "is_canonical": true, "is_deletable": true,
	"ownership": "user", "revision": 4,
	"created_at": "2026-07-30T12:00:00Z", "updated_at": "2026-07-30T12:00:00Z"
}`

func TestPersonRelationshipAddPostsTheDeclaredEdge(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var body struct {
		SourcePersonID       int64   `json:"source_person_id"`
		TargetPersonID       int64   `json:"target_person_id"`
		RelationshipTypeSlug string  `json:"relationship_type_slug"`
		StartDate            *string `json:"start_date"`
		Notes                *string `json:"notes"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/v1/person-relationships", r.URL.Path)
		if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, err := w.Write([]byte(testPersonRelationshipJSON))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	savedJSON, savedFrom := personJSON, relationshipStartDate
	personJSON, relationshipStartDate = false, "1994"
	t.Cleanup(func() { personJSON, relationshipStartDate = savedJSON, savedFrom })

	var output bytes.Buffer
	command := &cobra.Command{
		Use: personRelationshipAddCmd.Use, Args: personRelationshipAddCmd.Args, RunE: personRelationshipAddCmd.RunE,
	}
	command.SetOut(&output)
	command.SetArgs([]string{"3", "parent", "4"})

	require.NoError(command.Execute())
	assert.Equal(int64(3), body.SourcePersonID)
	assert.Equal(int64(4), body.TargetPersonID)
	assert.Equal("parent", body.RelationshipTypeSlug)
	require.NotNil(body.StartDate)
	assert.Equal("1994", *body.StartDate)
	assert.Contains(output.String(), "Relationship: 11")
	assert.Contains(output.String(), "parent")
}

func TestPersonRelationshipListRendersBothDirections(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodGet, r.Method)
		assert.Equal("/api/v1/persons/3/relationships", r.URL.Path)
		assert.Equal("true", r.URL.Query().Get("include_ended"))
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"relationships":[
			{
				"relationship": {"id": 11, "source_person_id": 3, "target_person_id": 4,
					"relationship_type_id": 9, "type_slug": "parent", "forward_label": "parent",
					"reverse_label": "child", "is_symmetric": false, "status": "active", "source": "user",
					"created_by": "user", "updated_by": "user", "revision": 1,
					"created_at": "2026-07-30T12:00:00Z", "updated_at": "2026-07-30T12:00:00Z"},
				"direction": "outgoing", "counterpart_person_id": 4, "counterpart_label": "child",
				"counterpart_display_name": "bob", "counterpart_vcard_uid": "17b0c43a-3feb-4a2d-bc47-3a87578a9abe"},
			{
				"relationship": {"id": 12, "source_person_id": 5, "target_person_id": 3,
					"relationship_type_id": 9, "type_slug": "parent", "forward_label": "parent",
					"reverse_label": "child", "is_symmetric": false, "status": "ended", "end_date": {"year": 2001},
					"source": "user", "created_by": "user", "updated_by": "user", "revision": 2,
					"created_at": "2026-07-30T12:00:00Z", "updated_at": "2026-07-30T12:00:00Z"},
				"direction": "incoming", "counterpart_person_id": 5, "counterpart_label": "parent",
				"counterpart_vcard_uid": "2f8a1b3c-1111-4111-8111-111111111111"}
		]}`))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	savedJSON, savedIncludeEnded := personJSON, relationshipIncludeEnded
	personJSON, relationshipIncludeEnded = false, true
	t.Cleanup(func() { personJSON, relationshipIncludeEnded = savedJSON, savedIncludeEnded })

	var output bytes.Buffer
	command := &cobra.Command{
		Use: personRelationshipListCmd.Use, Args: personRelationshipListCmd.Args, RunE: personRelationshipListCmd.RunE,
	}
	command.SetOut(&output)
	command.SetArgs([]string{"3"})

	require.NoError(command.Execute())
	rendered := output.String()
	assert.Contains(rendered, "4 (bob)")
	assert.Contains(rendered, "child")
	assert.Contains(rendered, "5 (2f8a1b3c-1111-4111-8111-111111111111)")
	assert.Contains(rendered, "parent")
	assert.Contains(rendered, "2001")
}

func TestPersonRelationshipEndSendsIfMatchFromTheCurrentRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var sentIfMatch string
	var sentBody struct {
		EndDate *string `json:"end_date"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			assert.Equal("/api/v1/person-relationships/11", r.URL.Path)
			_, err := w.Write([]byte(testPersonRelationshipJSON))
			assert.NoError(err)
		case http.MethodPatch:
			sentIfMatch = r.Header.Get("If-Match")
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&sentBody)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, err := w.Write([]byte(`{
				"id": 11, "source_person_id": 3, "target_person_id": 4,
				"relationship_type_id": 9, "type_slug": "spouse", "forward_label": "spouse",
				"reverse_label": "spouse", "is_symmetric": true, "status": "ended",
				"end_date": {"year": 2023, "month": 5}, "source": "user", "created_by": "user",
				"updated_by": "user", "revision": 5, "created_at": "2026-07-30T12:00:00Z",
				"updated_at": "2026-07-30T12:00:00Z"}`))
			assert.NoError(err)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
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
		Use: personRelationshipEndCmd.Use, Args: personRelationshipEndCmd.Args, RunE: personRelationshipEndCmd.RunE,
	}
	command.SetOut(&output)
	command.SetArgs([]string{"11", "2023-05"})

	require.NoError(command.Execute())
	assert.Equal(`"person-relationship-11-r1"`, sentIfMatch)
	require.NotNil(sentBody.EndDate)
	assert.Equal("2023-05", *sentBody.EndDate)
	assert.Contains(output.String(), "ended")
}

func TestRelationshipTypeUpdateUsesCurrentRevisionETag(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var sentIfMatch string
	var sentBody struct {
		ForwardLabel *string `json:"forward_label"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			assert.Equal("/api/v1/relationship-types/9", r.URL.Path)
			_, err := w.Write([]byte(testRelationshipTypeJSON))
			assert.NoError(err)
		case http.MethodPatch:
			sentIfMatch = r.Header.Get("If-Match")
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&sentBody)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, err := w.Write([]byte(`{
				"id": 9, "universal_id": "17b0c43a-3feb-4a2d-bc47-3a87578a9abe",
				"slug": "mentor", "forward_label": "guide", "reverse_label": "mentee",
				"is_symmetric": false, "is_canonical": true, "is_deletable": true,
				"ownership": "user", "revision": 5,
				"created_at": "2026-07-30T12:00:00Z", "updated_at": "2026-07-30T12:00:00Z"}`))
			assert.NoError(err)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	savedJSON, savedForward := personJSON, relationshipTypeUpdateForwardLabel
	forwardFlag := relationshipTypeUpdateCmd.Flags().Lookup("forward-label")
	savedChanged := forwardFlag.Changed
	personJSON = false
	t.Cleanup(func() {
		personJSON, relationshipTypeUpdateForwardLabel = savedJSON, savedForward
		forwardFlag.Changed = savedChanged
	})

	var output bytes.Buffer
	command := &cobra.Command{
		Use: relationshipTypeUpdateCmd.Use, Args: relationshipTypeUpdateCmd.Args, RunE: relationshipTypeUpdateCmd.RunE,
	}
	command.Flags().AddFlagSet(relationshipTypeUpdateCmd.Flags())
	command.SetOut(&output)
	command.SetArgs([]string{"9", "--forward-label", "guide"})

	require.NoError(command.Execute())
	assert.Equal(`"relationship-type-9-r4"`, sentIfMatch)
	require.NotNil(sentBody.ForwardLabel)
	assert.Equal("guide", *sentBody.ForwardLabel)
	assert.Contains(output.String(), "Forward: guide")
}

func TestRelationshipTypeCreateRejectsAnEmptyReverseLabel(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	command := &cobra.Command{
		Use: relationshipTypeCreateCmd.Use, Args: relationshipTypeCreateCmd.Args, RunE: relationshipTypeCreateCmd.RunE,
	}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"mentor", "mentor", "  "})

	require.Error(command.Execute())
	assert.False(called, "an invalid argument must not reach the daemon")
}
