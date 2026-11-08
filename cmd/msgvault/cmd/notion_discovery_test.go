package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/notionmeetings"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestConfiguredNotionDiscoveryUsesPrimaryIdentityAndRoots(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tmp := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmp)
	path := filepath.Join(tmp, "config.toml")
	require.NoError(os.WriteFile(path, []byte(`[[notion_meetings]]
identifier="synthetic"
account_email="owner@example.com"
token="primary-synthetic"
token_type="pat"
users_token="users-synthetic"
root_page_ids=["root"]
search_accessible=false
`), 0600))
	cfg, err := config.Load(path, "")
	require.NoError(err)
	var selfTokens []string
	var sections int
	meeting := notionmeetings.MeetingNote{Object: "block", ID: "meeting-1", Type: "meeting_notes", CreatedTime: "2026-11-07T10:00:00Z", CreatedBy: notionmeetings.UserRef{ID: "human-1"}, Parent: notionmeetings.Parent{Type: "data_source_id", DataSourceID: "rows"}, MeetingNotes: notionmeetings.MeetingNotesData{Status: "notes_ready", Children: notionmeetings.MeetingChildren{TranscriptBlockID: "transcript"}, CalendarEvent: notionmeetings.MeetingCalendarEvent{Attendees: []string{"other-user"}}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/users/me":
			selfTokens = append(selfTokens, r.Header.Get("Authorization"))
			_, _ = io.WriteString(w, `{"object":"user","id":"bot-1","type":"bot","bot":{"owner":{"type":"user","user":{"id":"human-1"}}}}`)
		case "/v1/blocks/meeting_notes/query":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"code":"restricted_resource"}`)
		case "/v1/blocks/root/children":
			_, _ = io.WriteString(w, `{"results":[{"object":"block","id":"nested","type":"toggle","has_children":true,"toggle":{}}],"has_more":false}`)
		case "/v1/blocks/nested/children":
			_, _ = io.WriteString(w, `{"results":[{"object":"block","id":"meeting-1","type":"meeting_notes","has_children":true,"meeting_notes":{}}],"has_more":false}`)
		case "/v1/blocks/meeting-1":
			assert.NoError(json.NewEncoder(w).Encode(meeting))
		case "/v1/blocks/transcript":
			sections++
			_, _ = io.WriteString(w, `{"object":"block","id":"transcript","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"Synthetic creator-only transcript."}]}}`)
		case "/v1/users/other-user":
			assert.Equal("Bearer users-synthetic", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"code":"restricted_resource"}`)
		default:
			assert.Fail("unexpected request", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	previousClient, previousUsers, previousRebuild := newNotionMeetingsClient, newNotionUsersClient, rebuildNotionMeetingsCacheAfterScheduledSync
	t.Cleanup(func() {
		newNotionMeetingsClient = previousClient
		newNotionUsersClient = previousUsers
		rebuildNotionMeetingsCacheAfterScheduledSync = previousRebuild
	})
	newNotionMeetingsClient = func(_ string, token string) notionmeetings.Source { return notionmeetings.NewClient(server.URL, token) }
	newNotionUsersClient = func(_ string, token string) notionmeetings.UserSource {
		return notionmeetings.NewClient(server.URL, token)
	}
	rebuildNotionMeetingsCacheAfterScheduledSync = func(context.Context, string) error { return nil }
	st := testutil.NewTestStore(t)
	_, err = st.GetOrCreateSource(notionmeetings.SourceType, "synthetic")
	require.NoError(err)
	require.NoError(runConfiguredNotionMeetingsSync(t.Context(), st, cfg.NotionMeetings[0]))
	assert.Equal([]string{"Bearer primary-synthetic"}, selfTokens)
	assert.Equal(1, sections)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages").Scan(&n))
	assert.Equal(1, n)
	require.NoError(runConfiguredNotionMeetingsSync(t.Context(), st, cfg.NotionMeetings[0]))
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages").Scan(&n))
	assert.Equal(1, n)
}
func TestNotionSummaryReportsCountOnlyScopeAndCoverage(t *testing.T) {
	assert := assert.New(t)

	var out bytes.Buffer
	writeNotionMeetingsSummary(&out, &notionmeetings.ImportSummary{IdentityProvenance: "pat_owner", ScopeCounts: map[string]int{"page_roots": 2, "data_source_roots": 1, "meeting_roots": 3, "accessible_search": 1}, CoverageReasons: map[string]int{"query_cap": 1, "search_best_effort": 1}, WorkRemaining: 4, PartialCoverage: true})
	assert.Contains(out.String(), "Identity: PAT owner")
	assert.Contains(out.String(), "page roots: 2")
	assert.Contains(out.String(), "Work remaining: 4")
	assert.Contains(out.String(), "Query limit: 1")
}

func TestNotionProbeDoesNotHydrateUnrelatedRefreshedMeeting(t *testing.T) {
	assert := assert.New(t)

	var sectionCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/users/me":
			_, _ = io.WriteString(w, `{"object":"user","id":"bot","type":"bot","bot":{"owner":{"type":"user","user":{"id":"human"}}}}`)
		case "/v1/blocks/meeting_notes/query":
			_, _ = io.WriteString(w, `{"results":[{"object":"block","id":"meeting","type":"meeting_notes","has_children":false,"meeting_notes":{}}],"has_more":true}`)
		case "/v1/blocks/meeting":
			_, _ = io.WriteString(w, `{"object":"block","id":"meeting","type":"meeting_notes","has_children":false,"created_by":{"id":"other"},"meeting_notes":{"children":{"transcript_block_id":"private-section"}}}`)
		case "/v1/users":
			_, _ = io.WriteString(w, `{"object":"list","results":[],"has_more":false}`)
		default:
			sectionCalls++
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	var out bytes.Buffer
	require.NoError(t, runNotionMeetingsProbe(t.Context(), &out, notionmeetings.NewClient(server.URL, "synthetic"), nil, notionmeetings.ImportOptions{TokenType: "pat"}))
	assert.Zero(sectionCalls)
	assert.Contains(out.String(), "Credentials: self verified")
	assert.Contains(out.String(), "Identity: PAT owner")
	assert.Contains(out.String(), "not selected for connected human")
	assert.NotContains(out.String(), "private-section")
}

func boolPointer(value bool) *bool { return &value }

func TestNotionProbeSkipsArchivedAndTrashedSections(t *testing.T) {
	for _, flag := range []string{"archived", "in_trash"} {
		t.Run(flag, func(t *testing.T) {
			assert := assert.New(t)

			sectionCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/users/me":
					_, _ = io.WriteString(w, `{"object":"user","id":"bot","type":"bot","bot":{"owner":{"type":"user","user":{"id":"human"}}}}`)
				case "/v1/blocks/meeting_notes/query":
					_, _ = io.WriteString(w, `{"results":[{"object":"block","id":"meeting","type":"meeting_notes","meeting_notes":{},"has_children":false}],"has_more":false}`)
				case "/v1/blocks/meeting":
					_, _ = io.WriteString(w, `{"object":"block","id":"meeting","type":"meeting_notes","has_children":false,"created_by":{"id":"human"},"`+flag+`":true,"meeting_notes":{"children":{"transcript_block_id":"private"}}}`)
				case "/v1/users":
					_, _ = io.WriteString(w, `{"results":[],"has_more":false}`)
				default:
					sectionCalls++
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			var out bytes.Buffer
			require.NoError(t, runNotionMeetingsProbe(t.Context(), &out, notionmeetings.NewClient(server.URL, "synthetic"), nil, notionmeetings.ImportOptions{TokenType: "pat"}))
			assert.Zero(sectionCalls)
			assert.Contains(out.String(), "archived or trash")
			assert.NotContains(out.String(), "private")
		})
	}
}
