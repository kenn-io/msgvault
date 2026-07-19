package discord

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestDiscoverCatalogFindsAndFiltersIndependentContainers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		paths = append(paths, request.URL.RequestURI())
		mu.Unlock()
		serveCatalogFixture(w, request)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "test-token")
	require.NoError(err)

	prior := map[string]ThreadCatalogState{
		"301": {
			PublicArchiveWatermark:  "2026-07-17T00:00:00Z",
			PrivateArchiveWatermark: "2026-07-16T00:00:00Z",
		},
	}
	wantPrior := prior["301"]
	result, err := DiscoverCatalog(context.Background(), client, "201", config.DiscordGuildConfig{
		Include: []string{"301", "402", "403"},
		Exclude: []string{"302", "403"},
	}, prior, false)
	require.NoError(err)
	assert.Empty(result.Issues)

	byID := catalogContainersByID(result.Containers)
	assert.Equal([]string{"301", "401", "402", "410", "411", "412"}, sortedCatalogIDs(byID))
	assert.NotContains(byID, "303", "forum parents are catalogs, not message containers")
	assert.NotContains(byID, "304", "media parents are catalogs, not message containers")
	require.Contains(byID, "401")
	assert.Equal("active-name", byID["401"].Channel.Name, "dedupe keeps richer active metadata")
	assert.Equal("active topic", byID["401"].Channel.Topic)
	require.NotNil(byID["401"].Channel.ThreadMetadata)
	threadMetadata := byID["401"].Channel.ThreadMetadata
	assert.True(threadMetadata.Archived, "newer archived state wins")
	assert.Equal(4320, threadMetadata.AutoArchiveDuration, "sparse archive copy keeps active duration")
	assert.Equal(mustParseTime(t, "2026-07-19T00:00:00Z"), threadMetadata.ArchiveTimestamp, "archive copy advances timestamp")
	assert.True(threadMetadata.Locked, "sparse archive copy keeps useful locked state")
	assert.True(threadMetadata.Invitable, "sparse archive copy keeps useful invitable state")
	require.NotNil(threadMetadata.CreateTimestamp)
	assert.Equal(mustParseTime(t, "2026-07-10T00:00:00Z"), *threadMetadata.CreateTimestamp)
	require.NotNil(byID["401"].Parent)
	assert.Equal("301", byID["401"].Parent.ID)
	assert.Equal("general", byID["401"].Parent.Name)
	require.Contains(byID, "402")
	assert.Equal("302", byID["402"].Parent.ID, "explicit thread include overrides excluded parent")
	assert.NotContains(byID, "403", "same-level thread exclude wins")
	require.Contains(byID, "412")
	assert.Equal("301", byID["412"].Channel.ParentID, "archive endpoint supplies a sparse thread's parent")
	require.NotNil(byID["412"].Parent)
	assert.Equal("general", byID["412"].Parent.Name)

	assert.Equal("2026-07-19T00:00:00Z", result.ThreadCatalog["301"].PublicArchiveWatermark)
	assert.Equal("2026-07-18T00:00:00Z", result.ThreadCatalog["301"].PrivateArchiveWatermark)
	assert.Equal(wantPrior, prior["301"], "input state remains caller-owned")

	mu.Lock()
	requests := slices.Clone(paths)
	mu.Unlock()
	assert.Contains(requests, "/channels/301/threads/archived/public?before=2026-07-18T00%3A00%3A00Z&limit=100")
	assert.NotContains(requests, "/channels/301/threads/archived/public?before=2026-07-16T00%3A00%3A00Z&limit=100", "incremental scan stops after the complete boundary page")
	assert.Contains(requests, "/channels/303/threads/archived/public?limit=100", "excluded catalogs are still scanned for explicit child overrides")
	assert.Contains(requests, "/channels/304/threads/archived/public?limit=100")
}

func TestDiscoverCatalogFullScanExhaustsArchivePages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var publicRequests int
	var privateRequests int
	var privateBefore []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/channels/301/threads/archived/public" {
			publicRequests++
			before := request.URL.Query().Get("before")
			if before == "" {
				writeCatalogThreads(w, true, catalogThread("421", "301", "full-new", "2026-07-19T00:00:00Z"))
				return
			}
			writeCatalogThreads(w, false, catalogThread("422", "301", "full-old", "2026-07-01T00:00:00Z"))
			return
		}
		if request.URL.Path == "/channels/301/users/@me/threads/archived/private" {
			privateRequests++
			before := request.URL.Query().Get("before")
			privateBefore = append(privateBefore, before)
			if before == "" {
				writeCatalogThreads(w, true, catalogThread("423", "301", "private-full-new", "2026-07-20T00:00:00Z"))
				return
			}
			writeCatalogThreads(w, false, catalogThread("424", "301", "private-full-old", "2026-07-02T00:00:00Z"))
			return
		}
		serveMinimalCatalog(w, request)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "test-token")
	require.NoError(err)

	result, err := DiscoverCatalog(context.Background(), client, "201", config.DiscordGuildConfig{}, map[string]ThreadCatalogState{
		"301": {PublicArchiveWatermark: "2026-07-18T00:00:00Z", PrivateArchiveWatermark: "2026-07-18T00:00:00Z"},
	}, true)

	require.NoError(err)
	assert.Equal(2, publicRequests)
	assert.Equal(2, privateRequests)
	assert.Equal([]string{"", "423"}, privateBefore)
	assert.Contains(catalogContainersByID(result.Containers), "422", "full scan does not stop at the prior watermark")
	assert.Contains(catalogContainersByID(result.Containers), "424", "private full scan emits its second page")
	assert.Equal("2026-07-19T00:00:00Z", result.ThreadCatalog["301"].PublicArchiveWatermark)
	assert.Equal("2026-07-20T00:00:00Z", result.ThreadCatalog["301"].PrivateArchiveWatermark)
}

func TestDiscoverCatalogIncrementalPrivateArchiveExhaustsIDOrderedPages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var privateBefore []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/channels/301/users/@me/threads/archived/private":
			before := request.URL.Query().Get("before")
			privateBefore = append(privateBefore, before)
			switch before {
			case "":
				writeCatalogThreads(w, true, catalogThread("500", "301", "older timestamp", "2026-07-17T00:00:00Z"))
			case "500":
				writeCatalogThreads(w, false, catalogThread("400", "301", "newer timestamp on later ID page", "2026-07-19T00:00:00Z"))
			default:
				writeDiscordJSON(w, http.StatusBadRequest, map[string]any{"code": 0, "message": "unexpected private cursor"})
			}
		default:
			serveMinimalCatalog(w, request)
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "test-token")
	require.NoError(err)

	result, err := DiscoverCatalog(t.Context(), client, "201", config.DiscordGuildConfig{}, map[string]ThreadCatalogState{
		"301": {PrivateArchiveWatermark: "2026-07-18T00:00:00Z"},
	}, false)

	require.NoError(err)
	assert.Equal([]string{"", "500"}, privateBefore)
	assert.Contains(catalogContainersByID(result.Containers), "400",
		"an older-ID page can contain a newly archived or newly accessible thread")
	assert.Equal("2026-07-19T00:00:00Z", result.ThreadCatalog["301"].PrivateArchiveWatermark)
}

func TestDiscoverCatalogQueriesPrivateArchivesOnlyForGuildTextParents(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var mu sync.Mutex
	var privatePaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		if strings.Contains(request.URL.Path, "/users/@me/threads/archived/private") {
			privatePaths = append(privatePaths, request.URL.Path)
		}
		mu.Unlock()
		switch request.URL.Path {
		case "/guilds/201/channels":
			writeDiscordJSON(w, http.StatusOK, []map[string]any{
				{"id": "301", "guild_id": "201", "type": 0, "name": "text"},
				{"id": "302", "guild_id": "201", "type": 5, "name": "announcement"},
				{"id": "303", "guild_id": "201", "type": 15, "name": "forum"},
				{"id": "304", "guild_id": "201", "type": 16, "name": "media"},
			})
		case "/guilds/201/threads/active":
			writeDiscordJSON(w, http.StatusOK, map[string]any{"threads": []any{}})
		default:
			writeCatalogThreads(w, false)
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "test-token")
	require.NoError(err)

	_, err = DiscoverCatalog(t.Context(), client, "201", config.DiscordGuildConfig{}, nil, false)
	require.NoError(err)

	mu.Lock()
	got := slices.Clone(privatePaths)
	mu.Unlock()
	assert.Equal([]string{"/channels/301/users/@me/threads/archived/private"}, got)
}

func TestDiscoverCatalogRejectsDuplicateThreadWithConflictingParent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	const priorWatermark = "2026-07-17T00:00:00Z"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/guilds/201/channels":
			writeDiscordJSON(w, http.StatusOK, []map[string]any{
				{"id": "301", "guild_id": "201", "type": 0, "name": "first"},
				{"id": "302", "guild_id": "201", "type": 0, "name": "second"},
			})
		case "/guilds/201/threads/active":
			writeDiscordJSON(w, http.StatusOK, map[string]any{"threads": []map[string]any{
				{"id": "450", "guild_id": "201", "parent_id": "301", "type": 11, "name": "active"},
			}})
		case "/channels/302/threads/archived/public":
			writeCatalogThreads(w, false, catalogThread("450", "302", "conflict", "2026-07-19T00:00:00Z"))
		default:
			writeCatalogThreads(w, false)
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "test-token")
	require.NoError(err)
	prior := map[string]ThreadCatalogState{"302": {PublicArchiveWatermark: priorWatermark}}

	result, err := DiscoverCatalog(t.Context(), client, "201", config.DiscordGuildConfig{}, prior, false)

	require.Error(err)
	require.ErrorIs(err, ErrMalformedCatalog)
	assert.Equal(priorWatermark, result.ThreadCatalog["302"].PublicArchiveWatermark)
	require.NotEmpty(result.Issues)
	issue := result.Issues[len(result.Issues)-1]
	assert.Equal(CatalogScopePublicArchive, issue.Scope)
	assert.Equal("302", issue.ParentID)
	assert.True(issue.Fatal)
}

func TestDiscoverCatalogArchiveDenialsAreNonfatalAndIndependent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/channels/301/threads/archived/public":
			writeDiscordJSON(w, http.StatusForbidden, map[string]any{"code": 50013, "message": "missing permissions"})
		case "/channels/301/users/@me/threads/archived/private":
			writeCatalogThreads(w, false, catalogThread("431", "301", "private", "2026-07-19T00:00:00Z"))
		case "/channels/302/threads/archived/public":
			writeCatalogThreads(w, false, catalogThread("432", "302", "public", "2026-07-18T00:00:00Z"))
		default:
			serveTwoParentCatalog(w, request)
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "test-token")
	require.NoError(err)
	prior := map[string]ThreadCatalogState{
		"301": {PublicArchiveWatermark: "2026-07-17T00:00:00Z", PrivateArchiveWatermark: "2026-07-17T00:00:00Z"},
		"302": {PublicArchiveWatermark: "2026-07-17T00:00:00Z", PrivateArchiveWatermark: "2026-07-16T00:00:00Z"},
	}

	result, err := DiscoverCatalog(context.Background(), client, "201", config.DiscordGuildConfig{}, prior, false)

	require.NoError(err)
	require.Len(result.Issues, 1)
	assert.Equal(CatalogIssueForbidden, result.Issues[0].Kind)
	assert.False(result.Issues[0].Fatal)
	assert.Equal("2026-07-17T00:00:00Z", result.ThreadCatalog["301"].PublicArchiveWatermark)
	assert.Equal("2026-07-19T00:00:00Z", result.ThreadCatalog["301"].PrivateArchiveWatermark)
	assert.Equal("2026-07-18T00:00:00Z", result.ThreadCatalog["302"].PublicArchiveWatermark)
	assert.Equal("2026-07-16T00:00:00Z", result.ThreadCatalog["302"].PrivateArchiveWatermark)
}

func TestDiscoverCatalogRejectsActiveThreadsWithoutAuthoritativeParents(t *testing.T) {
	tests := []struct {
		name       string
		parentData map[string]any
	}{
		{name: "missing parent ID", parentData: map[string]any{}},
		{name: "unknown parent ID", parentData: map[string]any{"parent_id": "999"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/guilds/201/threads/active" {
					malformed := map[string]any{
						"id": "451", "guild_id": "201", "type": 11, "name": "malformed",
						"thread_metadata": map[string]any{"archived": false, "archive_timestamp": "2026-07-19T00:00:00Z"},
					}
					maps.Copy(malformed, tt.parentData)
					writeDiscordJSON(w, http.StatusOK, map[string]any{"threads": []map[string]any{
						{"id": "450", "guild_id": "201", "parent_id": "301", "type": 11, "name": "valid", "thread_metadata": map[string]any{"archived": false, "archive_timestamp": "2026-07-19T00:00:00Z"}},
						malformed,
					}})
					return
				}
				serveMinimalCatalog(w, request)
			}))
			t.Cleanup(server.Close)
			client, err := NewClient(server.URL, "test-token")
			require.NoError(err)

			result, err := DiscoverCatalog(context.Background(), client, "201", config.DiscordGuildConfig{}, nil, false)

			require.Error(err)
			byID := catalogContainersByID(result.Containers)
			assert.Contains(byID, "301")
			assert.Contains(byID, "450", "valid active threads survive a malformed sibling")
			assert.NotContains(byID, "451")
			require.Len(result.Issues, 1)
			assert.Equal(CatalogScopeActiveThreads, result.Issues[0].Scope)
			assert.Equal(CatalogIssueMalformedPage, result.Issues[0].Kind)
			assert.True(result.Issues[0].Fatal)
			require.ErrorIs(result.Issues[0].Err, ErrMalformedCatalog)
		})
	}
}

func TestDiscoverCatalogRejectsArchivedThreadWithMismatchedEndpointParent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/channels/301/threads/archived/public":
			writeCatalogThreads(w, false,
				catalogThread("460", "301", "valid", "2026-07-19T00:00:00Z"),
				catalogThread("461", "999", "mismatched", "2026-07-18T00:00:00Z"),
			)
		case "/channels/301/users/@me/threads/archived/private":
			writeCatalogThreads(w, false, catalogThread("462", "301", "private-valid", "2026-07-20T00:00:00Z"))
		default:
			serveMinimalCatalog(w, request)
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "test-token")
	require.NoError(err)
	prior := map[string]ThreadCatalogState{
		"301": {PublicArchiveWatermark: "2026-07-17T00:00:00Z", PrivateArchiveWatermark: "2026-07-17T00:00:00Z"},
	}

	result, err := DiscoverCatalog(context.Background(), client, "201", config.DiscordGuildConfig{}, prior, false)

	require.Error(err)
	byID := catalogContainersByID(result.Containers)
	assert.Contains(byID, "460", "valid preceding archive entries remain in the partial result")
	assert.NotContains(byID, "461")
	assert.Contains(byID, "462", "private archive scan remains independent")
	require.Len(result.Issues, 1)
	assert.Equal(CatalogScopePublicArchive, result.Issues[0].Scope)
	assert.Equal(CatalogIssueMalformedPage, result.Issues[0].Kind)
	assert.True(result.Issues[0].Fatal)
	require.ErrorIs(result.Issues[0].Err, ErrMalformedCatalog)
	assert.Equal("2026-07-17T00:00:00Z", result.ThreadCatalog["301"].PublicArchiveWatermark)
	assert.Equal("2026-07-20T00:00:00Z", result.ThreadCatalog["301"].PrivateArchiveWatermark)
}

func TestDiscoverCatalogFatalArchiveFailuresPreserveOnlyFailedWatermarks(t *testing.T) {
	tests := []struct {
		name     string
		response func(http.ResponseWriter, *http.Request)
		wantKind CatalogIssueKind
		cancel   bool
	}{
		{
			name: "malformed cursor",
			response: func(w http.ResponseWriter, _ *http.Request) {
				writeDiscordJSON(w, http.StatusOK, map[string]any{"threads": []map[string]any{{"id": "441", "parent_id": "301", "type": 11}}, "has_more": true})
			},
			wantKind: CatalogIssueMalformedPage,
		},
		{
			name: "decode failure",
			response: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"threads":`))
			},
			wantKind: CatalogIssueDecode,
		},
		{
			name: "server failure",
			response: func(w http.ResponseWriter, _ *http.Request) {
				writeDiscordJSON(w, http.StatusInternalServerError, map[string]any{"code": 0, "message": "retry"})
			},
			wantKind: CatalogIssueAPI,
		},
		{
			name: "cancellation",
			response: func(_ http.ResponseWriter, request *http.Request) {
				<-request.Context().Done()
			},
			wantKind: CatalogIssueCanceled,
			cancel:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/channels/301/threads/archived/public":
					if !tt.cancel && request.URL.Query().Get("before") == "" {
						writeCatalogThreads(w, true, catalogThread("440", "301", "partial", "2026-07-19T00:00:00Z"))
						return
					}
					tt.response(w, request)
				case "/channels/301/users/@me/threads/archived/private":
					writeCatalogThreads(w, false, catalogThread("442", "301", "private-ok", "2026-07-19T00:00:00Z"))
				default:
					serveMinimalCatalog(w, request)
				}
			}))
			t.Cleanup(server.Close)
			client, err := NewClient(server.URL, "test-token")
			require.NoError(err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancel {
				time.AfterFunc(20*time.Millisecond, cancel)
			}
			prior := map[string]ThreadCatalogState{
				"301": {PublicArchiveWatermark: "2026-07-17T00:00:00Z", PrivateArchiveWatermark: "2026-07-17T00:00:00Z"},
			}

			result, err := DiscoverCatalog(ctx, client, "201", config.DiscordGuildConfig{}, prior, false)

			require.Error(err)
			require.NotEmpty(result.Issues)
			assert.Equal(tt.wantKind, result.Issues[0].Kind)
			assert.True(result.Issues[0].Fatal)
			if tt.wantKind == CatalogIssueMalformedPage {
				require.ErrorIs(result.Issues[0].Err, ErrMalformedCatalog)
			}
			assert.Equal("2026-07-17T00:00:00Z", result.ThreadCatalog["301"].PublicArchiveWatermark)
			if tt.cancel {
				assert.Equal("2026-07-17T00:00:00Z", result.ThreadCatalog["301"].PrivateArchiveWatermark)
			} else {
				assert.Contains(catalogContainersByID(result.Containers), "440", "containers from successful pages survive a later page failure")
				assert.Equal("2026-07-19T00:00:00Z", result.ThreadCatalog["301"].PrivateArchiveWatermark, "sibling archive kind advances independently")
			}
		})
	}
}

func TestDiscoverCatalogTopLevelAndActiveFailuresReturnPartialResult(t *testing.T) {
	t.Run("top-level channels", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeDiscordJSON(w, http.StatusUnauthorized, map[string]any{"code": 0, "message": "unauthorized"})
		}))
		t.Cleanup(server.Close)
		client, err := NewClient(server.URL, "test-token")
		require.NoError(err)

		result, err := DiscoverCatalog(context.Background(), client, "201", config.DiscordGuildConfig{}, nil, false)

		require.Error(err)
		require.Len(result.Issues, 1)
		assert.Equal(CatalogScopeGuildChannels, result.Issues[0].Scope)
		assert.True(result.Issues[0].Fatal)
	})

	t.Run("active threads", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			if request.URL.Path == "/guilds/201/threads/active" {
				writeDiscordJSON(w, http.StatusForbidden, map[string]any{"code": 50013, "message": "forbidden"})
				return
			}
			serveMinimalCatalog(w, request)
		}))
		t.Cleanup(server.Close)
		client, err := NewClient(server.URL, "test-token")
		require.NoError(err)

		result, err := DiscoverCatalog(context.Background(), client, "201", config.DiscordGuildConfig{}, nil, false)

		require.Error(err)
		assert.Contains(catalogContainersByID(result.Containers), "301")
		require.NotEmpty(result.Issues)
		assert.Equal(CatalogScopeActiveThreads, result.Issues[0].Scope)
		assert.True(result.Issues[0].Fatal)
	})
}

func serveCatalogFixture(w http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/guilds/201/channels":
		writeDiscordJSON(w, http.StatusOK, []map[string]any{
			{"id": "301", "guild_id": "201", "type": 0, "name": "general", "topic": "parent topic"},
			{"id": "302", "guild_id": "201", "type": 5, "name": "announcements"},
			{"id": "303", "guild_id": "201", "type": 15, "name": "forum"},
			{"id": "304", "guild_id": "201", "type": 16, "name": "media"},
			{"id": "305", "guild_id": "201", "type": 4, "name": "category"},
			{"id": "306", "guild_id": "201", "type": 2, "name": "voice"},
		})
	case "/guilds/201/threads/active":
		writeDiscordJSON(w, http.StatusOK, map[string]any{"threads": []map[string]any{
			{"id": "401", "guild_id": "201", "parent_id": "301", "type": 11, "name": "active-name", "topic": "active topic", "thread_metadata": map[string]any{"archived": false, "auto_archive_duration": 4320, "archive_timestamp": "2026-07-18T12:00:00Z", "locked": true, "invitable": true, "create_timestamp": "2026-07-10T00:00:00Z"}},
			{"id": "402", "guild_id": "201", "parent_id": "302", "type": 10, "name": "explicit-override", "thread_metadata": map[string]any{"archived": false, "archive_timestamp": "2026-07-18T12:00:00Z"}},
			{"id": "403", "guild_id": "201", "parent_id": "303", "type": 11, "name": "same-level-excluded", "thread_metadata": map[string]any{"archived": false, "archive_timestamp": "2026-07-18T12:00:00Z"}},
		}})
	case "/channels/301/threads/archived/public":
		if request.URL.Query().Get("before") == "" {
			sparseDuplicate := catalogThread("401", "301", "", "2026-07-19T00:00:00Z")
			sparseDuplicate["thread_metadata"] = map[string]any{"archived": true, "archive_timestamp": "2026-07-19T00:00:00Z"}
			writeCatalogThreads(w, true,
				sparseDuplicate,
				catalogThread("410", "301", "new-public", "2026-07-18T00:00:00Z"),
			)
			return
		}
		writeCatalogThreads(w, true, catalogThread("411", "301", "boundary-public", "2026-07-16T00:00:00Z"))
	case "/channels/301/users/@me/threads/archived/private":
		thread := catalogThread("412", "301", "private", "2026-07-18T00:00:00Z")
		delete(thread, "parent_id")
		writeCatalogThreads(w, false, thread)
	default:
		if isArchivePath(request.URL.Path) {
			writeCatalogThreads(w, false)
			return
		}
		writeDiscordJSON(w, http.StatusNotFound, map[string]any{"code": 0, "message": "not found"})
	}
}

func serveMinimalCatalog(w http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/guilds/201/channels":
		writeDiscordJSON(w, http.StatusOK, []map[string]any{{"id": "301", "guild_id": "201", "type": 0, "name": "general"}})
	case "/guilds/201/threads/active":
		writeDiscordJSON(w, http.StatusOK, map[string]any{"threads": []any{}})
	default:
		if isArchivePath(request.URL.Path) {
			writeCatalogThreads(w, false)
			return
		}
		writeDiscordJSON(w, http.StatusNotFound, map[string]any{"code": 0, "message": "not found"})
	}
}

func serveTwoParentCatalog(w http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/guilds/201/channels":
		writeDiscordJSON(w, http.StatusOK, []map[string]any{
			{"id": "301", "guild_id": "201", "type": 0, "name": "general"},
			{"id": "302", "guild_id": "201", "type": 5, "name": "announcements"},
		})
	case "/guilds/201/threads/active":
		writeDiscordJSON(w, http.StatusOK, map[string]any{"threads": []any{}})
	default:
		writeDiscordJSON(w, http.StatusNotFound, map[string]any{"code": 0, "message": "not found"})
	}
}

func catalogThread(id, parentID, name, archiveTimestamp string) map[string]any {
	return map[string]any{
		"id": id, "guild_id": "201", "parent_id": parentID, "type": 11, "name": name,
		"thread_metadata": map[string]any{"archived": true, "archive_timestamp": archiveTimestamp, "auto_archive_duration": 1440},
	}
}

func writeCatalogThreads(w http.ResponseWriter, hasMore bool, threads ...map[string]any) {
	writeDiscordJSON(w, http.StatusOK, map[string]any{"threads": threads, "members": []any{}, "has_more": hasMore})
}

func isArchivePath(path string) bool {
	return slices.Contains([]string{
		"/channels/301/threads/archived/public",
		"/channels/301/users/@me/threads/archived/private",
		"/channels/302/threads/archived/public",
		"/channels/302/users/@me/threads/archived/private",
		"/channels/303/threads/archived/public",
		"/channels/303/users/@me/threads/archived/private",
		"/channels/304/threads/archived/public",
		"/channels/304/users/@me/threads/archived/private",
	}, path)
}

func catalogContainersByID(containers []CatalogContainer) map[string]CatalogContainer {
	byID := make(map[string]CatalogContainer, len(containers))
	for _, container := range containers {
		byID[container.Channel.ID] = container
	}
	return byID
}

func sortedCatalogIDs(containers map[string]CatalogContainer) []string {
	ids := make([]string, 0, len(containers))
	for id := range containers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
