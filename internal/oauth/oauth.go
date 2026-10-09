// Package oauth provides OAuth2 authentication flows for Gmail.
package oauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Gmail scope identifiers. Declared once so the scope *sets* below and every
// "does this account have write access" check derive from the same strings.
const (
	// ScopeGmailReadonly grants read access to mail and settings.
	ScopeGmailReadonly = "https://www.googleapis.com/auth/gmail.readonly"
	// ScopeGmailModify grants read plus trash/untrash/label changes.
	ScopeGmailModify = "https://www.googleapis.com/auth/gmail.modify"
	// ScopeGmailFull is Gmail's broad full-access scope. It is required for
	// batchDelete and, being a superset of modify, also permits trashing.
	ScopeGmailFull = "https://mail.google.com/"

	// ScopeGmailSend and the scopes below it are the remaining write-capable
	// Gmail scopes. msgvault never requests any of them, but a token it is
	// asked to reuse may carry them, and narrowing must not leave one behind.
	ScopeGmailSend            = "https://www.googleapis.com/auth/gmail.send"
	ScopeGmailCompose         = "https://www.googleapis.com/auth/gmail.compose"
	ScopeGmailInsert          = "https://www.googleapis.com/auth/gmail.insert"
	ScopeGmailLabels          = "https://www.googleapis.com/auth/gmail.labels"
	ScopeGmailSettingsBasic   = "https://www.googleapis.com/auth/gmail.settings.basic"
	ScopeGmailSettingsSharing = "https://www.googleapis.com/auth/gmail.settings.sharing"

	// ScopeGmailMetadata is read-only: headers and labels without message
	// bodies. Named so it is not mistaken for a write scope.
	ScopeGmailMetadata = "https://www.googleapis.com/auth/gmail.metadata"
)

// Scopes for normal msgvault operations (sync, search, read).
var Scopes = []string{
	ScopeGmailReadonly,
	ScopeGmailModify,
}

// ScopesDeletion includes full access required for batchDelete API.
// gmail.modify supports trash/untrash but NOT batchDelete.
var ScopesDeletion = []string{
	ScopeGmailFull,
}

// ScopesGmailReadonly is the Gmail scope set requested by
// `add-account --readonly`: read access and nothing else.
var ScopesGmailReadonly = []string{
	ScopeGmailReadonly,
}

// ScopesGmailWrite is every Gmail scope that confers write access, meaning any
// ability to alter the mailbox or act as the user: modify mail, send or insert
// it, manage labels, or change settings.
//
// Any decision of the form "does this account currently have Gmail write
// access" or "which scopes should be dropped when narrowing to read-only" MUST
// consider this whole set. Checking gmail.modify alone silently mishandles an
// account holding only the full-access scope, and checking only those two
// silently mishandles one holding gmail.send.
//
// msgvault requests only readonly, modify, and full, so the rest appear only on
// a token minted elsewhere for the same OAuth client. Narrowing still has to
// strip them: a grant is read-only when nothing in it can write, not when the
// scopes msgvault happens to request are absent.
//
// This list names the write scopes for documentation and for messages that
// enumerate them. It is NOT what classification consults — that is
// isGmailWriteScope, which allow-lists the read-only scopes so an unrecognised
// Gmail scope fails closed rather than passing as read-only.
var ScopesGmailWrite = []string{
	ScopeGmailModify,
	ScopeGmailFull,
	ScopeGmailSend,
	ScopeGmailCompose,
	ScopeGmailInsert,
	ScopeGmailLabels,
	ScopeGmailSettingsBasic,
	ScopeGmailSettingsSharing,
}

// ScopesGmailDraftWrite are the accepted grants for creating, editing, or
// deleting a Gmail draft.
var ScopesGmailDraftWrite = []string{
	ScopeGmailFull,
	ScopeGmailModify,
	ScopeGmailCompose,
}

// ScopesGmailSendAsList are the accepted grants for listing send-as entries.
var ScopesGmailSendAsList = []string{
	ScopeGmailSettingsBasic,
	ScopeGmailFull,
	ScopeGmailModify,
	ScopeGmailReadonly,
}

// GrantCoversAnyScope reports whether a stored grant contains one accepted
// scope. Operation sets stay separate because Gmail assigns them separately.
func GrantCoversAnyScope(granted, accepted []string) bool {
	granted = normalizedScopeList(granted)
	for _, scope := range normalizedScopeList(accepted) {
		if slices.Contains(granted, scope) {
			return true
		}
	}
	return false
}

// gmailScopePrefixes match every scope Google issues for Gmail. A scope
// carrying one of these is Gmail's; anything else belongs to another API.
var gmailScopePrefixes = []string{
	"https://mail.google.com/",
	"https://www.googleapis.com/auth/gmail.",
}

// gmailReadOnlyScopes is the complete allow-list of Gmail scopes that cannot
// modify the mailbox or act as the user.
//
// Classification is an allow-list on purpose. A deny-list of known write scopes
// fails open: a Gmail scope nobody thought of reads as read-only, so a grant
// that can send mail passes as narrow. Enumerating what is safe fails closed
// instead — an unrecognised Gmail scope is treated as write access, which at
// worst refuses a narrowing that would have been fine.
var gmailReadOnlyScopes = []string{
	ScopeGmailReadonly,
	ScopeGmailMetadata,
	"https://www.googleapis.com/auth/gmail.addons.current.message.readonly",
	"https://www.googleapis.com/auth/gmail.addons.current.message.metadata",
}

// isGmailScope reports whether a scope belongs to the Gmail API.
func isGmailScope(scope string) bool {
	for _, prefix := range gmailScopePrefixes {
		if strings.HasPrefix(scope, prefix) {
			return true
		}
	}
	return false
}

// isGmailWriteScope reports whether a scope is a Gmail scope that is not on the
// read-only allow-list. Unrecognised Gmail scopes count as write.
func isGmailWriteScope(scope string) bool {
	return isGmailScope(scope) && !slices.Contains(gmailReadOnlyScopes, scope)
}

// HasGmailWriteScope reports whether scopes contain any Gmail scope that
// grants write access. See gmailReadOnlyScopes for why this is decided by an
// allow-list rather than by membership of ScopesGmailWrite.
func HasGmailWriteScope(scopes []string) bool {
	return len(GrantedGmailWriteScopes(scopes)) > 0
}

// GrantedGmailWriteScopes returns the Gmail write scopes present in scopes,
// so callers can name exactly what an account holds in operator-facing
// messages rather than guessing.
func GrantedGmailWriteScopes(scopes []string) []string {
	var granted []string
	for _, scope := range scopes {
		if isGmailWriteScope(scope) {
			granted = append(granted, scope)
		}
	}
	return granted
}

// WithoutGmailWriteScopes returns scopes with every Gmail write scope removed
// and everything else — Calendar, Drive, gmail.readonly — left in place and in
// order. This is how narrowing preserves unrelated grants.
func WithoutGmailWriteScopes(scopes []string) []string {
	kept := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		if isGmailWriteScope(scope) {
			continue
		}
		kept = append(kept, scope)
	}
	return kept
}

// HasAnyGmailScope reports whether scopes contain any Gmail scope at all.
// It distinguishes "this account holds a narrower Gmail grant" from "this
// account has no Gmail grant yet", which read differently: the first is a
// deliberate narrowing worth preserving, the second is a first-time
// authorization where warning about widening would be noise.
func HasAnyGmailScope(scopes []string) bool {
	return slices.ContainsFunc(scopes, isGmailScope)
}

// IsNarrowedGmailGrant reports whether scopes describe a Gmail grant that has
// deliberately been narrowed: Gmail access is present, but no write scope is.
// Re-authorization paths use this to avoid silently restoring write access to
// an account the operator narrowed on purpose.
//
// A grant with no Gmail scopes at all is not "narrowed" — it has simply never
// had Gmail — so this returns false and such accounts widen as before.
func IsNarrowedGmailGrant(scopes []string) bool {
	return HasAnyGmailScope(scopes) && !HasGmailWriteScope(scopes)
}

// ScopeCardDAV grants access to Google Contacts through CardDAV.
const ScopeCardDAV = "https://www.googleapis.com/auth/carddav"

// ScopeUserinfoEmail allows account verification without requesting Gmail access.
const ScopeUserinfoEmail = "https://www.googleapis.com/auth/userinfo.email"

// ScopeCalendarReadonly is the read-only Calendar scope: it covers both
// calendarList enumeration and event reads, so an archival tool needs nothing
// finer-grained.
const ScopeCalendarReadonly = "https://www.googleapis.com/auth/calendar.readonly"

// ScopeCalendarEvents is opt-in permission to create and change calendar events.
const ScopeCalendarEvents = "https://www.googleapis.com/auth/calendar.events"

// ScopesCalendar is the opt-in scope set for calendar sync.
var ScopesCalendar = []string{
	ScopeCalendarReadonly,
}

// ScopesGmailCalendar bundles the normal Gmail scopes with Calendar for
// re-consent. Re-authorizing uses ApprovalForce with no include_granted_scopes,
// which REPLACES (not unions) the granted scope set — so an existing Gmail
// account opting into calendar must re-consent with BOTH scope families or it
// silently loses Gmail access. Always pass this bundle (never ScopesCalendar
// alone) when escalating an account that already has Gmail.
var ScopesGmailCalendar = append(append([]string{}, Scopes...), ScopesCalendar...)

const defaultProfileURL = "https://gmail.googleapis.com/gmail/v1/users/me/profile"
const defaultCalendarProfileURL = "https://www.googleapis.com/calendar/v3/users/me/calendarList/primary"

// ErrTokenChanged means another operation replaced the saved credentials.
var ErrTokenChanged = errors.New("saved Google credentials changed during sign-in; start sign-in again to use the current permissions")

// TokenMismatchError is returned when the authorized Google account
// does not match the expected email. Callers can inspect Expected
// and Actual to provide context-appropriate remediation.
type TokenMismatchError struct {
	Expected string // email the user asked to authorize
	Actual   string // email returned by the Gmail profile API
}

func (e *TokenMismatchError) Error() string {
	return fmt.Sprintf(
		"token mismatch: expected %s but authorized as %s",
		e.Expected, e.Actual,
	)
}

// Manager handles OAuth2 token acquisition and storage.
type Manager struct {
	authorizationExpected *tokenExpectation
	tokenStore            *TokenStore
	config                *oauth2.Config
	tokensDir             string
	logger                *slog.Logger
	profileURL            string // profile endpoint override for tests
	revokeURL             string // revocation endpoint override for tests
	// Nil for Desktop clients, whose loopback redirects need no registration.
	webRedirectURIs []string

	// browserFlowFn overrides browserFlow in tests to avoid starting
	// a real HTTP server and browser. When nil, the real browserFlow
	// is used.
	browserFlowFn func(ctx context.Context, email string, launchBrowser bool) (*oauth2.Token, error)
}

// TokenSource returns a token source for the given email.
// If a valid token exists, it will be reused and auto-refreshed. A manager
// bound by WithTokenInfo starts from that snapshot instead of reading again;
// saving a refresh still compares against the current stored token.
func (m *Manager) TokenSource(ctx context.Context, email string) (oauth2.TokenSource, error) {
	tf, err := m.metadataTokenFile(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("no valid token for %s: %w", email, err)
	}
	token := tf.Token // saves rewrite tf in place; the source keeps its own copy
	source := &persistingTokenSource{
		manager:  m,
		source:   m.config.TokenSource(withRefreshHTTPClient(ctx), &token),
		email:    email,
		expected: tf,
		ctx:      ctx,
	}
	if _, err := source.Token(); err != nil {
		return nil, err
	}
	return source, nil
}

// HasToken checks if a token exists for the given email.
func (m *Manager) HasToken(ctx context.Context, email string) bool {
	_, err := m.metadataTokenFile(ctx, email)
	return err == nil
}

// ForceRefresh redeems the stored refresh token for a new access token,
// bypassing any cached access token that has not expired yet. TokenSource
// returns a still-valid cached access token without contacting the provider,
// so it cannot detect a revoked refresh token; callers that must know whether
// the token remains refreshable (e.g. add-calendar deciding between reuse and
// reauthorization) use this probe instead. The refreshed token is saved on
// success.
func (m *Manager) ForceRefresh(ctx context.Context, email string) error {
	tf, err := m.loadTokenFile(ctx, email)
	if err != nil {
		return fmt.Errorf("no valid token for %s: %w", email, err)
	}
	if tf.RefreshToken == "" {
		return fmt.Errorf("token for %s has no refresh token", email)
	}
	stale := &oauth2.Token{RefreshToken: tf.RefreshToken}
	source := &persistingTokenSource{
		manager:  m,
		source:   m.config.TokenSource(withRefreshHTTPClient(ctx), stale),
		email:    email,
		expected: tf,
		ctx:      ctx,
	}
	_, err = source.Token()
	return err
}

// HeadlessAccountArgs shares account and permission flags across setup instructions.
func HeadlessAccountArgs(command, email, app string, readonly, write bool) []string {
	args := []string{"msgvault", command, email}
	if app != "" {
		args = append(args, "--oauth-app", app)
	}
	if command == "add-account" && readonly {
		args = append(args, "--readonly")
	}
	if command == "add-calendar" && write {
		args = append(args, "--write")
	}
	return args
}

func headlessArgumentSafe(arg string) bool {
	if arg == "" || arg[0] == '@' {
		return false
	}
	for _, ch := range arg {
		if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && !strings.ContainsRune("_./:@+=-", ch) {
			return false
		}
	}
	return true
}

// PrintHeadlessCommand preserves native arguments in the supported shells.
func PrintHeadlessCommand(out io.Writer, args ...string) {
	if slices.ContainsFunc(args, func(arg string) bool { return !headlessArgumentSafe(arg) }) {
		posix, powershell := slices.Clone(args), slices.Clone(args)
		for i, arg := range args {
			if !headlessArgumentSafe(arg) {
				posix[i] = ShellQuote(arg)
				powershell[i] = "'" + strings.ReplaceAll(arg, "'", "''") + "'"
			}
		}
		_, _ = fmt.Fprintln(out, "    On Windows, use the PowerShell 7.3+ command.")
		_, _ = fmt.Fprintf(out, "    POSIX shell:\n    %s\n", strings.Join(posix, " "))
		_, _ = fmt.Fprintf(out, "    PowerShell 7.3+:\n    %s\n", strings.Join(powershell, " "))
		return
	}
	_, _ = fmt.Fprintf(out, "    %s\n", strings.Join(args, " "))
}

// PrintHeadlessInstructions prints setup instructions for headless servers.
// Google's device flow does not support Gmail scopes, so users must authorize
// on a machine with a browser and copy the token file.
// tokensDir should be the configured tokens directory (e.g., cfg.TokensDir()).
// readonly echoes --readonly back into the printed commands so the operator
// authorizes on the browser machine with the same narrowed grant they asked
// for here.
func PrintHeadlessInstructions(email, tokensDir, oauthApp string, readonly bool) {
	// Use same sanitization as tokenPath for consistency
	tokenFile := sanitizeEmail(email) + ".json"
	tokenPath := filepath.Join(tokensDir, tokenFile)
	_, tokenErr := os.Stat(tokenPath)
	hasToken := tokenErr == nil

	addArgs := HeadlessAccountArgs("add-account", email, oauthApp, readonly, false)

	fmt.Println()
	fmt.Println("=== Headless Server Setup ===")
	fmt.Println()
	fmt.Println("Google's OAuth device flow does not support Gmail scopes, so --headless")
	fmt.Println("cannot directly authorize. Instead, authorize on a machine with a browser")
	fmt.Println("and copy the token to your server.")
	fmt.Println()
	step := 1
	if hasToken {
		fmt.Printf("Step %d: Copy the existing token from the headless server to the browser machine:\n", step)
		fmt.Println()
		fmt.Printf("    mkdir -p %s\n", shellQuote(tokensDir))
		fmt.Printf("    scp user@server:%s %s\n", shellQuote(tokenPath), shellQuote(tokenPath))
		fmt.Println()
		step++
	}
	fmt.Printf("Step %d: On a machine with a browser, run:\n", step)
	fmt.Println()
	browserArgs := slices.Clone(addArgs)
	if hasToken {
		browserArgs = append(browserArgs, "--force")
	}
	PrintHeadlessCommand(os.Stdout, browserArgs...)
	fmt.Println()
	step++
	fmt.Printf("Step %d: Copy the token file to your headless server:\n", step)
	fmt.Println()
	fmt.Printf("    ssh user@server mkdir -p %s\n", shellQuote(tokensDir))
	fmt.Printf("    scp %s user@server:%s\n", shellQuote(tokenPath), shellQuote(tokenPath))
	fmt.Println()
	step++
	fmt.Printf("Step %d: On the headless server, register the account:\n", step)
	fmt.Println()
	PrintHeadlessCommand(os.Stdout, addArgs...)
	fmt.Println()
	fmt.Println("The token will be detected and the account registered. No browser needed.")
	fmt.Println("All msgvault commands (sync, tui, etc.) will work normally.")
	fmt.Println()
}

// PrintCalendarHeadlessInstructions prints setup instructions for adding
// Calendar access on a headless server. As with Gmail, Google's device flow
// does not support Calendar scopes, so the operator must authorize on a machine
// with a browser and copy the token file to the server. Calendar re-consent
// REPLACES the granted scopes, so the browser machine must keep existing
// permissions plus Calendar checked or access is dropped.
// tokensDir should be the configured tokens directory (e.g., cfg.TokensDir()).
func PrintCalendarHeadlessInstructions(email, tokensDir, oauthApp string, write ...bool) {
	tokenFile := sanitizeEmail(email) + ".json"
	tokenPath := filepath.Join(tokensDir, tokenFile)

	addArgs := HeadlessAccountArgs("add-calendar", email, oauthApp, false, len(write) > 0 && write[0])
	syncArgs := HeadlessAccountArgs("sync-calendar", email, oauthApp, false, false)

	fmt.Println()
	fmt.Println("=== Headless Server Calendar Setup ===")
	fmt.Println()
	fmt.Println("A headless server cannot complete Google's browser consent, and the OAuth")
	fmt.Println("device flow does not support Calendar scopes. Authorize on a machine with a")
	fmt.Println("browser and copy the token to your server.")
	fmt.Println()
	fmt.Println("Step 0: If this account already has a token on the headless server, copy")
	fmt.Println("        that existing token to the browser machine first. This lets")
	fmt.Println("        add-calendar preserve Drive or other previously granted scopes:")
	fmt.Println()
	fmt.Printf("    mkdir -p %s\n", shellQuote(tokensDir))
	fmt.Printf("    scp user@server:%s %s\n", shellQuote(tokenPath), shellQuote(tokenPath))
	fmt.Println()
	fmt.Println("        Skip this step only when no token exists yet.")
	fmt.Println()
	fmt.Println("Step 1: On a machine with a browser (using the SAME client_secret.json as the")
	fmt.Println("        server), run:")
	fmt.Println()
	PrintHeadlessCommand(os.Stdout, addArgs...)
	fmt.Println()
	fmt.Println("        On the consent screen, keep all existing permissions plus Calendar")
	fmt.Println("        checked — re-consent REPLACES scopes, so unchecking an existing")
	fmt.Println("        permission would drop that access.")
	fmt.Println()
	fmt.Println("Step 2: Copy the token file to your headless server, replacing the existing one:")
	fmt.Println()
	fmt.Printf("    ssh user@server mkdir -p %s\n", shellQuote(tokensDir))
	fmt.Printf("    scp %s user@server:%s\n", shellQuote(tokenPath), shellQuote(tokenPath))
	fmt.Println()
	fmt.Println("Step 3: On the headless server, register the calendars (no browser needed)")
	fmt.Println("        and sync:")
	fmt.Println()
	PrintHeadlessCommand(os.Stdout, addArgs...)
	PrintHeadlessCommand(os.Stdout, syncArgs...)
	fmt.Println()
	fmt.Println("The copied token carries Calendar plus the existing Google permissions,")
	fmt.Println("so current sync jobs keep working.")
	fmt.Println()
}

// sanitizeEmail sanitizes an email for use in a filename.
func sanitizeEmail(email string) string {
	safe := strings.ReplaceAll(email, "/", "_")
	safe = strings.ReplaceAll(safe, "\\", "_")
	safe = strings.ReplaceAll(safe, "..", "_")
	return safe
}

// shellQuote returns a shell-safe quoted string using single quotes.
// Handles embedded single quotes by ending the quoted string, adding an
// escaped single quote, and starting a new quoted string: ' -> '\”.
func shellQuote(s string) string {
	return ShellQuote(s)
}

// ShellQuote returns a shell-safe single-quoted form of s, so a path printed
// into a copy-pasteable command survives spaces and quotes.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// Authorize performs the browser OAuth flow for a new account.
// It opens the default browser and validates that the authorized
// account matches the expected email.
func (m *Manager) Authorize(ctx context.Context, email string) error {
	return m.authorize(ctx, email, true, false)
}

// AuthorizeManual performs the OAuth flow without opening a browser.
// It prints the authorization URL with clear account context so the
// user knows exactly which account to authorize. Used during sync
// re-auth to prevent accidental account mismatch.
func (m *Manager) AuthorizeManual(ctx context.Context, email string) error {
	return m.authorize(ctx, email, false, false)
}

// AuthorizeManualPreservingGrantedScopes reauthorizes with the manager's
// required scopes plus any scopes already recorded on the token file.
func (m *Manager) AuthorizeManualPreservingGrantedScopes(ctx context.Context, email string) error {
	return m.authorize(ctx, email, false, true)
}

// AuthorizePreservingGrantedScopes is the browser twin of
// AuthorizeManualPreservingGrantedScopes: it opens the browser and reauthorizes
// with the manager's required scopes plus any scopes already recorded on the
// token file. Google's forced consent REPLACES the granted scope set, so
// re-authorizing with the bare required scopes would silently drop previously
// granted grants (Calendar, permanent-delete); the union preserves them.
func (m *Manager) AuthorizePreservingGrantedScopes(ctx context.Context, email string) error {
	return m.authorize(ctx, email, true, true)
}

func (m *Manager) prepareAuthorizationContext(ctx context.Context, email string, preserveGrants bool) (*Manager, *tokenFile, error) {
	data, err := m.tokenStore.Read(ctx, email)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("read token before authorization: %w", err)
	}
	if expectation := m.authorizationExpected; expectation != nil && expectation.email == email {
		if exists != expectation.snapshot.exists || !bytes.Equal(data, expectation.snapshot.data) {
			return nil, nil, ErrTokenChanged
		}
	}
	var existing tokenFile
	if exists {
		parsed, parseErr := m.parseTokenFile(data)
		if parseErr != nil && m.CommandTokens() {
			return nil, nil, fmt.Errorf("read token before authorization: %w", parseErr)
		}
		// File-backed authorization can replace a malformed token.
		existing = parsed
	}
	existing.snapshot = data
	existing.snapshotExists = exists
	if preserveGrants {
		m = m.WithScopes(scopesWithPreservedGrants(m.config.Scopes, existing.Scopes))
	}
	return m, &existing, nil
}

// WithScopes reuses client credentials with another requested scope set.
func (m *Manager) WithScopes(scopes []string) *Manager {
	scoped := *m
	config := *m.config
	config.Scopes = normalizedScopeList(scopes)
	scoped.config = &config
	return &scoped
}

// scopesWithPreservedGrants unions the scopes a caller requires with the ones
// the account already holds, because Google's forced re-consent REPLACES the
// granted set rather than adding to it.
//
// The union is asymmetric in one direction: if the existing grant is a
// deliberately narrowed Gmail grant (read access, no write scope), the Gmail
// write scopes are dropped from the required set rather than being restored.
// Callers here reach this path on incidental re-authorization — a token
// expired, or Calendar is being added — where quietly handing back write
// access the operator removed would defeat `add-account --readonly`. Paths
// that mean to escalate build their scope list explicitly and call Authorize
// directly, so they are unaffected.
func scopesWithPreservedGrants(required, granted []string) []string {
	if IsNarrowedGmailGrant(granted) {
		required = WithoutGmailWriteScopes(required)
	}
	scopes := append([]string(nil), required...)
	for _, scope := range granted {
		if !slices.Contains(scopes, scope) {
			scopes = append(scopes, scope)
		}
	}
	return normalizedScopeList(scopes)
}

func (m *Manager) authorize(
	ctx context.Context, email string, launchBrowser, preserveGrants bool,
) error {
	m, expected, err := m.prepareAuthorizationContext(ctx, email, preserveGrants)
	if err != nil {
		return err
	}
	flow := m.browserFlow
	if m.browserFlowFn != nil {
		flow = m.browserFlowFn
	}
	token, err := flow(ctx, email, launchBrowser)
	if err != nil {
		return err
	}

	return m.verifyAndSaveToken(ctx, email, token, expected)
}

func (m *Manager) verifyAndSaveToken(ctx context.Context, email string, token *oauth2.Token, expected *tokenFile) error {
	scopes, err := m.verifyToken(ctx, email, token)
	if err != nil {
		return err
	}
	return m.saveTokenComparedContext(ctx, email, token, scopes, expected)
}

// verifyToken checks the account and scopes and returns the scopes to record.
func (m *Manager) verifyToken(ctx context.Context, email string, token *oauth2.Token) ([]string, error) {
	// Validate the token belongs to the expected account before
	// persisting it. This prevents token pollution where selecting
	// the wrong Google account would overwrite a valid token file.
	// The token is always saved under the original identifier (email)
	// since that's the key used for all lookups elsewhere in the app.
	if _, err := m.resolveTokenEmail(ctx, email, token); err != nil {
		return nil, err
	}

	grantedScopes := grantedScopesFromToken(token, m.config.Scopes)
	if missing := missingScopes(m.config.Scopes, grantedScopes); len(missing) > 0 {
		return nil, fmt.Errorf("authorized token missing required OAuth scopes: %s", strings.Join(missing, ", "))
	}
	return grantedScopes, nil
}

const (
	callbackPath            = "/callback"
	terminalCallbackAddress = "localhost:8089"
	terminalRedirectURL     = "http://" + terminalCallbackAddress + callbackPath
)

// newCallbackHandler returns an HTTP handler that processes the OAuth callback.
func (m *Manager) newCallbackHandler(expectedState string, codeChan chan<- string, errChan chan<- error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != expectedState {
			errChan <- errors.New("state mismatch: possible CSRF attack")
			_, _ = fmt.Fprintf(w, "Error: state mismatch")
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			errChan <- errors.New("no code in callback")
			_, _ = fmt.Fprintf(w, "Error: no authorization code received")
			return
		}
		codeChan <- code
		_, _ = fmt.Fprintf(w, "Authorization successful! You can close this window.")
	}
}

// browserFlow runs the OAuth authorization flow with a local callback server.
// email is used as login_hint to pre-select the Google account.
// If launchBrowser is false, the URL is printed without opening a browser.
func (m *Manager) browserFlow(
	ctx context.Context, email string, launchBrowser bool,
) (*oauth2.Token, error) {
	// Bail early if context is already cancelled — no point starting
	// a server or opening a browser.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if m.webRedirectURIs != nil && !slices.Contains(m.webRedirectURIs, terminalRedirectURL) {
		return nil, fmt.Errorf("to authorize from the terminal, register %s in this Web application OAuth client, or use a Desktop application client", terminalRedirectURL)
	}

	listener, err := net.Listen("tcp", terminalCallbackAddress)
	if err != nil {
		return nil, fmt.Errorf("listen for OAuth callback: %w", err)
	}

	// Generate random state for CSRF protection.
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("generate state: %w", err)
	}
	state := base64.URLEncoding.EncodeToString(stateBytes)
	verifier := oauth2.GenerateVerifier()

	// Start local server for callback
	codeChan := make(chan string, 1)
	errChan := make(chan error, 1)

	mux := http.NewServeMux()
	mux.Handle(callbackPath, m.newCallbackHandler(state, codeChan, errChan))
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := server.Serve(listener); err != http.ErrServerClosed {
			errChan <- err
		}
	}()

	defer func() { _ = server.Shutdown(ctx) }()

	// Generate auth URL with login_hint to pre-select account
	m.config.RedirectURL = terminalRedirectURL
	authOpts := []oauth2.AuthCodeOption{
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce,
		oauth2.S256ChallengeOption(verifier),
	}
	if email != "" {
		authOpts = append(authOpts,
			oauth2.SetAuthURLParam("login_hint", email))
	}
	authURL := m.config.AuthCodeURL(state, authOpts...)

	if launchBrowser {
		fmt.Printf("Opening browser for authorization...\n")
		fmt.Printf("If browser doesn't open, visit:\n%s\n\n", authURL)
		if err := openBrowser(ctx, authURL); err != nil {
			m.logger.Warn("failed to open browser", "error", err)
		}
	} else {
		fmt.Printf("\n=== Re-authorization required for %s ===\n\n", email)
		fmt.Printf("Open this URL in your browser and select the account %s:\n\n", email)
		fmt.Printf("  %s\n\n", authURL)
		fmt.Printf("Waiting for authorization...\n")
	}

	// Wait for callback
	select {
	case code := <-codeChan:
		token, err := m.config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
		if err != nil {
			return nil, fmt.Errorf("exchange authorization code: %w", err)
		}
		return token, nil
	case err := <-errChan:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

const resolveTimeout = 10 * time.Second

// refreshHTTPTimeout caps each token-endpoint HTTP call (refresh grant or
// service-account JWT exchange) made by token sources this package creates.
// oauth2 sources take their HTTP client from the creation context, and later
// refreshes run through the contextless TokenSource.Token() — invoked by
// oauth2.Transport before dispatching an API request — so a stalled token
// endpoint can only be bounded inside that client. The cap applies per call:
// when x/oauth2 does not know the endpoint's auth style it retries once with
// the other style, so one Token() performs at most two bounded calls. It is
// independent of any caller's request deadline.
const refreshHTTPTimeout = 30 * time.Second

// withRefreshHTTPClient returns ctx carrying a token-endpoint client whose
// Timeout is capped at refreshHTTPTimeout. The client already in ctx — or
// http.DefaultClient, oauth2's fallback — is shallow-copied so Transport,
// Jar and CheckRedirect are preserved and the original is never mutated; a
// client whose Timeout is already refreshHTTPTimeout or less is kept as is.
// The context's cancellation chain is untouched.
func withRefreshHTTPClient(ctx context.Context) context.Context {
	base := http.DefaultClient
	if c, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok {
		base = c
	}
	if base.Timeout > 0 && base.Timeout <= refreshHTTPTimeout {
		return ctx
	}
	capped := *base
	capped.Timeout = refreshHTTPTimeout
	return context.WithValue(ctx, oauth2.HTTPClient, &capped)
}

// resolveTokenEmail calls a Google profile endpoint to confirm that
// the token belongs to an account matching the expected email.
// Returns the canonical Google account email when available,
// which may differ from the input when the user supplies an alias
// or secondary login address. The token is never persisted by this
// function — the caller decides what to do on success or failure.
func (m *Manager) resolveTokenEmail(
	ctx context.Context, email string, token *oauth2.Token,
) (string, error) {
	endpoint := tokenProfileEndpointForScopes(m.config.Scopes)
	if m.profileURL != "" {
		endpoint.url = m.profileURL
	}
	ts := m.config.TokenSource(withRefreshHTTPClient(ctx), token)
	return fetchTokenProfileEmailFromEndpoint(ctx, ts, endpoint, email, tokenProfileErrorOAuth)
}

type tokenProfileEndpoint struct {
	url         string
	serviceName string
}

func tokenProfileEndpointForScopes(scopes []string) tokenProfileEndpoint {
	scopes = normalizedScopeList(scopes)
	if slices.Contains(scopes, ScopeUserinfoEmail) {
		return tokenProfileEndpoint{url: "https://www.googleapis.com/oauth2/v2/userinfo", serviceName: "Google account API"}
	}
	if slices.Contains(scopes, ScopeCalendarReadonly) && !hasGmailProfileScope(scopes) {
		return tokenProfileEndpoint{
			url:         defaultCalendarProfileURL,
			serviceName: "Calendar API",
		}
	}
	return tokenProfileEndpoint{
		url:         defaultProfileURL,
		serviceName: "Gmail API",
	}
}

func hasGmailProfileScope(scopes []string) bool {
	if slices.Contains(scopes, ScopeGmailFull) {
		return true
	}
	for _, scope := range Scopes {
		if slices.Contains(scopes, scope) {
			return true
		}
	}
	return false
}

func grantedScopesFromToken(token *oauth2.Token, requested []string) []string {
	var scopes []string
	switch raw := token.Extra("scope").(type) {
	case string:
		scopes = strings.Fields(raw)
	case []string:
		scopes = raw
	case []any:
		for _, v := range raw {
			if s, ok := v.(string); ok {
				scopes = append(scopes, s)
			}
		}
	}
	if len(scopes) == 0 {
		scopes = requested
	}
	return normalizedScopeList(scopes)
}

// canonicalScope maps Google's OpenID Connect aliases to their OAuth names.
// Other scopes, including openid, retain their identity and permissions.
func canonicalScope(scope string) string {
	switch scope = strings.TrimSpace(scope); scope {
	case "email":
		return ScopeUserinfoEmail
	case "profile":
		return "https://www.googleapis.com/auth/userinfo.profile"
	default:
		return scope
	}
}

func normalizedScopeList(scopes []string) []string {
	out := make([]string, 0, len(scopes))
	seen := map[string]struct{}{}
	for _, scope := range scopes {
		scope = canonicalScope(scope)
		if scope == "" {
			continue
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		out = append(out, scope)
	}
	return out
}

func missingScopes(required, granted []string) []string {
	grantedSet := map[string]struct{}{}
	for _, scope := range normalizedScopeList(granted) {
		grantedSet[scope] = struct{}{}
	}
	var missing []string
	for _, scope := range normalizedScopeList(required) {
		if _, ok := grantedSet[scope]; !ok {
			missing = append(missing, scope)
		}
	}
	return missing
}

// SameGoogleAccount returns true if two email addresses belong to the
// same Google account. This covers the common alias cases:
//   - exact match (case-insensitive)
//   - gmail.com dot-insensitive (first.last@gmail.com == firstlast@gmail.com)
//   - gmail.com plus-address (user+tag@gmail.com == user@gmail.com)
//   - googlemail.com ↔ gmail.com equivalence
//
// For Google Workspace domains we cannot verify aliases without an
// admin API call, so we fall back to exact-match only.
func SameGoogleAccount(expected, canonical string) bool {
	if strings.EqualFold(expected, canonical) {
		return true
	}

	// Normalize gmail.com / googlemail.com addresses for comparison
	expectedNorm := normalizeGmailAddress(expected)
	canonicalNorm := normalizeGmailAddress(canonical)

	return expectedNorm != "" && expectedNorm == canonicalNorm
}

// normalizeGmailAddress returns a canonical form of a gmail.com or
// googlemail.com address by lowercasing, stripping +suffixes and dots
// from the local part, and mapping googlemail.com → gmail.com.
// Returns "" for non-Gmail addresses.
func normalizeGmailAddress(email string) string {
	local, domain, ok := strings.CutLast(email, "@")
	if !ok {
		return ""
	}
	local = strings.ToLower(local)
	domain = strings.ToLower(domain)

	if domain != "gmail.com" && domain != "googlemail.com" {
		return ""
	}

	// Gmail ignores dots and +suffixes in the local part
	if plus := strings.Index(local, "+"); plus >= 0 {
		local = local[:plus]
	}
	local = strings.ReplaceAll(local, ".", "")
	return local + "@gmail.com"
}

// tokenFile wraps an OAuth2 token with metadata about the scopes and
// client it was authorized with. This enables proactive scope checking
// (e.g., detecting that deletion requires re-authorization) and client
// identity verification (detecting that an OAuth app switch requires
// re-authorization) without making an API call first.
type tokenFile struct {
	oauth2.Token

	// snapshot is the exact file read before a refresh starts.
	snapshot       []byte
	snapshotExists bool

	Scopes   []string `json:"scopes,omitempty"`
	ClientID string   `json:"client_id,omitempty"`
}

func (m *Manager) loadTokenFile(ctx context.Context, email string) (*tokenFile, error) {
	data, err := m.tokenStore.Read(ctx, email)
	if err != nil {
		return nil, err
	}
	tf, err := m.parseTokenFile(data)
	if err != nil {
		return nil, err
	}
	tf.snapshot = data
	tf.snapshotExists = true
	return &tf, nil
}

// parseTokenFile reserves ErrInvalidTokenJSON for token files, which a new
// sign-in may replace. Unusable command output is the wrapper's failure.
func (m *Manager) parseTokenFile(data []byte) (tokenFile, error) {
	var tf tokenFile
	err := json.Unmarshal(data, &tf)
	if m.CommandTokens() && (err != nil || (tf.AccessToken == "" && tf.RefreshToken == "")) {
		return tokenFile{}, errors.New("invalid token JSON from command; " +
			"fix [oauth.tokens].read_command or the stored token JSON, then retry")
	}
	if err != nil {
		return tokenFile{}, fmt.Errorf("%w: %w", ErrInvalidTokenJSON, err)
	}
	tf.Scopes = normalizedScopeList(tf.Scopes)
	return tf, nil
}

// metadataTokenFile reuses a checked authorization selection. The backend is
// reread by prepareAuthorizationContext and every compare-before-save operation.
func (m *Manager) metadataTokenFile(ctx context.Context, email string) (*tokenFile, error) {
	selection := m.authorizationExpected
	if selection == nil || selection.email != email {
		return m.loadTokenFile(ctx, email)
	}
	if !selection.snapshot.exists {
		return nil, os.ErrNotExist
	}
	tf, err := m.parseTokenFile(selection.snapshot.data)
	if err != nil {
		return nil, err
	}
	tf.snapshot, tf.snapshotExists = selection.snapshot.data, true
	return &tf, nil
}

// TokenMatchesClient returns true if the stored token for the given email
// was minted by this manager's OAuth client. Returns false if the token
// doesn't exist, has no client_id metadata (legacy token), or was minted
// by a different client.
func (m *Manager) TokenMatchesClient(ctx context.Context, email string) bool {
	info, err := m.tokenInfo(m.metadataTokenFile(ctx, email))
	return err == nil && info.ClientMatches
}

// TokenIssuedByDifferentClient reports whether the stored token is known to
// have come from a different OAuth client than this manager's.
//
// It differs from !TokenMatchesClient in what it does with uncertainty. That
// helper answers "can this token be trusted for this client", so an unreadable
// token or one with no recorded client_id is a no. Callers asking the opposite
// question — "is this definitely a different client" — must not read those same
// cases as a yes, or an unknown provenance becomes a positive claim about it.
func (m *Manager) TokenIssuedByDifferentClient(ctx context.Context, email string) bool {
	info, err := m.tokenInfo(m.metadataTokenFile(ctx, email))
	return err == nil && info.DifferentClient
}

// HasScopeMetadata returns true if the token file for this account has any
// scope metadata stored. Legacy tokens (saved before scope tracking) return false.
func (m *Manager) HasScopeMetadata(ctx context.Context, email string) bool {
	tf, err := m.metadataTokenFile(ctx, email)
	if err != nil {
		return false
	}
	return len(tf.Scopes) > 0
}

// HasScope checks if the stored token for the given email was authorized
// with the specified scope. Returns false if the token doesn't exist or
// doesn't have scope metadata (legacy tokens saved before scope tracking).
func (m *Manager) HasScope(ctx context.Context, email, scope string) bool {
	info, err := m.tokenInfo(m.metadataTokenFile(ctx, email))
	return err == nil && info.HasScope(scope)
}

// GrantedScopes returns a copy of the stored scope metadata for the account.
// Legacy tokens or missing token files return nil.
func (m *Manager) GrantedScopes(ctx context.Context, email string) []string {
	tf, err := m.metadataTokenFile(ctx, email)
	if err != nil || len(tf.Scopes) == 0 {
		return nil
	}
	return append([]string(nil), tf.Scopes...)
}

// saveTokenComparedContext writes a token only if its original snapshot is still current.
// The lock covers comparison and replacement, never the network exchange, and
// coordinates separate managers and CLI/daemon processes sharing this account.
func (m *Manager) saveTokenComparedContext(
	ctx context.Context, email string, token *oauth2.Token, scopes []string, expected *tokenFile,
) error {
	tf := tokenFile{Token: *token, Scopes: normalizedScopeList(scopes), ClientID: m.config.ClientID}
	data, err := json.Marshal(tf, jsontext.WithIndent("  "), json.Deterministic(true))
	if err != nil {
		return err
	}
	var snapshot *tokenSnapshot
	if expected != nil {
		snapshot = &tokenSnapshot{data: expected.snapshot, exists: expected.snapshotExists}
	}
	if err := m.tokenStore.replace(ctx, email, data, snapshot); err != nil {
		return err
	}
	tf.snapshot = data
	tf.snapshotExists = true
	if expected != nil {
		*expected = tf
	}
	m.updateAuthorizationExpected(email, tokenSnapshot{data: data, exists: true})
	return nil
}

// updateAuthorizationExpected records a write in place. Managers derived with
// WithScopes share the selection, so the caller sees a save made by its copy.
func (m *Manager) updateAuthorizationExpected(email string, snapshot tokenSnapshot) {
	if m.authorizationExpected != nil && m.authorizationExpected.email == email {
		m.authorizationExpected.snapshot = snapshot
	}
}

// tokenPath returns the path to the token file for an email.
func (m *Manager) tokenPath(email string) string {
	return safeTokenFilePath(m.tokensDir, email)
}

// safeTokenFilePath sanitizes email to prevent path traversal and refuses
// symlinks that leave tokensDir.
func safeTokenFilePath(tokensDir, email string) string {
	safe := sanitizeEmail(email)

	// Ensure the final path is within tokensDir
	path := filepath.Join(tokensDir, safe+".json")
	cleanPath := filepath.Clean(path)
	cleanTokensDir := filepath.Clean(tokensDir)

	// Verify the path is still within tokensDir (using proper directory check
	// to avoid prefix attacks like tokensDir-evil matching tokensDir)
	if !hasPathPrefix(cleanPath, cleanTokensDir) {
		// If path escapes tokensDir, use a hash-based fallback
		return filepath.Join(tokensDir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(email))))
	}

	// Check if path is a symlink that could escape tokensDir.
	// Note: There is an inherent TOCTOU (time-of-check to time-of-use) race between
	// this check and when the token is actually written. An attacker could create a
	// symlink after this check passes but before the write occurs. However, exploiting
	// this would require the attacker to have write access to the tokens directory and
	// precise timing, making it difficult to exploit in practice.
	if info, err := os.Lstat(cleanPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		// Path exists and is a symlink - resolve it and verify it stays within tokensDir
		resolved, err := filepath.EvalSymlinks(cleanPath)
		if err != nil || !isPathWithinDir(resolved, cleanTokensDir) {
			// Symlink resolution failed or escapes tokensDir - use hash-based fallback
			return filepath.Join(tokensDir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(email))))
		}
	}

	return cleanPath
}

// hasPathPrefix checks if path is equal to or a child of dir.
// This prevents prefix attacks like tokensDir-evil matching tokensDir,
// and correctly handles filesystem roots (/, C:\).
// Does not resolve symlinks - use isPathWithinDir when symlink resolution is needed.
func hasPathPrefix(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	// rel must not escape via ".." and must not be absolute
	if rel == "." {
		return true
	}
	if filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// isPathWithinDir checks if path is within dir, resolving symlinks in dir.
// Use this when checking resolved symlink targets.
func isPathWithinDir(path, dir string) bool {
	// Resolve symlinks in dir to get the real base directory
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		resolvedDir = dir // fallback to original if dir doesn't exist yet
	}
	return hasPathPrefix(filepath.Clean(path), filepath.Clean(resolvedDir))
}

// scopesToString joins scopes with spaces.
func scopesToString(scopes []string) string {
	return strings.Join(scopes, " ")
}

// validateBrowserURL checks that rawURL is a valid http or https URL.
// Returns an error for invalid URLs or disallowed schemes.
func validateBrowserURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("refused to open URL with scheme %q: only http and https are allowed", parsed.Scheme)
	}
	return nil
}

// openBrowser opens the default browser to the given URL.
// Only http and https URLs are allowed to prevent command injection
// via dangerous URL schemes (e.g., file://, custom protocol handlers).
func openBrowser(ctx context.Context, rawURL string) error {
	if err := validateBrowserURL(rawURL); err != nil {
		return err
	}

	// rawURL is validated above; pass it directly to the OS opener.
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", rawURL) //nolint:gosec // rawURL passed validateBrowserURL above
	case "linux":
		cmd = exec.CommandContext(ctx, "xdg-open", rawURL) //nolint:gosec // rawURL passed validateBrowserURL above
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", rawURL) //nolint:gosec // rawURL passed validateBrowserURL above
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start browser command: %w", err)
	}
	return nil
}

// parseClientSecrets parses Google OAuth client secrets JSON.
// Requires credentials with redirect_uris (Desktop app or Web app).
// TV/device clients are not supported (device flow doesn't work with Gmail).
func parseClientSecrets(data []byte, scopes []string) (*oauth2.Config, []string, error) {
	var secrets struct {
		Installed *struct {
			RedirectURIs []string `json:"redirect_uris"`
		} `json:"installed"`
		Web *struct {
			RedirectURIs []string `json:"redirect_uris"`
		} `json:"web"`
	}
	if err := json.Unmarshal(data, &secrets); err != nil {
		return nil, nil, fmt.Errorf("parse OAuth client secrets: %w", err)
	}
	config, err := google.ConfigFromJSON(data, normalizedScopeList(scopes)...)
	if err != nil {
		// Check if it's a client missing redirect_uris (TV/device or misconfigured)
		missingRedirects := (secrets.Installed != nil && len(secrets.Installed.RedirectURIs) == 0) ||
			(secrets.Web != nil && len(secrets.Web.RedirectURIs) == 0)
		if missingRedirects {
			return nil, nil, errors.New("OAuth client is missing redirect_uris (TV/device clients are not supported - Gmail doesn't work with device flow). Please create a 'Desktop application' or 'Web application' OAuth client in Google Cloud Console")
		}
		return nil, nil, fmt.Errorf("parse OAuth client secrets: %w", err)
	}
	if secrets.Web != nil {
		return config, secrets.Web.RedirectURIs, nil
	}
	return config, nil, nil
}

// TokenFilePath returns the token file path for an email within the
// given tokens directory. Use this when you need the path without a
// full Manager instance (e.g., cleanup during account removal).
func TokenFilePath(tokensDir, email string) string {
	safe := sanitizeEmail(email)
	return filepath.Join(tokensDir, safe+".json")
}

type tokenProfileErrorMode int

const (
	tokenProfileErrorOAuth tokenProfileErrorMode = iota
	tokenProfileErrorServiceAccount
)

func fetchTokenProfileEmail(
	ctx context.Context,
	ts oauth2.TokenSource,
	profileURL string,
	email string,
	mode tokenProfileErrorMode,
) (string, error) {
	return fetchTokenProfileEmailFromEndpoint(ctx, ts, tokenProfileEndpoint{
		url:         profileURL,
		serviceName: "gmail API",
	}, email, mode)
}

func fetchTokenProfileEmailFromEndpoint(
	ctx context.Context,
	ts oauth2.TokenSource,
	endpoint tokenProfileEndpoint,
	email string,
	mode tokenProfileErrorMode,
) (string, error) {
	valCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()

	client := oauth2.NewClient(valCtx, ts)
	req, err := http.NewRequestWithContext(valCtx, http.MethodGet, endpoint.url, nil)
	if err != nil {
		return "", fmt.Errorf("create profile request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		err = authorizationProviderError(err, nil, "")
		if mode == tokenProfileErrorOAuth {
			return "", fmt.Errorf(
				"could not verify token belongs to %s: %w "+
					"(re-run the command to try again)", email, err)
		}
		return "", fmt.Errorf("verify access to %s: %w", email, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		providerErr := fmt.Errorf("%s returned HTTP %d: %s", endpoint.serviceName, resp.StatusCode, string(body))
		providerErr = authorizationProviderError(providerErr, resp, "")
		if mode == tokenProfileErrorOAuth {
			return "", fmt.Errorf(
				"could not verify token belongs to %s: %w "+
					"(re-run the command to try again)", email, providerErr)
		}
		return "", fmt.Errorf("verify access to %s: %w", email, providerErr)
	}

	var profile struct {
		EmailAddress string `json:"emailAddress"`
		Email        string `json:"email"`
		ID           string `json:"id"`
	}
	if err := json.UnmarshalRead(resp.Body, &profile); err != nil {
		err = authorizationProviderError(err, nil, "")
		if mode == tokenProfileErrorOAuth {
			return "", fmt.Errorf(
				"could not verify token belongs to %s: "+
					"failed to parse profile response: %w "+
					"(re-run the command to try again)", email, err)
		}
		return "", fmt.Errorf("parse profile for %s: %w", email, err)
	}

	profileEmail := profile.EmailAddress
	if profileEmail == "" {
		profileEmail = profile.Email
	}
	if profileEmail == "" {
		profileEmail = profile.ID
	}
	if profileEmail == "" {
		missing := authorizationProviderError(errors.New("profile response did not include an email address"), nil, "")
		if mode == tokenProfileErrorOAuth {
			return "", fmt.Errorf(
				"could not verify token belongs to %s: %w "+
					"(re-run the command to try again)", email, missing)
		}
		return "", fmt.Errorf("parse profile for %s: %w", email, missing)
	}

	if !SameGoogleAccount(email, profileEmail) {
		return "", &TokenMismatchError{Expected: email, Actual: profileEmail}
	}

	return profileEmail, nil
}

// ValidateTokenEmail calls the Gmail profile API to confirm that the token
// source can access the given email account. Used by service account flows
// where no Manager is available.
func ValidateTokenEmail(ctx context.Context, ts oauth2.TokenSource, email string) error {
	_, err := fetchTokenProfileEmail(ctx, ts, defaultProfileURL, email, tokenProfileErrorServiceAccount)
	return err
}

// revokeTimeout bounds the revocation request; revocation is a single POST
// and should not hang a CLI command on a stalled connection.
const revokeTimeout = 15 * time.Second

const defaultRevokeURL = "https://oauth2.googleapis.com/revoke"

// ErrRevokeCredentialInvalid reports that the revocation endpoint rejected
// the stored credential as already expired or revoked. Callers treating
// revocation as cleanup (account removal) can read it as nothing-to-do.
var ErrRevokeCredentialInvalid = errors.New("stored credential is already expired or revoked")

// RevokeToken revokes the stored grant at Google's revocation endpoint.
// Revoking the refresh token invalidates the grant server-side, so copies of
// the token file (backups, other hosts, previously exposed credentials) lose
// access too — deleting the local file alone would not achieve that.
func (m *Manager) RevokeToken(ctx context.Context, email string) error {
	tf, err := m.loadTokenFile(ctx, email)
	if err != nil {
		return fmt.Errorf("load token for %s: %w", email, err)
	}
	credential := tf.RefreshToken
	if credential == "" {
		credential = tf.AccessToken
	}
	if credential == "" {
		return fmt.Errorf("token for %s holds no credential to revoke", email)
	}

	endpoint := m.revokeURL
	if endpoint == "" {
		endpoint = defaultRevokeURL
	}

	reqCtx, cancel := context.WithTimeout(ctx, revokeTimeout)
	defer cancel()
	form := url.Values{"token": {credential}}
	req, err := http.NewRequestWithContext(
		reqCtx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("create revocation request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("revoke token for %s: %w", email, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusBadRequest {
		var apiErr struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Error == "invalid_token" {
			return fmt.Errorf("revoke token for %s: %w", email, ErrRevokeCredentialInvalid)
		}
	}
	return fmt.Errorf(
		"revoke token for %s: revocation endpoint returned HTTP %d: %s",
		email, resp.StatusCode, strings.TrimSpace(string(body)))
}

func findEquivalentTokenEmails(tokensDir, email string) []string {
	entries, err := os.ReadDir(tokensDir)
	if err != nil {
		return nil
	}
	own := sanitizeEmail(email) + ".json"
	ownInfo, ownErr := os.Stat(filepath.Join(tokensDir, own))
	var equivalents []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || name == own {
			continue
		}
		if ownErr == nil {
			if info, infoErr := entry.Info(); infoErr == nil && os.SameFile(ownInfo, info) {
				continue
			}
		}
		stored := strings.TrimSuffix(name, ".json")
		if SameGoogleAccount(email, stored) {
			equivalents = append(equivalents, stored)
		}
	}
	return equivalents
}

// DeleteToken removes the token file for the given email.
func (m *Manager) DeleteToken(ctx context.Context, email string) error {
	err := m.tokenStore.Delete(ctx, email)
	if err == nil {
		m.updateAuthorizationExpected(email, tokenSnapshot{})
	}
	return err
}

// TokenPath returns the path to the token file for an email (for external use).
func (m *Manager) TokenPath(email string) string {
	return m.tokenStore.path(email)
}
