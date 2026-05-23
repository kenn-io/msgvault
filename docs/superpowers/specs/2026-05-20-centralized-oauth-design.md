# Centralized verified Google OAuth client for msgvault

Status: Design, ready for review
Date: 2026-05-20

## Summary

Replace the bring-your-own (BYO) Google Cloud OAuth setup that every new user
must complete today with a centralized, Google-verified OAuth client baked into
the msgvault binary. New users go from "create a Cloud project, configure the
consent screen, register yourself as a test user, download client_secret.json,
edit config.toml" to "run `msgvault add-account you@gmail.com`". BYO,
named-apps, and the service-account path all remain as escape valves for
Workspace orgs, privacy-conscious users, and high-volume mailboxes.

## Decisions resolved during brainstorming

| Decision | Choice |
|---|---|
| Strategy | Eliminate the user-owned OAuth app step entirely via a project-owned verified client |
| Verified scopes | `gmail.readonly` + `gmail.modify` + `mail.google.com/` |
| BYO path | Stay as a peer option (hybrid-full): BYO + named apps + per-account binding all keep working |
| Embed mechanism | gh-style hybrid: package-level defaults in source, overridable via `-ldflags -X` at build time |
| Microsoft 365 | Out of scope for this design; tackled in a separate effort later |
| Rollout | Land in `main` and ship continuously while Google verification runs in parallel. BYO is the documented fallback for anyone who hits the 100-user lifetime cap before verification completes. |

## Goals

- A clean-install user can run `msgvault add-account you@gmail.com` with no
  prior Google Cloud Console work and reach the browser consent screen.
- BYO config (`[oauth] client_secrets`, `[oauth.apps.*]`,
  `[oauth] service_account_key`) continues to work unchanged.
- The embedded credentials live as package variables, default to a known value
  in source, and can be overridden at build time via Go `-ldflags`.
- The verified client requests scopes equivalent to today's combined
  `oauth.Scopes` + `oauth.ScopesDeletion` so the centralized path supports
  every existing feature including permanent delete.
- Dead code that only existed to handle the "no OAuth credentials configured"
  first-run cliff is removed.
- Documentation reframes setup: centralized is the default path, BYO is an
  "advanced" footnote.

## Non-goals

- Microsoft 365 / Graph centralization. The `[microsoft]` config block,
  `add-o365` command, and the existing BYO Azure flow are untouched.
- IMAP changes. `add-imap` and its app-password flow are unrelated.
- The service-account path. It already serves Workspace admins via
  domain-wide delegation and stays as-is.
- Removing any BYO surface (named apps, `--oauth-app` flag, per-account
  binding column, `TokenMatchesClient`, scope-escalation prompt for BYO
  users). All of it stays for the mixed personal+Workspace user case.

## Background

### Current setup pain

A new user today must, before they can sync a single message:

1. Create a Google Cloud project in the console.
2. Enable the Gmail API.
3. Configure the OAuth consent screen, including `gmail.modify` and listing
   themselves as a test user.
4. Create an OAuth client ID (Desktop type).
5. Download `client_secret.json`.
6. Create or edit `~/.msgvault/config.toml` to point `[oauth] client_secrets`
   at the file.
7. Live with Google's 7-day refresh-token expiry that applies to unverified
   restricted-scope clients.

Steps 1 through 6 take 5 to 15 minutes for someone who has never used Cloud
Console and is hostile to anyone who has not.

### Existing infrastructure that already supports the new design

- `internal/oauth/oauth.go` already separates `NewManager` (default scopes,
  reads a `client_secret.json` path) from `NewManagerWithScopes` (custom
  scopes). The flow factor we are adding is "no client_secret.json path,
  build the `oauth2.Config` from embedded values".
- The Makefile already wires version metadata into the binary via `-ldflags`
  (`Makefile:9-11`). We extend the same pattern with two more `-X` entries.
- The release workflow at `.github/workflows/release.yml` is where production
  credentials get injected from GitHub Actions Secrets.

### Why hybrid-full and not "drop BYO entirely"

The brainstorming session initially settled on dropping BYO entirely. That
broke down for three reasons that surfaced once we looked at the code:

1. **Workspace orgs that mandate internal OAuth apps.** Some IT policies
   prohibit employees from authorizing third-party OAuth apps. BYO is one
   path for those users; the service account is another.
2. **The 100-user lifetime cap during verification.** Google caps unverified
   restricted-scope clients at 100 distinct grants over the project lifetime
   regardless of publishing status. msgvault's install path (Homebrew,
   conda-forge, `install.sh`) can fill that in a single Hacker News thread.
   BYO is the only escape valve during the verification window that does not
   require running a long-lived branch.
3. **The named-apps complexity is independent of BYO-as-default.** Group B
   features (named apps, `--oauth-app`, per-account binding column) exist to
   support users with multiple Gmail accounts pointing at different OAuth
   clients (personal + work). That use case persists in the hybrid world, so
   the code stays.

The work that goes away is the Group A "you forgot to configure OAuth"
plumbing, not the Group B per-account-binding system.

## Design

### Credential resolution

OAuth credential resolution becomes a three-way decision inside
`resolveOAuthManager`. Service-account resolution is unchanged and stays at
the call sites that need it: they short-circuit on
`cfg.OAuth.ServiceAccountKeyFor(appName) != ""` before invoking
`resolveOAuthManager`. So the resolver only sees OAuth cases.

```
Given an effective app name (from --oauth-app on add-account, or from
sources.oauth_app for commands operating on an existing account), and
assuming the caller has already short-circuited any service-account path:

1. Effective app name is non-empty:
   - cfg.OAuth.Apps[name].ClientSecrets is set
     -> Use BYO OAuth via the named app's client_secrets.
   - Otherwise
     -> Return error "OAuth app NAME not configured". This preserves
        today's behavior when --oauth-app or sources.oauth_app references
        a name that has no OAuth client_secrets entry. Silent fallthrough
        to embedded for an explicitly-named app would be a footgun.

2. Effective app name is empty and cfg.OAuth.ClientSecrets is set:
   -> Use BYO OAuth via the global default client_secrets.

3. Otherwise:
   -> Use the embedded client.
```

The branch is captured in a single helper so the if-chain does not get
duplicated at every call site. Existing accounts whose `sources.oauth_app`
column points at a named BYO binding continue to use that binding. Existing
accounts authorized under the global BYO default continue to use it. The
embedded path is reached only when nothing is configured.

### Embedded credentials module

New file: `internal/oauth/embedded.go`.

```go
// Package vars are intentional (not consts) so -ldflags -X can override.
// Per https://developers.google.com/identity/protocols/oauth2 the desktop
// client secret is "obviously not treated as a secret"; PKCE provides the
// flow security.
var (
    oauthClientID     = "TBD-msgvault-dev-client-id"
    oauthClientSecret = "TBD-msgvault-dev-client-secret"
)

// EmbeddedConfig returns the oauth2.Config built from the embedded
// credentials, using the loopback flow that the existing browserFlow code
// already implements.
func EmbeddedConfig(scopes []string) *oauth2.Config { ... }

// NewEmbeddedManager mirrors NewManagerWithScopes but uses embedded
// credentials instead of reading client_secret.json from disk.
func NewEmbeddedManager(tokensDir string, logger *slog.Logger, scopes []string) (*Manager, error) { ... }

// HasEmbeddedCredentials reports whether the package vars are non-empty.
// Used by selection logic to suppress the fall-through-to-embedded branch
// when a fork has stripped the values out.
func HasEmbeddedCredentials() bool { ... }
```

Source defaults:

- A separate "msgvault-dev" Google Cloud project owns the source-default
  client. Its consent screen lists current contributors as test users; its
  Gmail API quota is small. The defaults are *not* the production values,
  so forks and source builds do not burn the production project's 100-user
  cap during the verification window.
- Production values are injected at release time via `-ldflags` from GitHub
  Actions Secrets and never appear in the repo.

`HasEmbeddedCredentials` is true in any normal build (release or source);
both have non-empty package vars. A fork that strips the values out, or a
contributor who clears them locally to test the fallback path, would see it
return false; in that case `resolveOAuthManager` returns a typed
"no embedded OAuth credentials in this build" error. Callers print a
"build with embedded credentials, or configure BYO in config.toml"
message. This is a separate failure mode from the verification-window
"access_denied" case described under Verification window UX below; they
do not need to share an error type.

### Scope set for the embedded client

New variable in `internal/oauth/oauth.go`:

```go
// ScopesEmbedded is the scope set requested by the centralized verified
// msgvault OAuth client. It is the union of Scopes and ScopesDeletion so
// users on the embedded path never need scope escalation for permanent
// delete.
var ScopesEmbedded = []string{
    "https://www.googleapis.com/auth/gmail.readonly",
    "https://www.googleapis.com/auth/gmail.modify",
    "https://mail.google.com/",
}
```

`Scopes` (`internal/oauth/oauth.go:28`) and `ScopesDeletion`
(`internal/oauth/oauth.go:35`) keep their current values. The escalation flow
in `cmd/msgvault/cmd/deletions.go` keeps working unchanged for BYO users
whose own OAuth clients are registered for `gmail.readonly + gmail.modify`
only. On the embedded path, `HasScope("https://mail.google.com/")` returns
true at first auth, so the escalation prompt is never reached.

### Resolver helper

New helper in a small new file `cmd/msgvault/cmd/oauth_resolve.go`:

```go
// resolveOAuthManager builds the *oauth.Manager appropriate for the
// account+config+scopes triple. Resolution order matches the section
// above: named BYO OAuth, global BYO OAuth, embedded. Returns an error
// when an explicitly-named app has no client_secrets, or (rare) when
// embedded credentials are absent (forks, stripped builds).
// Callers handle service-account resolution themselves before calling
// this helper, because *oauth.Manager and the service-account manager
// have different interfaces.
func resolveOAuthManager(
    cfg *config.Config,
    appName string,
    scopes []string,
    logger *slog.Logger,
) (*oauth.Manager, error) { ... }
```

The canonical refactor target is `oauthManagerCache()` at
`cmd/msgvault/cmd/root.go:419-442`. That function builds and caches
`*oauth.Manager` instances per app name, and is consumed by the sync
codepath via `getOAuthMgr := oauthManagerCache()` (syncfull.go:79). Its
internals collapse to a single call to `resolveOAuthManager`; the outer
caching shell stays.

Other call sites that today inline the
`ClientSecretsFor(appName)` + `oauth.NewManagerWithScopes(...)` pair move
to `resolveOAuthManager` directly:

- `cmd/msgvault/cmd/addaccount.go:165-176` (the BYO branch that today
  emits `errOAuthNotConfigured` / `wrapOAuthError`)
- `cmd/msgvault/cmd/deletions.go:435-479` and `:695-708` (manager
  resolution before scope checks, plus the escalation re-resolve)
- `cmd/msgvault/cmd/verify.go:126-132` (the OAuth-fallback arm of
  verify's resolution)

Service-account call sites that today branch on
`cfg.OAuth.ServiceAccountKeyFor(appName)` first
(`cmd/msgvault/cmd/sync.go:228`, `cmd/msgvault/cmd/serve.go:351`,
`cmd/msgvault/cmd/verify.go:117`, `cmd/msgvault/cmd/addaccount.go:91`,
`cmd/msgvault/cmd/deletions.go:432`) keep that early branch; only their
"no service account configured, fall through to OAuth manager" arm uses
the new helper.

The `buildAPIClient` site at `cmd/msgvault/cmd/syncfull.go:228-241`
already routes through `getOAuthMgr` and so picks up the new behavior
automatically once `oauthManagerCache()` is refactored.

Two multi-source loops (`cmd/msgvault/cmd/syncfull.go:118` and
`cmd/msgvault/cmd/sync.go:128`) currently skip per-source with
`if !cfg.OAuth.HasAnyConfig() { fmt.Printf("Skipping %s ..."); continue }`.
With the embedded fallthrough, that skip becomes dead — every Gmail
source can reach the resolver. Both blocks are removed; the loop simply
calls `getOAuthMgr(appName)` for each Gmail source.

Each of those sites currently picks scopes (`oauth.Scopes` vs
`oauth.ScopesDeletion`). The helper accepts the requested scopes from the
caller; on the embedded path it ignores the request and always uses
`ScopesEmbedded` because the embedded grant is broader than any per-call
need.

### Code removed

These exist only to handle the "no OAuth configured" first-run cliff and
become dead once the embedded path is the no-config default:

| Symbol | File | Reason |
|---|---|---|
| `errOAuthNotConfigured` | `cmd/msgvault/cmd/root.go` | Embedded path is always available in release builds |
| `tryFindClientSecrets` | `cmd/msgvault/cmd/root.go` | Same |
| `oauthSetupHint` | `cmd/msgvault/cmd/root.go` | Same |
| `wrapOAuthError` | `cmd/msgvault/cmd/root.go` | Same |
| `OAuthConfig.HasAnyConfig` | `internal/config/config.go` | All six call sites are "if !HasAnyConfig { error or skip }" gates; embedded fallthrough makes each one unreachable. |
| Interactive OAuth prompt in `setup` (`setupOAuthSecrets`) | `cmd/msgvault/cmd/setup.go` | The step asks for `client_secret.json`; no longer required because the embedded client is the default. The function itself is removed. |
| Mandatory `client_secret.json` copy into `setup`'s deployment bundle | `cmd/msgvault/cmd/setup.go` | Setup's bundle mode still supports copying a BYO `client_secret.json` into the bundle when the operator opts in; it just no longer requires one. The required-copy code path goes away. |
| Tests covering the above | `cmd/msgvault/cmd/root_test.go`, `cmd/msgvault/cmd/setup_test.go`, `internal/config/config_test.go` (`TestOAuthConfig_HasAnyConfig` plus the inline `HasAnyConfig` assertions in `TestLoadWithNamedOAuthApps` and `TestLoadWithNamedOAuthApps_RelativePaths`) | Follow the symbols |

The `setup` command's other responsibilities (data dir creation, optional
remote configuration) stay. Whether to retire `setup` entirely in favor of a
slimmer `init` is a separable decision and not part of this design.

### What stays untouched

- `[oauth] client_secrets` field in config
- `[oauth.apps.NAME]` named-apps map and the entire `OAuthApp` struct
- `[oauth] service_account_key` field and `[oauth.apps.NAME]
  service_account_key`
- `--oauth-app NAME` flag on every command that has it today
- `sources.oauth_app` column and binding-change detection
- `oauth.TokenMatchesClient`, `HasScope`, `HasScopeMetadata`
- Scope-escalation prompt (`promptScopeEscalation` in
  `cmd/msgvault/cmd/deletions.go`) for BYO accounts whose own clients are
  read+modify-only
- `OAuthConfig.ClientSecretsFor` (the resolver helper calls it on the BYO
  branches)
- `OAuthConfig.ServiceAccountKeyFor` (call sites still use it for their
  early service-account short-circuit)

## Documentation changes

- `README.md`: drop the "Follow the OAuth Setup Guide" prerequisite from the
  Quick Start. Replace with a one-liner: "`msgvault add-account
  you@gmail.com`, that's it". Add a short subsection later in the README
  titled "Advanced: bring your own OAuth client" that links to the existing
  setup guide for the Workspace-mandate, privacy-conscious, and quota
  cases.
- `cmd/msgvault/cmd/quickstart.md`: same shape. Embedded is the default;
  BYO is a footnote.
- `https://msgvault.io/guides/oauth-setup/`: not removed (BYO users still
  need it), but the page intro changes to "Most users do not need this
  page; the default `msgvault add-account` flow works without any Cloud
  Console setup. This guide is for Workspace orgs that mandate internal
  OAuth apps, users who prefer their own Cloud project, and similar
  advanced cases."
- `https://msgvault.io/` landing page: drop the "~5 minutes" OAuth-setup
  callout from the install path.

The msgvault.io site lives in a separate repo; site changes are out of
scope for the implementation PR in this codebase, but the Quick Start
docs that live in this repo are in scope.

## Build and release integration

Extend `LDFLAGS` in the Makefile:

```make
LDFLAGS := -X github.com/wesm/msgvault/cmd/msgvault/cmd.Version=$(VERSION) \
           -X github.com/wesm/msgvault/cmd/msgvault/cmd.Commit=$(COMMIT) \
           -X github.com/wesm/msgvault/cmd/msgvault/cmd.BuildDate=$(BUILD_DATE) \
           -X github.com/wesm/msgvault/internal/oauth.oauthClientID=$(MSGVAULT_OAUTH_CLIENT_ID) \
           -X github.com/wesm/msgvault/internal/oauth.oauthClientSecret=$(MSGVAULT_OAUTH_CLIENT_SECRET)
```

When the env vars are unset (e.g., `make build` by a contributor), the `-X`
flags become no-ops and the package vars keep their source defaults.

In `.github/workflows/release.yml`, the release build step sets:

```yaml
- name: Build release binary
  env:
    MSGVAULT_OAUTH_CLIENT_ID: ${{ secrets.MSGVAULT_OAUTH_CLIENT_ID }}
    MSGVAULT_OAUTH_CLIENT_SECRET: ${{ secrets.MSGVAULT_OAUTH_CLIENT_SECRET }}
  run: make build-release
```

The CI workflow at `.github/workflows/ci.yml` does *not* set these. CI builds
use the source defaults (the dev client), which is fine for unit tests since
they stub the OAuth manager.

## Verification window UX

While Google verification is in progress (estimated 2 to 3 months from
submission), the embedded client lives in Cloud Console "In Production
(unverified)" status. Two constraints apply:

- **100-user lifetime cap.** Across the project lifetime, no more than 100
  distinct Google accounts can authorize the client. The counter does not
  reset.
- **7-day refresh-token expiry.** Tokens issued by an unverified
  restricted-scope client expire after 7 days. Users re-authorize weekly.

When `add-account` on the embedded path receives an `access_denied`
response from Google's OAuth endpoints (the failure mode for both
"caller is not on the test-user list" and "100-user lifetime cap reached"),
it prints:

```
msgvault's centralized OAuth client is still in Google's verification
queue. Two options:

  1. Use the bring-your-own setup (one-time, ~5 minutes):
     https://msgvault.io/guides/oauth-setup/

  2. Request beta access (open a GitHub issue with your Gmail address):
     https://github.com/wesm/msgvault/issues/new?template=beta-oauth.md
```

The implementer should not key off Google's specific error sub-codes (those
have changed over time and are not contractually stable). `access_denied`
plus the absence of a returned token from the loopback flow is signal
enough to print the fallback message.

No silent failures, no infinite retry loops. Users on the embedded path who
re-authorize weekly during the window do not see this message; only users
hitting the cap or unlisted-test-user wall do.

The same fallback message is printed by any command that triggers a
browser-OAuth flow on the embedded path and receives `access_denied`, not
just `add-account`. This includes sync commands that fail token refresh
after the 7-day window and re-enter the loopback flow, and `verify` when
it has to re-authorize.

After verification lands, Google flips a flag on their end. The cap lifts;
refresh tokens become long-lived. No msgvault code change is needed at that
moment.

## Testing strategy

- `internal/oauth/embedded_test.go` (new): unit test the `EmbeddedConfig`
  builder and `HasEmbeddedCredentials` detection. Verify the ldflags
  override mechanism by setting the package vars in a test fixture.
- `internal/oauth/oauth_test.go`: existing tests stay; they exercise BYO
  paths via `parseClientSecrets`.
- `cmd/msgvault/cmd/addaccount_test.go`: add cases covering the new
  resolution paths visible from `add-account`: the service-account
  short-circuit, the three branches inside `resolveOAuthManager` (named
  BYO OAuth, global BYO OAuth, embedded), and the error case where
  `--oauth-app NAME` references an app without `client_secrets`.
- `cmd/msgvault/cmd/oauth_resolve_test.go` (new): unit-test the resolver
  helper in isolation with each combination of config inputs.
- End-to-end: existing fixtures cover BYO; add a stub embedded fixture that
  fakes `oauth.NewEmbeddedManager` to skip the browser-flow step.
- Manual test plan documented in the implementation plan: walk through
  clean-install on a fresh machine, verify both the embedded happy path and
  the cap-exceeded message.

## Migration

No migration code needed.

- Users with `[oauth] client_secrets` set: keep working via BYO.
- Users with `[oauth.apps.*]` named apps: keep working via named BYO.
- Users with `[oauth] service_account_key`: keep working via service account.
- Users with empty/missing OAuth config on a clean install: previously got
  `errOAuthNotConfigured`; now get the embedded client.

The only implicit transition is when an existing user deletes their
`[oauth] client_secrets` line. Their next `add-account` for that email runs
the embedded consent flow. The resulting token overwrites the existing
token file for that email (under `<TokensDir>/<sanitized-email>.json`),
replacing the old BYO `client_id` metadata with the embedded one. From
that point forward, every command resolving credentials for that account
uses the embedded path. The user's old BYO Cloud project becomes
irrelevant but is not touched.

## Out of scope

- Microsoft 365 centralization. Microsoft has its own verification process
  (publisher verification, permissions justification, separate cost
  structure). Tackle in a follow-up effort.
- A "msgvault init wizard" that bundles `init-db` + first `add-account`
  into a single command. Separable improvement.
- Encryption at rest (database, attachments). Tracked in the existing
  "Not Yet Implemented" list in `CLAUDE.md`.
- Removing the `setup` command. The OAuth-credentials step is removed; the
  rest of `setup` is a separate cleanup.

## Out-of-band verification prep

These do not block landing the code, but the verified client cannot be used
in production by anyone outside the 100-user test cohort until they are
complete.

| Item | Owner | Notes |
|---|---|---|
| Privacy policy hosted on msgvault.io (first-party) | Project maintainer | Must be on a verified-owned domain. GitHub Pages does not count. |
| OAuth consent screen branding | Project maintainer | App name "msgvault", logo, support email, scope justifications |
| Brand verification (Google confirms msgvault.io ownership) | Project maintainer | 2 to 3 business days |
| Demo video (unlisted YouTube, ~5 minutes) | Project maintainer | Shows OAuth flow plus each scope's usage in production-level domain |
| OAuth consent screen submission for restricted scopes | Project maintainer | 2 to 8 weeks of review iteration |
| CASA assessor contract | Project maintainer | TAC Security is cheapest (~$500/yr) via Google's negotiated deal |
| OWASP ZAP pre-scan in CI | Project maintainer | Add as a GitHub Actions job before formal DAST |
| CASA Tier 2 SAQ draft | Project maintainer | 54 questions; mostly reusable across years |
| DAST scan against production app | Assessor | Likely targets msgvault.io plus OAuth client source audit |
| Letter of Validation submitted to Cloud Console | Project maintainer | Final step |
| Annual recertification | Project maintainer | Recert email arrives 12 months from LOV approval |

## Project ownership and verification deadline

- **Production OAuth client.** Owned by the project maintainer. Recert
  emails go to the account on file. The specific account is tracked
  outside this repo.
- **Verification deadline is the 100-user cap, not the calendar.** Google
  caps unverified restricted-scope clients at 100 distinct grants over the
  project lifetime, and the counter does not reset. Verification (consent
  screen submission, brand verification, CASA Tier 2 assessment, LOV) must
  complete before the production project accumulates 100 grants. If it
  does not, new users hit the BYO fallback path until verification lands.
  This is treated as a hard scheduling constraint, not a soft target.
- **Dev project cap.** The dev Cloud project also has the 100-user lifetime
  cap. Not worth solving until it actually becomes a problem; contributors
  who hit it can BYO.

## References

- Google docs on the desktop client secret not being treated as a secret:
  https://developers.google.com/identity/protocols/oauth2
- Google docs on restricted scope verification:
  https://developers.google.com/identity/protocols/oauth2/production-readiness/restricted-scope-verification
- Google docs on app audience and user cap behavior:
  https://support.google.com/cloud/answer/15549945
- Google Gmail API scope classifications (confirms readonly, modify, and
  mail.google.com/ are all in the Restricted bucket):
  https://developers.google.com/workspace/gmail/api/auth/scopes
- gh CLI precedent for ldflags-overridable embedded OAuth client_id:
  https://sourcegraph.com/r/github.com/cli/cli/-/blob/internal/authflow/flow.go
- CASA Tier 2 process overview:
  https://appdefensealliance.dev/casa/tier-2/tier2-overview
