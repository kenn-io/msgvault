package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestOrganizationCreateSendsNormalizedBodyAndPrintsResult(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var received struct {
		Name          string  `json:"name"`
		Kind          string  `json:"kind"`
		PrimaryDomain *string `json:"primary_domain"`
	}
	var decodeErr error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/v1/organizations", r.URL.Path)
		decodeErr = json.NewDecoder(r.Body).Decode(&received)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"organization-4-r1"`)
		w.WriteHeader(http.StatusCreated)
		_, err := w.Write([]byte(`{"id":4,"name":"Example Org","kind":"company","primary_domain":"example.com","revision":1,"created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"}`))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	output := runOrganizationCommand(t, organizationCreateCmd, []string{"Example Org", "--kind", "company", "--domain", "Example.com"})
	require.NoError(decodeErr)
	assert.Equal("Example Org", received.Name)
	assert.Equal("company", received.Kind)
	require.NotNil(received.PrimaryDomain)
	assert.Equal("Example.com", *received.PrimaryDomain)
	assert.Contains(output, "Organization: 4")
	assert.Contains(output, "Name: Example Org")
	assert.Contains(output, "Primary domain: example.com")
	assert.Contains(output, "Revision: 1")
}

func TestOrganizationSetReadsCurrentRevisionAndSendsIfMatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requests atomic.Int32
	var ifMatch string
	var body map[string]any
	var decodeErr error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal("/api/v1/organizations/4", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, err := w.Write([]byte(`{"organization":{"id":4,"name":"Example Org","kind":"company","primary_domain":"example.com","description":"Synthetic description.","revision":3,"retired_at":"2026-07-29T12:00:00Z","created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"}}`))
			assert.NoError(err)
		case http.MethodPatch:
			ifMatch = r.Header.Get("If-Match")
			decodeErr = json.NewDecoder(r.Body).Decode(&body)
			_, err := w.Write([]byte(`{"id":4,"name":"Example Group","kind":"company","primary_domain":"example.com","description":"Synthetic description.","revision":4,"retired_at":"2026-07-29T12:00:00Z","created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"}`))
			assert.NoError(err)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	output := runOrganizationCommand(t, organizationSetCmd, []string{"4", "--name", "Example Group"})
	require.NoError(decodeErr)
	assert.Equal(int32(2), requests.Load())
	assert.Equal(`"organization-4-r3"`, ifMatch)
	assert.Equal("example.com", body["primary_domain"])
	assert.Equal("Synthetic description.", body["description"])
	assert.Equal(true, body["retired"])
	assert.Contains(output, "Name: Example Group")
	assert.Contains(output, "Revision: 4")
	require.NotEmpty(output)
}

func TestOrganizationLifecycleCommandsPreserveRootFields(t *testing.T) {
	for _, test := range []struct {
		name      string
		command   *cobra.Command
		retiredAt string
		want      bool
	}{
		{name: "retire", command: organizationRetireCmd, want: true},
		{
			name: "unretire", command: organizationUnretireCmd,
			retiredAt: `,"retired_at":"2026-07-29T12:00:00Z"`, want: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var body map[string]any
			var decodeErr error
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.Method {
				case http.MethodGet:
					_, err := fmt.Fprintf(w,
						`{"organization":{"id":4,"name":"Example Org","kind":"company","primary_domain":"example.com","description":"Synthetic description.","revision":3%s,"created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"}}`,
						test.retiredAt)
					assert.NoError(err)
				case http.MethodPatch:
					decodeErr = json.NewDecoder(r.Body).Decode(&body)
					_, err := w.Write([]byte(`{"id":4,"name":"Example Org","kind":"company","primary_domain":"example.com","description":"Synthetic description.","revision":4,"created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"}`))
					assert.NoError(err)
				default:
					w.WriteHeader(http.StatusMethodNotAllowed)
				}
			}))
			t.Cleanup(server.Close)
			withStoreResolverConfig(t, &config.Config{
				Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
			})

			runOrganizationCommand(t, test.command, []string{"4"})
			require.NoError(decodeErr)
			assert.Equal("example.com", body["primary_domain"])
			assert.Equal("Synthetic description.", body["description"])
			assert.Equal(test.want, body["retired"])
		})
	}
}

func TestOrganizationDeleteReportsEmploymentConflict(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, err := w.Write([]byte(`{"organization":{"id":4,"name":"Example Org","kind":"company","revision":1,"created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"}}`))
			assert.NoError(err)
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, err := w.Write([]byte(`{"error":"organization_has_employments","message":"Organization still has employment records; retire it instead of deleting it"}`))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	command := cloneOrganizationCommand(organizationDeleteCmd)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"4"})
	err := command.Execute()
	require.Error(err)
	assert.Contains(err.Error(), "retire it instead of deleting it")
}

func TestOrganizationListSendsQueryParametersAndRendersTable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var rawQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/organizations", r.URL.Path)
		rawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"organizations":[{"id":4,"name":"Example Org","kind":"company","primary_domain":"example.com","revision":2,"created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"}],"total":1,"limit":25,"offset":0}`))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	output := runOrganizationCommand(t, organizationListCmd, []string{"--limit", "25", "--query", "example", "--include-retired"})
	assert.Contains(rawQuery, "limit=25")
	assert.Contains(rawQuery, "q=example")
	assert.Contains(rawQuery, "include_retired=true")
	assert.Contains(output, "ID")
	assert.Contains(output, "Example Org")
	assert.Contains(output, "example.com")
	require.NotContains(output, "null")
}

func TestOrganizationShowHistoryPrintsSupersededRows(t *testing.T) {
	assert := assert.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/organizations/4/history", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"organization":{"id":4,"name":"Example Org","kind":"company","revision":2,"created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z"},"names":[{"name":"Earlier Org","name_kind":"former","name_normalized":"earlier org","organization_id":4,"envelope":{"id":1,"ordinal":0,"source":"user","created_at":"2026-07-30T12:00:00Z","updated_at":"2026-07-30T12:00:00Z","active_until":"2026-07-30T12:01:00Z"}}]}`))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	output := runOrganizationCommand(t, organizationShowCmd, []string{"4", "--history"})
	assert.Contains(output, "Earlier Org")
	assert.Contains(output, "active until 2026-07-30T12:01:00Z")
}

func TestOrganizationAttributeSetTransmitsOptionalExpectedValueID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var body map[string]any
	var decodeErr error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/v1/organizations/4/attributes", r.URL.Path)
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		decodeErr = decoder.Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, err := w.Write([]byte(`{"value":{"id":8,"organization_id":4,"definition_id":2,"definition_slug":"industry_focus","ordinal":0,"value":{"type":"text","text":"information retrieval"},"active_from":"2026-07-30T12:00:00Z","created_at":"2026-07-30T12:00:00Z","source":"user"},"dry_run":false}`))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	output := runOrganizationCommand(t, organizationAttributeSetCmd, []string{
		"4", "--definition", "industry_focus", "--text", "information retrieval",
		"--expected-value-id", "7", "--ordinal", "2",
	})
	require.NoError(decodeErr)
	assert.Equal(json.Number("7"), body["expected_value_id"])
	assert.Equal(json.Number("2"), body["ordinal"],
		"a multi-valued attribute update must target its ordinal")
	assert.Contains(output, "Set industry_focus: information retrieval")
}

func TestOrganizationAttributeClearForwardsOrdinalExpectedValueIDAndDryRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodDelete, r.Method)
		assert.Equal("/api/v1/organizations/4/attributes/industry_focus", r.URL.Path)
		assert.Equal("2", r.URL.Query().Get("ordinal"))
		assert.Equal("8", r.URL.Query().Get("expected_value_id"))
		assert.Equal("true", r.URL.Query().Get("dry_run"))
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"dry_run":true,"superseded":{
			"id":8,"organization_id":4,"definition_id":2,
			"definition_slug":"industry_focus","ordinal":2,
			"value":{"type":"text","text":"information retrieval"},
			"active_from":"2026-07-30T12:00:00Z",
			"active_until":"2026-07-30T13:00:00Z",
			"created_at":"2026-07-30T12:00:00Z",
			"source":"user"}}`))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	output := runOrganizationCommand(t, organizationAttributeClearCmd, []string{
		"4", "industry_focus", "--ordinal", "2", "--expected-value-id", "8", "--dry-run",
	})
	require.NotEmpty(output)
	assert.Contains(output, "Dry run: Superseded industry_focus ordinal 2")
}

func cloneOrganizationCommand(template *cobra.Command) *cobra.Command {
	command := &cobra.Command{Use: template.Use, Args: template.Args, RunE: template.RunE}
	command.Flags().AddFlagSet(template.Flags())
	command.Flags().VisitAll(func(flag *pflag.Flag) {
		_ = flag.Value.Set(flag.DefValue)
		flag.Changed = false
	})
	return command
}
func runOrganizationCommand(t *testing.T, template *cobra.Command, args []string) string {
	t.Helper()
	saved := organizationJSON
	organizationJSON = false
	t.Cleanup(func() { organizationJSON = saved })
	command := cloneOrganizationCommand(template)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	require.NoError(t, command.Execute())
	return output.String()
}
