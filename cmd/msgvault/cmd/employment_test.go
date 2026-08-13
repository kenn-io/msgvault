package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestEmploymentAddSendsPartialDatesAndOmitsUndecidedPrimary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var body map[string]any
	var decodeErr error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/v1/employments", r.URL.Path)
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		decodeErr = decoder.Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, err := w.Write([]byte(`{"id":9,"person_id":3,"organization_id":4,"title":"Staff Engineer","start_date":{"year":2019,"month":4},"is_current":true,"is_primary":true,"source":"user","revision":1,"created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"}`))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	output := runEmploymentCommand(t, employmentAddCmd, []string{"--person", "3", "--organization", "4", "--title", "Staff Engineer", "--start", "2019-04"})
	require.NoError(decodeErr)
	assert.Equal(json.Number("3"), body["person_id"])
	assert.Equal(json.Number("4"), body["organization_id"])
	assert.Equal("2019-04", body["start_date"])
	_, hasPrimary := body["is_primary"]
	assert.False(hasPrimary)
	assert.Contains(output, "Employment: 9")
	assert.Contains(output, "Title: Staff Engineer")
	assert.Contains(output, "Primary: true")
}

func TestEmploymentAddRejectsConflictingPrimaryFlags(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	command := cloneEmploymentCommand(employmentAddCmd)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"--person", "3", "--organization", "4", "--primary", "--no-primary"})
	err := command.Execute()
	require.Error(err)
	assert.Contains(err.Error(), "--primary and --no-primary are mutually exclusive")
}

func TestEmploymentEndReadsRevisionAndSendsEndDate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var ifMatch string
	var body map[string]any
	var decodeErr error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/employments/9":
			_, err := w.Write([]byte(`{"id":9,"person_id":3,"organization_id":4,"title":"Staff Engineer","is_current":true,"is_primary":true,"source":"user","revision":2,"created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"}`))
			assert.NoError(err)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/employments/9/end":
			ifMatch = r.Header.Get("If-Match")
			decodeErr = json.NewDecoder(r.Body).Decode(&body)
			_, err := w.Write([]byte(`{"id":9,"person_id":3,"organization_id":4,"title":"Staff Engineer","end_date":{"year":2026,"month":6},"is_current":false,"is_primary":false,"source":"user","revision":3,"created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"}`))
			assert.NoError(err)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	output := runEmploymentCommand(t, employmentEndCmd, []string{"9", "--end", "2026-06"})
	require.NoError(decodeErr)
	assert.Equal(`"employment-9-r2"`, ifMatch)
	assert.Equal("2026-06", body["end_date"])
	assert.Contains(output, "Current: false")
	assert.Contains(output, "Primary: false")
	assert.Contains(output, "End date: 2026-06")
}

func TestEmploymentSetPreservesFieldsNotPassedAsFlags(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var ifMatch string
	var body map[string]any
	var decodeErr error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, err := w.Write([]byte(`{"id":9,"person_id":3,"organization_id":4,"title":"Staff Engineer","role":"Engineering","department":"Archive Platform","address_id":6,"start_date":{"year":2019,"month":4},"is_current":true,"is_primary":true,"source":"extraction","source_ref":"message:abc","confidence":0.8,"revision":2,"created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"}`))
			assert.NoError(err)
		case http.MethodPatch:
			ifMatch = r.Header.Get("If-Match")
			decoder := json.NewDecoder(r.Body)
			decoder.UseNumber()
			decodeErr = decoder.Decode(&body)
			_, err := w.Write([]byte(`{"id":9,"person_id":3,"organization_id":4,"title":"Principal Engineer","role":"Engineering","department":"Archive Platform","start_date":{"year":2019,"month":4},"is_current":true,"is_primary":true,"source":"user","revision":3,"created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"}`))
			assert.NoError(err)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	output := runEmploymentCommand(t, employmentSetCmd, []string{"9", "--title", "Principal Engineer"})
	require.NoError(decodeErr)
	assert.Equal(`"employment-9-r2"`, ifMatch)
	assert.Equal("Principal Engineer", body["title"])
	assert.Equal(json.Number("3"), body["person_id"], "person id must come from the fetched record")
	assert.Equal(json.Number("4"), body["organization_id"], "organization id must come from the fetched record")
	assert.Equal("Engineering", body["role"], "unspecified fields must not be wiped")
	assert.Equal("Archive Platform", body["department"])
	assert.Equal(json.Number("6"), body["address_id"])
	assert.Equal("2019-04", body["start_date"])
	assert.Equal("extraction", body["source"])
	assert.Equal("message:abc", body["source_ref"])
	assert.Equal(json.Number("0.8"), body["confidence"])
	assert.Equal(true, body["is_current"])
	assert.Equal(true, body["is_primary"])
	assert.Contains(output, "Title: Principal Engineer")
}

func TestEmploymentSetEndingAPrimaryEmploymentDemotesIt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var body map[string]any
	var decodeErr error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, err := w.Write([]byte(`{"id":9,"person_id":3,"organization_id":4,"title":"Staff Engineer","start_date":{"year":2019,"month":4},"is_current":true,"is_primary":true,"source":"user","revision":2,"created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"}`))
			assert.NoError(err)
		case http.MethodPatch:
			decodeErr = json.NewDecoder(r.Body).Decode(&body)
			_, err := w.Write([]byte(`{"id":9,"person_id":3,"organization_id":4,"title":"Staff Engineer","start_date":{"year":2019,"month":4},"end_date":{"year":2026,"month":6},"is_current":false,"is_primary":false,"source":"user","revision":3,"created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"}`))
			assert.NoError(err)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	runEmploymentCommand(t, employmentSetCmd, []string{"9", "--end", "2026-06"})
	require.NoError(decodeErr)
	assert.Equal(false, body["is_current"])
	assert.Equal(false, body["is_primary"],
		"ending a primary employment must demote it, not send an invalid historical primary")
}

func TestEmploymentSetRejectsPrimaryCombinedWithNotCurrent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"id":9,"person_id":3,"organization_id":4,"title":"Staff Engineer","is_current":true,"is_primary":false,"source":"user","revision":2,"created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"}`))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	command := cloneEmploymentCommand(employmentSetCmd)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"9", "--primary", "--not-current"})
	err := command.Execute()
	require.Error(err)
	assert.Contains(err.Error(), "--primary cannot be combined with --not-current or --end")
}

func TestEmploymentListPrintsHistoryAndProjection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/persons/3/employments", r.URL.Path)
		assert.Contains(r.URL.RawQuery, "current_only=false")
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"employments":[{"id":9,"person_id":3,"organization_id":4,"title":"Staff Engineer","is_current":true,"is_primary":true,"source":"user","revision":1,"created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"},{"id":8,"person_id":3,"organization_id":5,"title":"Junior Engineer","start_date":{"year":2015},"end_date":{"year":2018,"month":6},"is_current":false,"is_primary":false,"source":"user","revision":1,"created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"}],"projection":{"employment_id":9,"organization_id":4,"organization_name":"Example Org","title":"Staff Engineer","role":"Engineering","department":"Archive Platform","vcard":{"org":["Example Org","Archive Platform"],"title":"Staff Engineer","role":"Engineering"}}}`))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	output := runEmploymentCommand(t, employmentListCmd, []string{"--person", "3"})
	assert.Contains(output, "Staff Engineer")
	assert.Contains(output, "Junior Engineer")
	assert.Contains(output, "2018-06")
	assert.Contains(output, "Current company: Example Org")
	assert.Contains(output, "Current title: Staff Engineer")
	assert.Contains(output, "vCard ORG: Example Org;Archive Platform")
	require.NotContains(output, "<nil>")
}

func TestEmploymentSetReactivatesEndedEmploymentWithClearEnd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var body map[string]any
	var decodeErr error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, err := w.Write([]byte(`{"id":9,"person_id":3,"organization_id":4,"title":"Staff Engineer","start_date":{"year":2019,"month":4},"end_date":{"year":2024,"month":6},"is_current":false,"is_primary":false,"source":"user","revision":2,"created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"}`))
			assert.NoError(err)
		case http.MethodPatch:
			decodeErr = json.NewDecoder(r.Body).Decode(&body)
			_, err := w.Write([]byte(`{"id":9,"person_id":3,"organization_id":4,"title":"Staff Engineer","start_date":{"year":2019,"month":4},"is_current":true,"is_primary":false,"source":"user","revision":3,"created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"}`))
			assert.NoError(err)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	runEmploymentCommand(t, employmentSetCmd, []string{"9", "--current", "--clear-end"})
	require.NoError(decodeErr)
	assert.Equal(true, body["is_current"],
		"an ended employment must be reactivatable")
	_, hasEnd := body["end_date"]
	assert.False(hasEnd, "--clear-end must drop the stored end date")
	assert.Equal("2019-04", body["start_date"], "unrelated dates stay preserved")
}

func TestEmploymentSetRejectsEmptyAndConflictingStateFlags(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"id":9,"person_id":3,"organization_id":4,"title":"Staff Engineer","end_date":{"year":2024,"month":6},"is_current":false,"is_primary":false,"source":"user","revision":2,"created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "empty start", args: []string{"9", "--start", ""},
			want: "use --clear-start"},
		{name: "empty end", args: []string{"9", "--end", ""},
			want: "use --clear-end"},
		{name: "current with not-current", args: []string{"9", "--current", "--not-current"},
			want: "--current and --not-current are mutually exclusive"},
		{name: "current with retained end date", args: []string{"9", "--current"},
			want: "combine it with --clear-end"},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := cloneEmploymentCommand(employmentSetCmd)
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&output)
			command.SetArgs(test.args)
			err := command.Execute()
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
		})
	}
}

func TestEmploymentListByOrganizationShowsPersonColumn(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var rawQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/organizations/4/employments", r.URL.Path)
		rawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"employments":[{"id":9,"person_id":3,"organization_id":4,"title":"Staff Engineer","is_current":true,"is_primary":true,"source":"user","revision":1,"created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"},{"id":8,"person_id":7,"organization_id":4,"title":"Advisor","is_current":true,"is_primary":false,"source":"user","revision":1,"created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"}]}`))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	output := runEmploymentCommand(t, employmentListCmd,
		[]string{"--organization", "4", "--limit", "25", "--offset", "50"})
	assert.Contains(rawQuery, "limit=25", "pagination must reach the server")
	assert.Contains(rawQuery, "offset=50")
	require.Contains(output, "PERSON",
		"an organization-scoped listing must identify employees, not repeat the organization")
	assert.NotContains(output, "ORGANIZATION")
	lines := strings.Split(strings.TrimSpace(output), "\n")
	require.Len(lines, 3)
	assert.Regexp(`^9\s+3\s+Staff Engineer`, lines[1])
	assert.Regexp(`^8\s+7\s+Advisor`, lines[2])
}

func TestEmploymentListRequiresPersonOrOrganization(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	command := cloneEmploymentCommand(employmentListCmd)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(nil)
	err := command.Execute()
	require.Error(err)
	assert.Contains(err.Error(), "--person or --organization is required")
}
func cloneEmploymentCommand(template *cobra.Command) *cobra.Command {
	command := &cobra.Command{Use: template.Use, Args: template.Args, RunE: template.RunE}
	command.Flags().AddFlagSet(template.Flags())
	command.Flags().VisitAll(func(flag *pflag.Flag) {
		_ = flag.Value.Set(flag.DefValue)
		flag.Changed = false
	})
	return command
}
func runEmploymentCommand(t *testing.T, template *cobra.Command, args []string) string {
	t.Helper()
	saved := employmentJSON
	employmentJSON = false
	t.Cleanup(func() { employmentJSON = saved })
	command := cloneEmploymentCommand(template)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	require.NoError(t, command.Execute())
	return output.String()
}
