# Microsoft Teams Ingestion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Sync the signed-in user's own Microsoft Teams 1:1/group/meeting chats and channel messages into msgvault via delegated Microsoft Graph, searchable alongside Gmail/Outlook.

**Architecture:** A new `internal/teams` package (Graph REST client + sync orchestration + message mapping) talks to Graph using a delegated OAuth token obtained through a new Graph-scoped manager in `internal/microsoft` (the existing IMAP `Manager` rejects non-IMAP tokens). Messages are written through the existing granular store path (`UpsertMessage` → `UpsertMessageBody` → `UpsertMessageRawWithFormat("teams_json")` → `UpsertFTS`), reusing the generic chat schema with no new core tables. Chats incrementally sync via `lastModifiedDateTime` list filtering (no delegated chat delta exists); channels via `/messages/delta`. Per-conversation cursors persist as a JSON map in `sync_runs.cursor_after` (read back via `GetLastSuccessfulSync`), mirroring Gmail's historyID and fbmessenger's resume blob.

**Tech Stack:** Go, Microsoft Graph v1.0 REST, `golang.org/x/oauth2`, existing `internal/store` (SQLite/Postgres), testify, `net/http/httptest` for a fake Graph server.

**Spec:** `docs/superpowers/specs/2026-06-18-teams-ingestion-design.md` (load-bearing verified 2026-06-19; transcripts are a separate spec and OUT OF SCOPE here).

---

## Conventions locked in by this plan

- `source_type` = `"teams"` (new constant `sourceTypeTeams`).
- `message_type` = `"teams"`; `raw_format` = `"teams_json"`.
- `conversation_type`: `oneOnOne` → `"direct_chat"`; `group` and `meeting` → `"group_chat"`.
- Token file: `teams_<email>.json` (distinct from IMAP's `microsoft_<email>.json`).
- Participant resolution: resolve AAD object id → `mail` via `GET /users/{id}`; if `mail` present (or sender is `emailUser` whose id IS an email) use `EnsureParticipant(email, displayName, domain)` so Teams unifies with Gmail/Outlook by email; otherwise `EnsureParticipantByIdentifier("teams", aadObjectId, displayName)`.
- Cursor model: a JSON `SyncState` map persisted via `CompleteSync(syncID, jsonString)` into `sync_runs.cursor_after`, read next run via `GetLastSuccessfulSync(sourceID).CursorAfter`. In-run resume writes the same JSON to `cursor_before` via `UpdateSyncCheckpoint`.
- Write path per message (from WhatsApp/fbmessenger): `UpsertMessage` → `UpsertMessageBody` → `UpsertMessageRawWithFormat(id, raw, "teams_json")` → `UpsertFTS` → (recipients/reactions/attachments as needed). FTS is ALWAYS a separate call.
- Graph base URL is injectable so tests point the client at `httptest.NewServer`.

## File structure

```
internal/microsoft/graph_oauth.go        # NEW: delegated Graph token manager (no IMAP scope coupling)
internal/microsoft/graph_oauth_test.go   # NEW
internal/teams/
  ├── client.go            # NEW: Graph REST client (base URL, token, paging, 429/Retry-After)
  ├── client_test.go       # NEW: httptest-backed
  ├── types.go             # NEW: Graph DTOs (chat, chatMessage, channel, identitySet, hostedContent, delta envelope) + ImportOptions/ImportSummary
  ├── mapping.go           # NEW: chatMessage -> store.Message + body/text + participants
  ├── mapping_test.go      # NEW
  ├── participants.go      # NEW: identity -> participant resolution (with /users/{id} cache)
  ├── participants_test.go # NEW
  ├── syncstate.go         # NEW: SyncState JSON (per-conversation cursors) marshal/load
  ├── syncstate_test.go    # NEW
  ├── importer.go          # NEW: orchestration (chats + channels), persist sequence, checkpointing
  └── importer_test.go     # NEW: end-to-end against fake Graph server
cmd/msgvault/cmd/constants.go     # MODIFY: add sourceTypeTeams
cmd/msgvault/cmd/add_teams.go     # NEW: `add-account --teams` style OAuth + source create
cmd/msgvault/cmd/sync_teams.go    # NEW: `sync-teams <email>` full/incremental
cmd/msgvault/cmd/serve.go         # MODIFY: scheduler case for "teams"
internal/config/config.go         # (reuse MicrosoftConfig.ClientID/TenantID; no change expected)
```

---

## Task 1: Add the `teams` source-type constant

**Files:**
- Modify: `cmd/msgvault/cmd/constants.go`

- [ ] **Step 1: Add the constant**

Open `cmd/msgvault/cmd/constants.go` and add `sourceTypeTeams` alongside the existing source-type constants (the file already defines `sourceTypeGmail`, `sourceTypeIMAP`, etc. around lines 6-8):

```go
	sourceTypeTeams = "teams"
```

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: compiles (unused constant is fine in a `const` block).

- [ ] **Step 3: Commit**

```bash
git add cmd/msgvault/cmd/constants.go
git commit -m "feat(teams): add teams source-type constant"
```

---

## Task 2: Delegated Graph OAuth token manager

The existing `internal/microsoft.Manager` validates `Scopes[0]` is an IMAP scope and namespaces tokens as `microsoft_<email>.json` (`oauth.go:237-254`, `:602`). A Graph token fails that. Add a sibling manager that reuses the browser/device flow but stores `teams_<email>.json` and requests Graph scopes.

**Files:**
- Create: `internal/microsoft/graph_oauth.go`
- Test: `internal/microsoft/graph_oauth_test.go`

- [ ] **Step 1: Write the failing test for token path + scopes**

```go
package microsoft

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGraphTokenPath(t *testing.T) {
	dir := filepath.Join("tmp", "tokens")
	m := &GraphManager{tokensDir: dir}
	assert.Equal(t, filepath.Join(dir, "teams_user@example.com.json"), m.TokenPath("user@example.com"))
}

func TestGraphScopes(t *testing.T) {
	got := GraphScopes()
	assert.Contains(t, got, "https://graph.microsoft.com/Chat.Read")
	assert.Contains(t, got, "https://graph.microsoft.com/ChannelMessage.Read.All")
	assert.Contains(t, got, "https://graph.microsoft.com/Team.ReadBasic.All")
	assert.Contains(t, got, "https://graph.microsoft.com/Channel.ReadBasic.All")
	assert.Contains(t, got, "https://graph.microsoft.com/User.Read")
	assert.Contains(t, got, scopeOfflineAccess)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/microsoft/ -run 'TestGraph' -v`
Expected: FAIL — `GraphManager`/`GraphScopes` undefined.

- [ ] **Step 3: Implement the manager**

Reuse the existing constants (`DefaultTenant`, `redirectPort`, `callbackPath`, `scopeOfflineAccess`) and the existing browser-flow + token-persistence helpers in `oauth.go`. Mirror `NewManager`/`Authorize`/`TokenSource` but with Graph scopes and the `teams_` prefix, and WITHOUT the IMAP `Scopes[0]` validation.

```go
package microsoft

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"golang.org/x/oauth2"
)

// Graph delegated scopes for Teams ingestion (verified live 2026-06-19).
const (
	scopeGraphChatRead       = "https://graph.microsoft.com/Chat.Read"
	scopeGraphChannelMessage = "https://graph.microsoft.com/ChannelMessage.Read.All"
	scopeGraphTeamReadBasic  = "https://graph.microsoft.com/Team.ReadBasic.All"
	scopeGraphChannelBasic   = "https://graph.microsoft.com/Channel.ReadBasic.All"
	scopeGraphUserRead       = "https://graph.microsoft.com/User.Read"
	scopeGraphUserReadBasic  = "https://graph.microsoft.com/User.ReadBasic.All"
)

// GraphScopes is the delegated scope set requested for Teams.
func GraphScopes() []string {
	return []string{
		scopeGraphChatRead,
		scopeGraphChannelMessage,
		scopeGraphTeamReadBasic,
		scopeGraphChannelBasic,
		scopeGraphUserRead,
		scopeGraphUserReadBasic,
		scopeOfflineAccess,
		"openid",
		scopeEmail,
	}
}

// GraphManager performs the delegated browser OAuth flow for Microsoft Graph
// and persists tokens as teams_<email>.json, independent of the IMAP Manager.
type GraphManager struct {
	clientID  string
	tenantID  string
	tokensDir string
	logger    *slog.Logger
}

func NewGraphManager(clientID, tenantID, tokensDir string, logger *slog.Logger) *GraphManager {
	if tenantID == "" {
		tenantID = DefaultTenant
	}
	return &GraphManager{clientID: clientID, tenantID: tenantID, tokensDir: tokensDir, logger: logger}
}

func (m *GraphManager) TokenPath(email string) string {
	return filepath.Join(m.tokensDir, "teams_"+sanitizeEmail(email)+".json")
}

func (m *GraphManager) HasToken(email string) bool {
	_, err := loadTokenFile(m.TokenPath(email))
	return err == nil
}

// Authorize runs the interactive browser auth-code flow and writes the token file.
func (m *GraphManager) Authorize(ctx context.Context, email string) error {
	cfg := m.oauthConfig()
	tok, err := runBrowserFlow(ctx, cfg, m.tenantID, email, m.logger) // existing helper in oauth.go
	if err != nil {
		return fmt.Errorf("graph authorize: %w", err)
	}
	return saveTokenFile(m.TokenPath(email), tokenFileFromOAuth(tok, m.tenantID, GraphScopes()))
}

// TokenSource returns a function yielding a fresh access token (auto-refresh),
// with NO IMAP scope validation.
func (m *GraphManager) TokenSource(ctx context.Context, email string) (func(context.Context) (string, error), error) {
	tf, err := loadTokenFile(m.TokenPath(email))
	if err != nil {
		return nil, fmt.Errorf("no Teams token for %s — run 'msgvault add-account %s --teams': %w", email, email, err)
	}
	cfg := m.oauthConfig()
	src := cfg.TokenSource(ctx, tf.toOAuthToken())
	persisting := persistingTokenSource(src, m.TokenPath(email), tf) // existing helper pattern in oauth.go
	return func(ctx context.Context) (string, error) {
		t, err := persisting.Token()
		if err != nil {
			return "", err
		}
		return t.AccessToken, nil
	}, nil
}

func (m *GraphManager) oauthConfig() *oauth2.Config {
	return newAzureOAuthConfig(m.clientID, m.tenantID, GraphScopes()) // existing helper in oauth.go
}
```

> **Implementation note:** `oauth.go` already contains the browser-flow, token-file load/save, and refresh-persistence helpers used by `Manager`. Reuse them. The exact private helper names (`runBrowserFlow`, `loadTokenFile`, `saveTokenFile`, `persistingTokenSource`, `newAzureOAuthConfig`, `sanitizeEmail`, `tokenFileFromOAuth`, `toOAuthToken`) must be confirmed against `oauth.go` while implementing; if a needed helper is currently unexported-but-IMAP-specific, extract the IMAP-agnostic core into a shared private function rather than duplicating. Do NOT modify the existing IMAP `Manager` behavior.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/microsoft/ -run 'TestGraph' -v`
Expected: PASS.

- [ ] **Step 5: Verify existing Microsoft tests still pass**

Run: `go test ./internal/microsoft/ -v`
Expected: PASS (existing IMAP tests unaffected).

- [ ] **Step 6: Format, vet, commit**

```bash
go fmt ./... && go vet ./...
git add internal/microsoft/graph_oauth.go internal/microsoft/graph_oauth_test.go
git commit -m "feat(teams): delegated Graph OAuth token manager"
```

---

## Task 3: Graph DTO types

**Files:**
- Create: `internal/teams/types.go`

- [ ] **Step 1: Define the Graph DTOs and option/summary structs**

These mirror the Graph v1.0 JSON shapes confirmed in the load-bearing pass (identities carry id + displayName, no email; attachments use `contentType:"reference"`; hosted content via `$value`).

```go
package teams

import "time"

// ---- Graph response envelopes ----

type listResponse[T any] struct {
	Value     []T    `json:"value"`
	NextLink  string `json:"@odata.nextLink"`
	DeltaLink string `json:"@odata.deltaLink"`
}

// ---- Chats & channels ----

type Chat struct {
	ID         string `json:"id"`
	ChatType   string `json:"chatType"` // oneOnOne | group | meeting
	Topic      string `json:"topic"`
	OnlineInfo *struct {
		JoinWebURL string `json:"joinWebUrl"`
	} `json:"onlineMeetingInfo"`
}

type JoinedTeam struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

type Channel struct {
	ID             string `json:"id"`
	DisplayName    string `json:"displayName"`
	MembershipType string `json:"membershipType"` // standard | private | shared
}

// ---- Messages ----

type ChatMessage struct {
	ID                  string         `json:"id"`
	ReplyToID           string         `json:"replyToId"`
	MessageType         string         `json:"messageType"` // message | systemEventMessage | ...
	CreatedDateTime     time.Time      `json:"createdDateTime"`
	LastModifiedDateTime time.Time     `json:"lastModifiedDateTime"`
	DeletedDateTime     *time.Time     `json:"deletedDateTime"`
	Subject             string         `json:"subject"`
	Importance          string         `json:"importance"`
	From                *IdentitySet   `json:"from"`
	Body                MessageBody    `json:"body"`
	Attachments         []Attachment   `json:"attachments"`
	Mentions            []Mention      `json:"mentions"`
	Reactions           []Reaction     `json:"reactions"`
}

type MessageBody struct {
	ContentType string `json:"contentType"` // html | text
	Content     string `json:"content"`
}

type IdentitySet struct {
	User        *Identity `json:"user"`
	Application *Identity `json:"application"`
}

type Identity struct {
	ID               string `json:"id"`
	DisplayName      string `json:"displayName"`
	UserIdentityType string `json:"userIdentityType"` // aadUser | emailUser | anonymousGuest | skypeUser | ...
}

type Attachment struct {
	ID          string `json:"id"`
	ContentType string `json:"contentType"` // "reference" => shared file link
	ContentURL  string `json:"contentUrl"`
	Name        string `json:"name"`
}

type Mention struct {
	ID          int          `json:"id"`
	MentionText string       `json:"mentionText"`
	Mentioned   *IdentitySet `json:"mentioned"`
}

type Reaction struct {
	ReactionType     string       `json:"reactionType"` // like | heart | laugh | ...
	CreatedDateTime  time.Time    `json:"createdDateTime"`
	User             *IdentitySet `json:"user"`
}

// GraphUser is the subset of /users/{id} we resolve for participant email.
type GraphUser struct {
	ID                string `json:"id"`
	Mail              string `json:"mail"`
	UserPrincipalName string `json:"userPrincipalName"`
	DisplayName       string `json:"displayName"`
}

// ---- Importer options/summary ----

type ImportOptions struct {
	Email           string
	AttachmentsDir  string
	IncludeChannels bool // default true; allows chats-only runs
	Limit           int  // 0 = no limit (per-conversation message cap, for scoped runs)
	After           time.Time // zero = no lower bound
}

type ImportSummary struct {
	Duration            time.Duration
	SourceID            int64
	ChatsProcessed      int64
	ChannelsProcessed   int64
	MessagesProcessed   int64
	MessagesAdded       int64
	MessagesUpdated     int64
	ReactionsAdded      int64
	AttachmentsFound    int64
	InlineImagesCopied  int64
	Participants        int64
	Errors              int64
}
```

- [ ] **Step 2: Build**

Run: `go build ./internal/teams/`
Expected: compiles.

- [ ] **Step 3: Commit**

```bash
go fmt ./... && git add internal/teams/types.go
git commit -m "feat(teams): Graph DTO types and importer options"
```

---

## Task 4: Graph REST client with paging and Retry-After

Gmail's client has no `Retry-After` parsing (`client.go:91-188` uses fixed throttles), so implement it here. The client takes an injectable base URL and token function so tests use `httptest`.

**Files:**
- Create: `internal/teams/client.go`
- Test: `internal/teams/client_test.go`

- [ ] **Step 1: Write the failing test (paging + Retry-After)**

```go
package teams

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tokenFn(string) (string, error) { return "test-token", nil }

func TestClientGetJSONPaging(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.Write([]byte(`{"value":[{"id":"a"}],"@odata.nextLink":"` + r.Host + `/page2"}`))
			return
		}
		w.Write([]byte(`{"value":[{"id":"b"}],"@odata.deltaLink":"DELTA"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, func(context.Context) (string, error) { return "test-token", nil }, 5)
	var got []Chat
	delta, err := c.getAllPages(context.Background(), "/me/chats", func(page []Chat) { got = append(got, page...) })
	require.NoError(t, err)
	assert.Equal(t, "DELTA", delta)
	assert.Len(t, got, 2)
}

func TestClientRetryAfter(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"value":[]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50)
	_, err := c.getAllPages(context.Background(), "/x", func([]Chat) {})
	require.NoError(t, err)
	assert.EqualValues(t, 2, atomic.LoadInt32(&calls))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/teams/ -run 'TestClient' -v`
Expected: FAIL — `NewClient`/`getAllPages` undefined.

- [ ] **Step 3: Implement the client**

```go
package teams

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

const maxRetries = 8

type TokenFunc func(context.Context) (string, error)

type Client struct {
	baseURL string
	token   TokenFunc
	http    *http.Client
	limiter *rate.Limiter
}

func NewClient(baseURL string, token TokenFunc, qps float64) *Client {
	if qps <= 0 {
		qps = 5
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 60 * time.Second},
		limiter: rate.NewLimiter(rate.Limit(qps), 1),
	}
}

// get performs a single authenticated GET with rate limiting + Retry-After.
func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	if !strings.HasPrefix(url, "http") {
		url = c.baseURL + url
	}
	for attempt := 0; attempt < maxRetries; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, err
		}
		tok, err := c.token(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusOK:
			return body, nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			wait := retryAfter(resp.Header.Get("Retry-After"), attempt)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			continue
		default:
			return nil, fmt.Errorf("graph GET %s: status %d: %s", url, resp.StatusCode, string(body))
		}
	}
	return nil, fmt.Errorf("graph GET %s: exhausted %d retries", url, maxRetries)
}

func retryAfter(header string, attempt int) time.Duration {
	if header != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil {
			return time.Duration(secs) * time.Second
		}
	}
	// exponential fallback, capped
	d := time.Duration(1<<uint(attempt)) * time.Second
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

func (c *Client) getJSON(ctx context.Context, url string, out any) error {
	body, err := c.get(ctx, url)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// getAllPages follows @odata.nextLink, invoking fn per page, and returns the
// terminal @odata.deltaLink (empty for non-delta endpoints).
func (c *Client) getAllPages(ctx context.Context, startURL string, fn func([]Chat)) (string, error) {
	url := startURL
	for {
		var page listResponse[Chat]
		if err := c.getJSON(ctx, url, &page); err != nil {
			return "", err
		}
		fn(page.Value)
		if page.NextLink != "" {
			url = page.NextLink
			continue
		}
		return page.DeltaLink, nil
	}
}
```

> The test JSON returns a `nextLink` of the form `<host>/page2`; the client prefixes with `http` when absent. Real Graph returns absolute `https://graph.microsoft.com/...` links. In tests, the fake server's `nextLink` must be absolute — adjust the test handler to emit `"http://"+r.Host+"/page2"` if the assertion fails. (Keep the generic-over-`Chat` `getAllPages` for chats; add typed twins `getAllMessages`/`getAllChannels` in Task 6/7, or generalize with a small JSON-roundtrip helper — choose the minimal approach when implementing.)

> Add `golang.org/x/time/rate` if not already in go.mod: `go get golang.org/x/time/rate` (it is a common transitive dep; verify with `go list -m golang.org/x/time` first).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/teams/ -run 'TestClient' -v`
Expected: PASS.

- [ ] **Step 5: Format, vet, commit**

```bash
go fmt ./... && go vet ./internal/teams/
git add internal/teams/client.go internal/teams/client_test.go go.mod go.sum
git commit -m "feat(teams): Graph REST client with paging and Retry-After"
```

---

## Task 5: Participant resolution (identity → store participant)

**Files:**
- Create: `internal/teams/participants.go`
- Test: `internal/teams/participants_test.go`

- [ ] **Step 1: Write the failing test**

```go
package teams

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestResolveParticipant_EmailUserUsesIDAsEmail(t *testing.T) {
	st := testutil.NewTestStore(t)
	r := newParticipantResolver(st, nil) // nil client => no /users lookup needed
	id := &Identity{ID: "alice@outlook.com", DisplayName: "Alice", UserIdentityType: "emailUser"}
	pid, err := r.resolve(context.Background(), id)
	require.NoError(t, err)
	assert.NotZero(t, pid)
}

func TestResolveParticipant_AADUserResolvesMail(t *testing.T) {
	st := testutil.NewTestStore(t)
	fake := &fakeUserLookup{mail: map[string]string{"obj-1": "bob@example.com"}}
	r := newParticipantResolver(st, fake)
	id := &Identity{ID: "obj-1", DisplayName: "Bob", UserIdentityType: "aadUser"}
	pid, err := r.resolve(context.Background(), id)
	require.NoError(t, err)
	assert.NotZero(t, pid)

	// second call hits the cache, not the lookup
	_, err = r.resolve(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, 1, fake.calls)
}

type fakeUserLookup struct {
	mail  map[string]string
	calls int
}

func (f *fakeUserLookup) GetUser(_ context.Context, id string) (*GraphUser, error) {
	f.calls++
	return &GraphUser{ID: id, Mail: f.mail[id]}, nil
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/teams/ -run 'TestResolveParticipant' -v`
Expected: FAIL — undefined symbols.

- [ ] **Step 3: Implement the resolver**

```go
package teams

import (
	"context"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

type userLookup interface {
	GetUser(ctx context.Context, id string) (*GraphUser, error)
}

type participantResolver struct {
	store  *store.Store
	lookup userLookup
	cache  map[string]int64 // identity id -> participant id
}

func newParticipantResolver(s *store.Store, lookup userLookup) *participantResolver {
	return &participantResolver{store: s, lookup: lookup, cache: map[string]int64{}}
}

// resolve maps a Graph identity to a store participant id, unifying with
// email identities when possible. Returns 0 for nil/unresolvable identities.
func (r *participantResolver) resolve(ctx context.Context, id *Identity) (int64, error) {
	if id == nil || id.ID == "" {
		return 0, nil
	}
	if pid, ok := r.cache[id.ID]; ok {
		return pid, nil
	}
	var pid int64
	var err error
	switch id.UserIdentityType {
	case "emailUser":
		pid, err = r.byEmail(id.ID, id.DisplayName)
	case "aadUser", "onPremiseAadUser":
		email := r.lookupMail(ctx, id.ID)
		if email != "" {
			pid, err = r.byEmail(email, id.DisplayName)
		} else {
			pid, err = r.store.EnsureParticipantByIdentifier("teams", id.ID, id.DisplayName)
		}
	default:
		// application/bot, anonymousGuest, skypeUser, ACS: no email
		pid, err = r.store.EnsureParticipantByIdentifier("teams", id.ID, id.DisplayName)
	}
	if err != nil {
		return 0, err
	}
	r.cache[id.ID] = pid
	return pid, nil
}

func (r *participantResolver) byEmail(email, displayName string) (int64, error) {
	domain := ""
	if at := strings.LastIndex(email, "@"); at >= 0 {
		domain = strings.ToLower(email[at+1:])
	}
	return r.store.EnsureParticipant(strings.ToLower(email), displayName, domain)
}

func (r *participantResolver) lookupMail(ctx context.Context, objectID string) string {
	if r.lookup == nil {
		return ""
	}
	u, err := r.lookup.GetUser(ctx, objectID)
	if err != nil || u == nil {
		return ""
	}
	if u.Mail != "" {
		return u.Mail
	}
	// UPN is often (not always) an SMTP address; accept it as best-effort.
	if strings.Contains(u.UserPrincipalName, "@") && !strings.Contains(u.UserPrincipalName, "#EXT#") {
		return u.UserPrincipalName
	}
	return ""
}
```

- [ ] **Step 4: Add the real `GetUser` to the client**

In `internal/teams/client.go` add:

```go
func (c *Client) GetUser(ctx context.Context, id string) (*GraphUser, error) {
	var u GraphUser
	if err := c.getJSON(ctx, "/users/"+id+"?$select=id,mail,userPrincipalName,displayName", &u); err != nil {
		return nil, err
	}
	return &u, nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/teams/ -run 'TestResolveParticipant' -v`
Expected: PASS.

- [ ] **Step 6: Format, vet, commit**

```bash
go fmt ./... && go vet ./internal/teams/
git add internal/teams/participants.go internal/teams/participants_test.go internal/teams/client.go
git commit -m "feat(teams): identity-to-participant resolution with /users cache"
```

---

## Task 6: Message mapping (chatMessage → store.Message + body text)

**Files:**
- Create: `internal/teams/mapping.go`
- Test: `internal/teams/mapping_test.go`

- [ ] **Step 1: Write the failing test**

```go
package teams

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestHTMLToText(t *testing.T) {
	got := htmlToText(`<p>Hello <at id="0">Bob</at> see <a href="x">link</a></p>`)
	assert.Contains(t, got, "Hello")
	assert.Contains(t, got, "Bob")
	assert.NotContains(t, got, "<p>")
}

func TestMapMessageBasics(t *testing.T) {
	gm := &ChatMessage{
		ID:                   "m1",
		CreatedDateTime:      time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC),
		LastModifiedDateTime: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC),
		Body:                 MessageBody{ContentType: "html", Content: "<p>hi there</p>"},
		Attachments:          []Attachment{{ContentType: "reference", ContentURL: "http://sp/f", Name: "f.docx"}},
	}
	msg, text := mapMessage(gm, 10, 20)
	assert.Equal(t, "teams", msg.MessageType)
	assert.Equal(t, "m1", msg.SourceMessageID)
	assert.True(t, msg.SentAt.Valid)
	assert.True(t, msg.HasAttachments)
	assert.Equal(t, 1, msg.AttachmentCount)
	assert.Equal(t, "hi there", text)
	assert.Contains(t, msg.Snippet.String, "hi there")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/teams/ -run 'TestHTMLToText|TestMapMessage' -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement mapping**

For HTML→text, check whether `internal/mime` or `internal/textutil` already exposes an HTML-to-text function (the research noted email HTML→text exists for MIME). Prefer reusing it; only add a local fallback if none is exported.

```go
package teams

import (
	"database/sql"
	"regexp"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

var tagRe = regexp.MustCompile(`<[^>]+>`)

// htmlToText is a minimal fallback. If internal/textutil exposes an
// HTML-to-text helper, call that instead and delete this.
func htmlToText(html string) string {
	s := tagRe.ReplaceAllString(html, " ")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	return strings.TrimSpace(regexp.MustCompile(`\s+`).ReplaceAllString(s, " "))
}

func snippet(text string) string {
	r := []rune(text)
	if len(r) > 100 {
		return string(r[:100])
	}
	return text
}

// mapMessage builds the store.Message and returns the derived body text.
func mapMessage(gm *ChatMessage, conversationID, sourceID int64) (store.Message, string) {
	text := gm.Body.Content
	if strings.EqualFold(gm.Body.ContentType, "html") {
		text = htmlToText(gm.Body.Content)
	}
	msg := store.Message{
		ConversationID:  conversationID,
		SourceID:        sourceID,
		SourceMessageID: gm.ID,
		MessageType:     "teams",
		SentAt:          sql.NullTime{Time: gm.CreatedDateTime, Valid: !gm.CreatedDateTime.IsZero()},
		ReceivedAt:      sql.NullTime{Time: gm.CreatedDateTime, Valid: !gm.CreatedDateTime.IsZero()},
		Snippet:         sql.NullString{String: snippet(text), Valid: text != ""},
		HasAttachments:  len(gm.Attachments) > 0,
		AttachmentCount: len(gm.Attachments),
	}
	if gm.Subject != "" {
		msg.Subject = sql.NullString{String: gm.Subject, Valid: true}
	}
	return msg, text
}

func conversationType(chatType string) string {
	if chatType == "oneOnOne" {
		return "direct_chat"
	}
	return "group_chat" // group, meeting
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/teams/ -run 'TestHTMLToText|TestMapMessage' -v`
Expected: PASS.

- [ ] **Step 5: Format, vet, commit**

```bash
go fmt ./... && go vet ./internal/teams/
git add internal/teams/mapping.go internal/teams/mapping_test.go
git commit -m "feat(teams): chatMessage to store.Message mapping"
```

---

## Task 7: SyncState (per-conversation cursors)

**Files:**
- Create: `internal/teams/syncstate.go`
- Test: `internal/teams/syncstate_test.go`

- [ ] **Step 1: Write the failing test**

```go
package teams

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncStateRoundTrip(t *testing.T) {
	s := NewSyncState()
	s.SetChatCursor("19:abc@thread.v2", "2026-01-01T00:00:00Z")
	s.SetChannelDelta("team1/chanA", "https://graph/delta?token=xyz")

	blob, err := s.Marshal()
	require.NoError(t, err)

	got, err := LoadSyncState(blob)
	require.NoError(t, err)
	assert.Equal(t, "2026-01-01T00:00:00Z", got.ChatCursor("19:abc@thread.v2"))
	assert.Equal(t, "https://graph/delta?token=xyz", got.ChannelDelta("team1/chanA"))
	assert.Equal(t, "", got.ChatCursor("unknown"))
}

func TestLoadSyncStateEmpty(t *testing.T) {
	got, err := LoadSyncState("")
	require.NoError(t, err)
	assert.Equal(t, "", got.ChatCursor("anything"))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/teams/ -run 'TestSyncState|TestLoadSyncState' -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement**

```go
package teams

import "encoding/json"

type SyncState struct {
	Chats    map[string]string `json:"chats"`    // chatID -> max lastModifiedDateTime (RFC3339)
	Channels map[string]string `json:"channels"` // "teamID/channelID" -> deltaLink
}

func NewSyncState() *SyncState {
	return &SyncState{Chats: map[string]string{}, Channels: map[string]string{}}
}

func LoadSyncState(blob string) (*SyncState, error) {
	s := NewSyncState()
	if blob == "" {
		return s, nil
	}
	if err := json.Unmarshal([]byte(blob), s); err != nil {
		return nil, err
	}
	if s.Chats == nil {
		s.Chats = map[string]string{}
	}
	if s.Channels == nil {
		s.Channels = map[string]string{}
	}
	return s, nil
}

func (s *SyncState) Marshal() (string, error) {
	b, err := json.Marshal(s)
	return string(b), err
}

func (s *SyncState) ChatCursor(chatID string) string      { return s.Chats[chatID] }
func (s *SyncState) SetChatCursor(chatID, cursor string)  { s.Chats[chatID] = cursor }
func (s *SyncState) ChannelDelta(key string) string       { return s.Channels[key] }
func (s *SyncState) SetChannelDelta(key, link string)     { s.Channels[key] = link }
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/teams/ -run 'TestSyncState|TestLoadSyncState' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go fmt ./... && git add internal/teams/syncstate.go internal/teams/syncstate_test.go
git commit -m "feat(teams): per-conversation sync-state cursors"
```

---

## Task 8: Client enumeration + message-fetch methods

Add the concrete Graph calls the importer needs. (These wrap `getJSON`/paging; keep them thin so the importer is testable against the fake server.)

**Files:**
- Modify: `internal/teams/client.go`
- Test: `internal/teams/client_test.go`

- [ ] **Step 1: Write the failing test (chats + channel messages)**

```go
func TestListChatsAndMessages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/me/chats/") && strings.Contains(r.URL.Path, "/messages"):
			w.Write([]byte(`{"value":[{"id":"m1","createdDateTime":"2025-01-01T00:00:00Z","body":{"contentType":"text","content":"hi"}}]}`))
		case r.URL.Path == "/me/chats":
			w.Write([]byte(`{"value":[{"id":"19:x@thread.v2","chatType":"oneOnOne"}]}`))
		default:
			http.Error(w, "no", 404)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, tokenFnCtx, 50)
	chats, err := c.ListChats(context.Background())
	require.NoError(t, err)
	require.Len(t, chats, 1)

	msgs, _, err := c.ListChatMessages(context.Background(), chats[0].ID, "", "")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "m1", msgs[0].ID)
}

func tokenFnCtx(context.Context) (string, error) { return "t", nil }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/teams/ -run 'TestListChatsAndMessages' -v`
Expected: FAIL — undefined methods.

- [ ] **Step 3: Implement enumeration + message methods**

Generalize paging to any element type with a small helper to avoid duplicating `getAllPages` per type:

```go
// pageThrough follows nextLink, decoding each page into []T via json, calling fn.
// Returns the terminal deltaLink.
func pageThrough[T any](ctx context.Context, c *Client, startURL string, fn func([]T)) (string, error) {
	url := startURL
	for {
		var page listResponse[T]
		if err := c.getJSON(ctx, url, &page); err != nil {
			return "", err
		}
		fn(page.Value)
		if page.NextLink != "" {
			url = page.NextLink
			continue
		}
		return page.DeltaLink, nil
	}
}

func (c *Client) ListChats(ctx context.Context) ([]Chat, error) {
	var out []Chat
	_, err := pageThrough[Chat](ctx, c, "/me/chats?$top=50", func(p []Chat) { out = append(out, p...) })
	return out, err
}

func (c *Client) ListJoinedTeams(ctx context.Context) ([]JoinedTeam, error) {
	var out []JoinedTeam
	_, err := pageThrough[JoinedTeam](ctx, c, "/me/joinedTeams", func(p []JoinedTeam) { out = append(out, p...) })
	return out, err
}

func (c *Client) ListChannels(ctx context.Context, teamID string) ([]Channel, error) {
	var out []Channel
	_, err := pageThrough[Channel](ctx, c, "/teams/"+teamID+"/channels", func(p []Channel) { out = append(out, p...) })
	return out, err
}

// ListChatMessages: pass sinceISO for incremental (empty = full backfill).
// Returns messages and the nextURL is handled internally (full drain).
func (c *Client) ListChatMessages(ctx context.Context, chatID, sinceISO, _ string) ([]ChatMessage, string, error) {
	url := "/me/chats/" + chatID + "/messages?$top=50"
	if sinceISO != "" {
		url += "&$filter=lastModifiedDateTime%20gt%20" + sinceISO + "&$orderby=lastModifiedDateTime%20desc"
	}
	var out []ChatMessage
	_, err := pageThrough[ChatMessage](ctx, c, url, func(p []ChatMessage) { out = append(out, p...) })
	return out, "", err
}

// ChannelMessagesDelta drives the delta endpoint (or a stored deltaLink) to
// completion, returning all messages and the new deltaLink.
func (c *Client) ChannelMessagesDelta(ctx context.Context, teamID, channelID, deltaLink string) ([]ChatMessage, string, error) {
	start := deltaLink
	if start == "" {
		start = "/teams/" + teamID + "/channels/" + channelID + "/messages/delta"
	}
	var out []ChatMessage
	newDelta, err := pageThrough[ChatMessage](ctx, c, start, func(p []ChatMessage) { out = append(out, p...) })
	return out, newDelta, err
}

func (c *Client) ListChannelMessages(ctx context.Context, teamID, channelID string) ([]ChatMessage, error) {
	var out []ChatMessage
	_, err := pageThrough[ChatMessage](ctx, c, "/teams/"+teamID+"/channels/"+channelID+"/messages?$top=50", func(p []ChatMessage) { out = append(out, p...) })
	return out, err
}

func (c *Client) ListReplies(ctx context.Context, teamID, channelID, messageID string) ([]ChatMessage, error) {
	var out []ChatMessage
	_, err := pageThrough[ChatMessage](ctx, c, "/teams/"+teamID+"/channels/"+channelID+"/messages/"+messageID+"/replies", func(p []ChatMessage) { out = append(out, p...) })
	return out, err
}
```

> Now the original `getAllPages(...func([]Chat))` from Task 4 is redundant — replace it (and update `TestClientGetJSONPaging`/`TestClientRetryAfter`) to call `pageThrough[Chat](ctx, c, url, fn)`. Keep one paging implementation.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/teams/ -run 'TestClient|TestListChats' -v`
Expected: PASS.

- [ ] **Step 5: Format, vet, commit**

```bash
go fmt ./... && go vet ./internal/teams/
git add internal/teams/client.go internal/teams/client_test.go
git commit -m "feat(teams): Graph enumeration and message-fetch methods"
```

---

## Task 9: Importer — chats end-to-end

This is the orchestration core. Test it against a fake Graph server through the real store.

**Files:**
- Create: `internal/teams/importer.go`
- Test: `internal/teams/importer_test.go`

- [ ] **Step 1: Write the failing end-to-end test (chats)**

```go
package teams

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func fakeChatGraph(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			w.Write([]byte(`{"value":[{"id":"19:x@thread.v2","chatType":"oneOnOne","topic":"DM"}]}`))
		case strings.Contains(r.URL.Path, "/messages"):
			w.Write([]byte(`{"value":[
			  {"id":"m1","createdDateTime":"2025-01-01T00:00:00Z","lastModifiedDateTime":"2025-01-01T00:00:00Z",
			   "from":{"user":{"id":"alice@outlook.com","displayName":"Alice","userIdentityType":"emailUser"}},
			   "body":{"contentType":"text","content":"hello world"}}
			]}`))
		default:
			http.Error(w, "404", 404)
		}
	}))
}

func TestImportChatsEndToEnd(t *testing.T) {
	srv := fakeChatGraph(t)
	defer srv.Close()
	st := testutil.NewTestStore(t)

	c := NewClient(srv.URL, tokenFnCtx, 50)
	imp := NewImporter(st, c)
	sum, err := imp.Import(context.Background(), ImportOptions{Email: "me@example.com", IncludeChannels: false})
	require.NoError(t, err)
	assert.EqualValues(t, 1, sum.ChatsProcessed)
	assert.EqualValues(t, 1, sum.MessagesAdded)

	// message persisted and FTS-searchable
	var cnt int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='teams'`).Scan(&cnt))
	assert.Equal(t, 1, cnt)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/teams/ -run 'TestImportChatsEndToEnd' -v`
Expected: FAIL — `NewImporter`/`Import` undefined.

- [ ] **Step 3: Implement the importer (chats path)**

```go
package teams

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

type Importer struct {
	store  *store.Store
	client *Client
	res    *participantResolver
}

func NewImporter(s *store.Store, c *Client) *Importer {
	return &Importer{store: s, client: c, res: newParticipantResolver(s, c)}
}

func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (*ImportSummary, error) {
	start := time.Now()
	src, err := imp.store.GetOrCreateSource(sourceTypeTeams, opts.Email)
	if err != nil {
		return nil, err
	}
	sum := &ImportSummary{SourceID: src.ID}

	// load prior cursors
	state := NewSyncState()
	if prev, err := imp.store.GetLastSuccessfulSync(src.ID); err == nil && prev != nil && prev.CursorAfter.Valid {
		if s, err := LoadSyncState(prev.CursorAfter.String); err == nil {
			state = s
		}
	}

	syncID, err := imp.store.StartSync(src.ID, "teams")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = imp.store.FailSync(syncID, err.Error())
		}
	}()

	if err = imp.syncChats(ctx, src.ID, opts, state, sum); err != nil {
		return sum, err
	}
	if opts.IncludeChannels {
		if err = imp.syncChannels(ctx, src.ID, opts, state, sum); err != nil {
			return sum, err
		}
	}

	blob, _ := state.Marshal()
	if err = imp.store.CompleteSync(syncID, blob); err != nil {
		return sum, err
	}
	sum.Duration = time.Since(start)
	return sum, nil
}

const sourceTypeTeams = "teams"

func (imp *Importer) syncChats(ctx context.Context, sourceID int64, opts ImportOptions, state *SyncState, sum *ImportSummary) error {
	chats, err := imp.client.ListChats(ctx)
	if err != nil {
		return err
	}
	for _, ch := range chats {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		convID, err := imp.store.EnsureConversationWithType(sourceID, ch.ID, conversationType(ch.ChatType), ch.Topic)
		if err != nil {
			return err
		}
		since := state.ChatCursor(ch.ID)
		msgs, _, err := imp.client.ListChatMessages(ctx, ch.ID, since, "")
		if err != nil {
			sum.Errors++
			continue
		}
		maxCursor := since
		for i := range msgs {
			gm := &msgs[i]
			added, err := imp.persistMessage(ctx, convID, sourceID, gm, sum)
			if err != nil {
				return err
			}
			if added {
				sum.MessagesAdded++
			} else {
				sum.MessagesUpdated++
			}
			sum.MessagesProcessed++
			if iso := gm.LastModifiedDateTime.UTC().Format(time.RFC3339); iso > maxCursor {
				maxCursor = iso
			}
		}
		if maxCursor != "" {
			state.SetChatCursor(ch.ID, maxCursor)
		}
		sum.ChatsProcessed++
	}
	return nil
}

// persistMessage writes a single message via the granular store path.
// Returns true if newly inserted.
func (imp *Importer) persistMessage(ctx context.Context, convID, sourceID int64, gm *ChatMessage, sum *ImportSummary) (bool, error) {
	// deleted tombstone
	if gm.DeletedDateTime != nil {
		_ = imp.store.MarkMessageDeleted(sourceID, gm.ID)
		return false, nil
	}
	msg, text := mapMessage(gm, convID, sourceID)
	if gm.From != nil {
		pid, err := imp.res.resolve(ctx, identityOf(gm.From))
		if err != nil {
			return false, err
		}
		if pid != 0 {
			msg.SenderID = sql.NullInt64{Int64: pid, Valid: true}
		}
	}
	messageID, err := imp.store.UpsertMessage(&msg)
	if err != nil {
		return false, err
	}
	bodyHTML := sql.NullString{}
	if gm.Body.ContentType == "html" {
		bodyHTML = sql.NullString{String: gm.Body.Content, Valid: true}
	}
	if err := imp.store.UpsertMessageBody(messageID, sql.NullString{String: text, Valid: text != ""}, bodyHTML); err != nil {
		return false, err
	}
	if raw, err := json.Marshal(gm); err == nil {
		_ = imp.store.UpsertMessageRawWithFormat(messageID, raw, "teams_json")
	}
	senderAddr := ""
	if gm.From != nil && identityOf(gm.From) != nil {
		senderAddr = identityOf(gm.From).DisplayName
	}
	_ = imp.store.UpsertFTS(messageID, msg.Subject.String, text, senderAddr, "", "")

	// reactions
	for _, rc := range gm.Reactions {
		pid, _ := imp.res.resolve(ctx, identityOf(rc.User))
		if pid != 0 {
			if err := imp.store.UpsertReaction(messageID, pid, rc.ReactionType, rc.ReactionType, rc.CreatedDateTime); err == nil {
				sum.ReactionsAdded++
			}
		}
	}
	// shared-file attachment links (contentType "reference"); inline images in Task 11
	for _, att := range gm.Attachments {
		if att.ContentType == "reference" {
			_ = imp.store.UpsertAttachment(messageID, att.Name, "", att.ContentURL, "", 0)
			sum.AttachmentsFound++
		}
	}
	return true, nil
}

func identityOf(set *IdentitySet) *Identity {
	if set == nil {
		return nil
	}
	if set.User != nil {
		return set.User
	}
	return set.Application
}
```

> **Open detail to confirm while implementing:** `UpsertMessage` returns only an id, not an inserted/updated flag. If distinguishing added-vs-updated matters for the summary, check whether the store exposes `RowsAffected` or query existence first; otherwise treat all as "added" and drop `MessagesUpdated`. Don't block the task on this — pick the simplest correct behavior.

> `MarkMessageDeleted(sourceID, sourceMessageID)` exists (`messages.go:766`). Confirm the exact signature; adjust if it differs.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/teams/ -run 'TestImportChatsEndToEnd' -v`
Expected: PASS.

- [ ] **Step 5: Format, vet, full package test, commit**

```bash
go fmt ./... && go vet ./internal/teams/
go test ./internal/teams/ -v
git add internal/teams/importer.go internal/teams/importer_test.go
git commit -m "feat(teams): importer chats path end-to-end"
```

---

## Task 10: Importer — channels (delta + backfill + replies)

**Files:**
- Modify: `internal/teams/importer.go`
- Test: `internal/teams/importer_test.go`

- [ ] **Step 1: Write the failing test (channels via delta)**

```go
func fakeChannelGraph(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/joinedTeams":
			w.Write([]byte(`{"value":[{"id":"team1","displayName":"Acme"}]}`))
		case strings.HasSuffix(r.URL.Path, "/channels"):
			w.Write([]byte(`{"value":[{"id":"chanA","displayName":"General","membershipType":"standard"}]}`))
		case strings.HasSuffix(r.URL.Path, "/messages/delta"):
			w.Write([]byte(`{"value":[
			  {"id":"c1","createdDateTime":"2025-02-01T00:00:00Z","lastModifiedDateTime":"2025-02-01T00:00:00Z",
			   "body":{"contentType":"text","content":"channel post"}}
			],"@odata.deltaLink":"` + "http://" + r.Host + `/delta?token=next"}`))
		default:
			http.Error(w, "404", 404)
		}
	}))
}

func TestImportChannelsEndToEnd(t *testing.T) {
	srv := fakeChannelGraph(t)
	defer srv.Close()
	st := testutil.NewTestStore(t)

	imp := NewImporter(st, NewClient(srv.URL, tokenFnCtx, 50))
	sum, err := imp.Import(context.Background(), ImportOptions{Email: "me@example.com", IncludeChannels: true})
	require.NoError(t, err)
	assert.EqualValues(t, 1, sum.ChannelsProcessed)
	assert.EqualValues(t, 1, sum.MessagesAdded)

	// delta link persisted for next run
	src, _ := st.GetOrCreateSource("teams", "me@example.com")
	prev, _ := st.GetLastSuccessfulSync(src.ID)
	state, _ := LoadSyncState(prev.CursorAfter.String)
	assert.Contains(t, state.ChannelDelta("team1/chanA"), "token=next")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/teams/ -run 'TestImportChannelsEndToEnd' -v`
Expected: FAIL — `syncChannels` undefined / no channels processed.

- [ ] **Step 3: Implement `syncChannels`**

```go
func (imp *Importer) syncChannels(ctx context.Context, sourceID int64, opts ImportOptions, state *SyncState, sum *ImportSummary) error {
	teams, err := imp.client.ListJoinedTeams(ctx)
	if err != nil {
		return err
	}
	for _, tm := range teams {
		channels, err := imp.client.ListChannels(ctx, tm.ID)
		if err != nil {
			sum.Errors++
			continue
		}
		for _, chn := range channels {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			title := tm.DisplayName + " / " + chn.DisplayName
			convID, err := imp.store.EnsureConversationWithType(sourceID, tm.ID+"/"+chn.ID, "channel", title)
			if err != nil {
				return err
			}
			key := tm.ID + "/" + chn.ID
			prevDelta := state.ChannelDelta(key)

			var msgs []ChatMessage
			var newDelta string
			if prevDelta == "" {
				// first run: backfill roots + replies, then prime delta
				roots, err := imp.client.ListChannelMessages(ctx, tm.ID, chn.ID)
				if err != nil {
					sum.Errors++
					continue
				}
				msgs = append(msgs, roots...)
				for i := range roots {
					replies, err := imp.client.ListReplies(ctx, tm.ID, chn.ID, roots[i].ID)
					if err == nil {
						msgs = append(msgs, replies...)
					}
				}
				// prime the delta cursor for subsequent runs
				_, newDelta, _ = imp.client.ChannelMessagesDelta(ctx, tm.ID, chn.ID, "")
			} else {
				// incremental: drive delta from stored link, with fallback
				msgs, newDelta, err = imp.client.ChannelMessagesDelta(ctx, tm.ID, chn.ID, prevDelta)
				if err != nil {
					// delta-token rot (400/410): full re-page + dedupe via upsert
					roots, rerr := imp.client.ListChannelMessages(ctx, tm.ID, chn.ID)
					if rerr != nil {
						sum.Errors++
						continue
					}
					msgs = roots
					for i := range roots {
						if replies, rerr := imp.client.ListReplies(ctx, tm.ID, chn.ID, roots[i].ID); rerr == nil {
							msgs = append(msgs, replies...)
						}
					}
					_, newDelta, _ = imp.client.ChannelMessagesDelta(ctx, tm.ID, chn.ID, "")
				}
			}

			if err := imp.persistChannelMessages(ctx, convID, sourceID, msgs, sum); err != nil {
				return err
			}
			if newDelta != "" {
				state.SetChannelDelta(key, newDelta)
			}
			sum.ChannelsProcessed++
		}
	}
	return nil
}

func (imp *Importer) persistChannelMessages(ctx context.Context, convID, sourceID int64, msgs []ChatMessage, sum *ImportSummary) error {
	// pass 1: roots and replies whose parent already exists
	for i := range msgs {
		gm := &msgs[i]
		added, err := imp.persistMessage(ctx, convID, sourceID, gm, sum)
		if err != nil {
			return err
		}
		if added {
			sum.MessagesAdded++
		} else {
			sum.MessagesUpdated++
		}
		sum.MessagesProcessed++
		// set reply linkage if parent is known
		if gm.ReplyToID != "" {
			_ = imp.store.SetReplyTo(sourceID, gm.ID, gm.ReplyToID)
		}
	}
	return nil
}
```

> **Reply linkage:** the plan calls `store.SetReplyTo(sourceID, childSourceMsgID, parentSourceMsgID)` to populate `messages.reply_to_message_id` by resolving the parent's `source_message_id` to its internal id. The research did NOT find such a method — **verify** whether one exists; if not, add a small store method in this task:
>
> ```go
> // internal/store/messages.go
> func (s *Store) SetReplyTo(sourceID int64, childSourceMessageID, parentSourceMessageID string) error {
> 	_, err := s.db.Exec(s.dialect.Rebind(`
> 		UPDATE messages SET reply_to_message_id =
> 		  (SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?)
> 		WHERE source_id = ? AND source_message_id = ?`),
> 		sourceID, parentSourceMessageID, sourceID, childSourceMessageID)
> 	return err
> }
> ```
> Confirm the dialect/Rebind helper name against existing store methods before writing. Because backfill inserts roots before replies (roots list first, replies appended), the parent is present when the reply is processed. For delta runs where a reply may arrive before its root, the UPDATE simply sets NULL and a later run reconciles — acceptable.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/teams/ -run 'TestImportChannelsEndToEnd|TestImportChatsEndToEnd' -v`
Expected: PASS.

- [ ] **Step 5: Format, vet, full package test, commit**

```bash
go fmt ./... && go vet ./...
go test ./internal/teams/ ./internal/store/ -v
git add internal/teams/importer.go internal/teams/importer_test.go internal/store/messages.go
git commit -m "feat(teams): importer channels path (delta + backfill + replies)"
```

---

## Task 11: Inline images (hostedContents) download

Inline images need a separate `GET .../hostedContents/{id}/$value` (bytes are null on the message read). Detect them by the `hostedContents/{id}/$value` URL pattern in the body HTML and store into content-addressed storage.

**Files:**
- Modify: `internal/teams/client.go` (raw `$value` GET), `internal/teams/importer.go`
- Test: `internal/teams/importer_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestInlineImageDownloaded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/hostedContents/") && strings.HasSuffix(r.URL.Path, "/$value"):
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("PNGDATA"))
		case r.URL.Path == "/me/chats":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"value":[{"id":"19:x@thread.v2","chatType":"oneOnOne"}]}`))
		case strings.Contains(r.URL.Path, "/messages"):
			w.Header().Set("Content-Type", "application/json")
			body := `<div><img src="https://graph.microsoft.com/v1.0/chats/19:x@thread.v2/messages/m1/hostedContents/1/$value"></div>`
			w.Write([]byte(`{"value":[{"id":"m1","createdDateTime":"2025-01-01T00:00:00Z","body":{"contentType":"html","content":` + jsonString(body) + `}}]}`))
		default:
			http.Error(w, "404", 404)
		}
	}))
	defer srv.Close()
	st := testutil.NewTestStore(t)
	dir := t.TempDir()

	imp := NewImporter(st, NewClient(srv.URL, tokenFnCtx, 50))
	sum, err := imp.Import(context.Background(), ImportOptions{Email: "me@example.com", AttachmentsDir: dir})
	require.NoError(t, err)
	assert.EqualValues(t, 1, sum.InlineImagesCopied)
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }
```

(Add `import "encoding/json"` to the test file if not present.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/teams/ -run 'TestInlineImageDownloaded' -v`
Expected: FAIL.

- [ ] **Step 3: Implement raw fetch + extraction + storage**

Add to `client.go`:

```go
func (c *Client) GetRaw(ctx context.Context, url string) ([]byte, error) {
	return c.get(ctx, url)
}
```

Reuse the existing content-addressed attachment storage helper. The research found `export.StoreAttachmentFile` and `store.UpsertAttachment(messageID, filename, mimeType, storagePath, contentHash, size)`. In `importer.go` add inline-image handling inside `persistMessage` (after body persistence), guarded by `opts.AttachmentsDir != ""`. Thread `opts` into `persistMessage` (change its signature to accept `opts ImportOptions` or store `imp.attachmentsDir`).

```go
var hostedRe = regexp.MustCompile(`https://[^"'\s)]+/hostedContents/[^"'\s)]+/\$value`)

func (imp *Importer) downloadInlineImages(ctx context.Context, messageID int64, bodyHTML, attachmentsDir string, sum *ImportSummary) {
	if attachmentsDir == "" {
		return
	}
	for _, url := range hostedRe.FindAllString(bodyHTML, -1) {
		data, err := imp.client.GetRaw(ctx, url)
		if err != nil || len(data) == 0 {
			sum.Errors++
			continue
		}
		hash := sha256Hex(data)
		storagePath, err := storeContentAddressed(attachmentsDir, hash, data) // mirror export.StoreAttachmentFile
		if err != nil {
			sum.Errors++
			continue
		}
		if err := imp.store.UpsertAttachment(messageID, hash, "", storagePath, hash, len(data)); err == nil {
			sum.InlineImagesCopied++
		}
	}
}
```

> Implement `sha256Hex` and `storeContentAddressed` by reusing existing helpers: the research cited `internal/export/store_attachment.go` (`export.StoreAttachmentFile`) and content-addressing at `ab/abcd...`. Prefer calling those exported helpers over re-implementing; only add thin local wrappers if the exported signature doesn't fit. Confirm the helper name/signature while implementing.

Call `imp.downloadInlineImages(ctx, messageID, gm.Body.Content, attachmentsDir, sum)` in `persistMessage` when `gm.Body.ContentType == "html"`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/teams/ -run 'TestInlineImageDownloaded' -v`
Expected: PASS.

- [ ] **Step 5: Format, vet, full package, commit**

```bash
go fmt ./... && go vet ./...
go test ./internal/teams/ -v
git add internal/teams/client.go internal/teams/importer.go internal/teams/importer_test.go
git commit -m "feat(teams): download inline hosted-content images"
```

---

## Task 12: `add-account --teams` CLI command

Template: `cmd/msgvault/cmd/addo365.go` (gates on `cfg.Microsoft.ClientID`, builds the manager, calls `Authorize`, creates the source). Extend `add-account` with a `--teams` flag OR add a dedicated command — this plan uses a dedicated `add_teams.go` to avoid entangling the Gmail/IMAP add-account flow, then notes the `--teams` alias as optional.

**Files:**
- Create: `cmd/msgvault/cmd/add_teams.go`

- [ ] **Step 1: Implement the command**

```go
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/store"
)

var addTeamsCmd = &cobra.Command{
	Use:   "add-teams <email>",
	Short: "Authorize Microsoft Teams (delegated Graph) for an account",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		email := args[0]
		if cfg.Microsoft.ClientID == "" {
			return fmt.Errorf("Microsoft client_id not configured — set [microsoft].client_id in config.toml")
		}
		s, err := store.Open(cfg.DatabaseDSN())
		if err != nil {
			return err
		}
		defer s.Close()
		if err := s.InitSchema(); err != nil {
			return err
		}
		if err := runStartupMigrationsForIngest(s); err != nil {
			return err
		}

		mgr := microsoft.NewGraphManager(cfg.Microsoft.ClientID, cfg.Microsoft.EffectiveTenantID(), cfg.TokensDir(), logger())
		if err := mgr.Authorize(cmd.Context(), email); err != nil {
			return fmt.Errorf("authorize Teams: %w", err)
		}

		src, err := s.GetOrCreateSource(sourceTypeTeams, email)
		if err != nil {
			return err
		}
		_ = s.UpdateSourceDisplayName(src.ID, email)
		if err := runPostSourceCreateMigrations(s); err != nil {
			return err
		}
		fmt.Printf("Teams authorized for %s. Run: msgvault sync-teams %s\n", email, email)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(addTeamsCmd)
}
```

> Confirm helper names against the codebase while implementing: `logger()` (or how `addo365.go` obtains its `*slog.Logger`), `cfg.Microsoft.EffectiveTenantID()` (research cited it), `cfg.TokensDir()`, `UpdateSourceDisplayName`, `runStartupMigrationsForIngest`, `runPostSourceCreateMigrations`. Copy exact usage from `addo365.go`.

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: compiles.

- [ ] **Step 3: Manual smoke (documented, not automated)**

Run: `./msgvault add-teams you@yourtenant.com` and complete the browser consent.
Expected: writes `~/.msgvault/tokens/teams_you@yourtenant.com.json` and prints the next-step hint. (Requires the Entra app from the spec.)

- [ ] **Step 4: Format, vet, commit**

```bash
go fmt ./... && go vet ./...
git add cmd/msgvault/cmd/add_teams.go
git commit -m "feat(teams): add-teams OAuth command"
```

---

## Task 13: `sync-teams` CLI command

Template: `import_messenger.go` (store open, signal handling, summary, `rebuildCacheAfterWrite`). Wire the Graph token source from the `GraphManager`.

**Files:**
- Create: `cmd/msgvault/cmd/sync_teams.go`

- [ ] **Step 1: Implement the command**

```go
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/teams"
)

var (
	syncTeamsNoChannels bool
	syncTeamsLimit      int
)

var syncTeamsCmd = &cobra.Command{
	Use:   "sync-teams <email>",
	Short: "Sync Microsoft Teams chats and channels (full or incremental)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		email := args[0]
		s, err := store.Open(cfg.DatabaseDSN())
		if err != nil {
			return err
		}
		defer s.Close()
		if err := s.InitSchema(); err != nil {
			return err
		}
		if err := runStartupMigrations(s); err != nil {
			return err
		}

		mgr := microsoft.NewGraphManager(cfg.Microsoft.ClientID, cfg.Microsoft.EffectiveTenantID(), cfg.TokensDir(), logger())
		tokenFn, err := mgr.TokenSource(cmd.Context(), email)
		if err != nil {
			return err
		}

		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sigChan)
		go func() { <-sigChan; cancel() }()

		client := teams.NewClient("https://graph.microsoft.com/v1.0", teams.TokenFunc(tokenFn), float64(cfg.Sync.RateLimitQPS))
		imp := teams.NewImporter(s, client)
		sum, err := imp.Import(ctx, teams.ImportOptions{
			Email:           email,
			AttachmentsDir:  cfg.AttachmentsDir(),
			IncludeChannels: !syncTeamsNoChannels,
			Limit:           syncTeamsLimit,
		})
		if ctx.Err() != nil {
			fmt.Println("Interrupted — re-run sync-teams to resume.")
			rebuildCacheAfterWrite(cfg.DatabaseDSN())
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Printf("Teams sync complete for %s in %s: %d chats, %d channels, %d messages added (%d reactions, %d attachments, %d inline images, %d errors)\n",
			email, sum.Duration.Round(time.Second), sum.ChatsProcessed, sum.ChannelsProcessed, sum.MessagesAdded, sum.ReactionsAdded, sum.AttachmentsFound, sum.InlineImagesCopied, sum.Errors)
		rebuildCacheAfterWrite(cfg.DatabaseDSN())
		return nil
	},
}

func init() {
	syncTeamsCmd.Flags().BoolVar(&syncTeamsNoChannels, "no-channels", false, "sync chats only (skip team channels)")
	syncTeamsCmd.Flags().IntVar(&syncTeamsLimit, "limit", 0, "max messages per conversation (0 = no limit)")
	rootCmd.AddCommand(syncTeamsCmd)
}
```

> Confirm: `cfg.Sync.RateLimitQPS` field name/type, `rebuildCacheAfterWrite` signature (research showed it takes a db path), `runStartupMigrations`, and that `teams.TokenFunc(tokenFn)` type-converts cleanly (both are `func(context.Context)(string,error)`). Adjust to match exact codebase names.

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: compiles.

- [ ] **Step 3: Manual smoke**

Run: `./msgvault sync-teams you@yourtenant.com --limit 50`
Expected: prints a summary; `./msgvault tui` shows Teams messages searchable.

- [ ] **Step 4: Format, vet, commit**

```bash
go fmt ./... && go vet ./...
git add cmd/msgvault/cmd/sync_teams.go
git commit -m "feat(teams): sync-teams command"
```

---

## Task 14: Scheduler integration (`serve`)

Add a `"teams"` case to the daemon's per-source-type dispatch (`serve.go:runScheduledSync` switch ~`:418`, and `findScheduledSyncSource` ~`:462-469`).

**Files:**
- Modify: `cmd/msgvault/cmd/serve.go`

- [ ] **Step 1: Add the teams case**

In `runScheduledSync`'s `switch sourceType` block, add before `default`:

```go
	case sourceTypeTeams:
		err = runScheduledTeamsSync(ctx, email, s)
```

Add the helper (mirrors `runScheduledGmailSync` but returns only error since Teams has its own summary type):

```go
func runScheduledTeamsSync(ctx context.Context, email string, s *store.Store) error {
	mgr := microsoft.NewGraphManager(cfg.Microsoft.ClientID, cfg.Microsoft.EffectiveTenantID(), cfg.TokensDir(), logger())
	tokenFn, err := mgr.TokenSource(ctx, email)
	if err != nil {
		return err
	}
	client := teams.NewClient("https://graph.microsoft.com/v1.0", teams.TokenFunc(tokenFn), float64(cfg.Sync.RateLimitQPS))
	_, err = teams.NewImporter(s, client).Import(ctx, teams.ImportOptions{
		Email:           email,
		AttachmentsDir:  cfg.AttachmentsDir(),
		IncludeChannels: true,
	})
	return err
}
```

Also add `sourceTypeTeams` to the inner switch in `findScheduledSyncSource` so the daemon recognizes Teams sources as schedulable.

> The existing switch's `default` returns "only gmail and imap" — update that message to include teams, and confirm whether `runScheduledSync` shares a `summary` variable typed `*gmail.SyncSummary` (research said the gmail/imap helpers return that). The Teams helper deliberately returns only `error`; assign via a separate branch that doesn't touch `summary`, or refactor the shared variable. Keep the change minimal.

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: compiles.

- [ ] **Step 3: Verify the daemon recognizes teams sources**

Run: `go vet ./cmd/...` and a quick `./msgvault serve --help`
Expected: no errors. (Full daemon e2e requires a live token; the importer is already covered by Task 9/10 tests.)

- [ ] **Step 4: Format, vet, commit**

```bash
go fmt ./... && go vet ./...
git add cmd/msgvault/cmd/serve.go
git commit -m "feat(teams): schedule Teams syncs in serve daemon"
```

---

## Task 15: Full suite + docs

**Files:**
- Modify: `CLAUDE.md` (Quick Commands), `docs/superpowers/specs/2026-06-18-teams-ingestion-design.md` (status → implemented)

- [ ] **Step 1: Run the whole test suite**

Run: `make test`
Expected: PASS (SQLite). If a Postgres instance is available: `make test-pg`.

- [ ] **Step 2: Lint**

Run: `make lint-ci`
Expected: clean.

- [ ] **Step 3: Document the commands**

Add to `CLAUDE.md` under Quick Commands:

```bash
./msgvault add-teams you@tenant.com         # Authorize Teams (delegated Graph)
./msgvault sync-teams you@tenant.com        # Sync Teams chats + channels
./msgvault sync-teams you@tenant.com --no-channels --limit 50
```

- [ ] **Step 4: Commit**

```bash
go fmt ./...
git add CLAUDE.md docs/superpowers/specs/2026-06-18-teams-ingestion-design.md
git commit -m "docs(teams): document Teams commands; mark spec implemented"
```

---

## Self-review notes (already applied)

- **Spec coverage:** chats (Task 9), channels std+private (Task 10, private channels are ordinary channel objects), meeting chats (Task 9 — `meeting` chatType → group_chat), rich text/body_html + FTS (Tasks 6/9), reactions (Task 9), shared-file links (Task 9), inline images (Task 11), AAD→email participant resolution (Task 5), incremental cursors chats-vs-channels (Tasks 7/9/10), OAuth independence/IMAP-blocker (Task 2), CLI (Tasks 12/13), scheduler (Task 14). Transcripts intentionally excluded (separate spec).
- **Unverified store helpers flagged for confirmation during implementation:** `SetReplyTo` (likely new, code provided), added-vs-updated flag from `UpsertMessage`, exact private helper names in `microsoft/oauth.go`, `export.StoreAttachmentFile`/content-address helper signature, `cfg.Microsoft.EffectiveTenantID`, `rebuildCacheAfterWrite`. Each task names the thing to confirm and gives a working fallback.
- **Type consistency:** `TokenFunc`, `SyncState` accessors, `ImportOptions`/`ImportSummary` field names used consistently across tasks (note the ASCII fix for `InlineImagesCopied` in Task 3).
- **No `is_edited`/`mention` assumptions:** edits handled via re-upsert on `lastModifiedDateTime`; deletes via `MarkMessageDeleted`. `recipient_type='mention'` is not written in this plan (mentions are captured in raw JSON; promoting them to recipient rows is a deferred enhancement) — avoids relying on schema-only behavior.
