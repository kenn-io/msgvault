package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/oauth"
	extOAuth2 "golang.org/x/oauth2"
)

func TestErrOAuthNotConfigured(t *testing.T) {
	err := errOAuthNotConfigured()

	if err == nil {
		t.Fatal("errOAuthNotConfigured() = nil, want error")
	}

	msg := err.Error()

	// Should contain the main message
	if !strings.Contains(msg, "OAuth client secrets not configured") {
		t.Errorf("error message missing 'not configured': %q", msg)
	}

	// Should contain either:
	// 1. A "Found OAuth credentials" hint (if client_secret*.json exists on this machine)
	// 2. The setup URL (if no credentials found)
	hasFoundHint := strings.Contains(msg, "Found OAuth credentials at:")
	hasSetupURL := strings.Contains(msg, "https://msgvault.io/guides/oauth-setup/")

	if !hasFoundHint && !hasSetupURL {
		t.Errorf("error message missing both 'Found OAuth credentials' hint and setup URL: %q", msg)
	}

	// Should contain config file instructions (either "config.toml" or "<config file>" placeholder)
	if !strings.Contains(msg, "config") {
		t.Errorf("error message missing config reference: %q", msg)
	}
}

func TestWrapOAuthError_NotExist(t *testing.T) {
	originalErr := fmt.Errorf("open /path/to/secrets.json: %w", os.ErrNotExist)

	wrapped := wrapOAuthError(originalErr)

	msg := wrapped.Error()

	// Should contain accessible message (not "not found" anymore)
	if !strings.Contains(msg, "not accessible") {
		t.Errorf("error message missing 'not accessible': %q", msg)
	}

	// Should contain setup hint
	if !strings.Contains(msg, "https://msgvault.io/guides/oauth-setup/") {
		t.Errorf("error message missing setup URL: %q", msg)
	}
}

func TestWrapOAuthError_Permission(t *testing.T) {
	originalErr := fmt.Errorf("open /path/to/secrets.json: %w", os.ErrPermission)

	wrapped := wrapOAuthError(originalErr)

	msg := wrapped.Error()

	// Should contain accessible message
	if !strings.Contains(msg, "not accessible") {
		t.Errorf("error message missing 'not accessible': %q", msg)
	}

	// Should contain setup hint
	if !strings.Contains(msg, "https://msgvault.io/guides/oauth-setup/") {
		t.Errorf("error message missing setup URL: %q", msg)
	}
}

func TestWrapOAuthError_OtherError(t *testing.T) {
	originalErr := errors.New("some other error")

	wrapped := wrapOAuthError(originalErr)

	// Should return the original error unchanged
	if wrapped != originalErr {
		t.Errorf("wrapOAuthError() changed unrelated error: got %v, want %v", wrapped, originalErr)
	}
}

func TestWrapOAuthError_NestedNotExist(t *testing.T) {
	// Test that errors.Is can find nested os.ErrNotExist
	innerErr := fmt.Errorf("file error: %w", os.ErrNotExist)
	outerErr := fmt.Errorf("oauth manager: %w", innerErr)

	wrapped := wrapOAuthError(outerErr)

	msg := wrapped.Error()

	// Should detect the nested os.ErrNotExist and wrap appropriately
	if !strings.Contains(msg, "not accessible") {
		t.Errorf("failed to detect nested os.ErrNotExist: %q", msg)
	}
}

// newTestRootCmd creates a fresh root command for testing, avoiding mutation
// of the global rootCmd which could cause race conditions in parallel tests.
func newTestRootCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "msgvault",
		Short: "Offline email archive tool",
	}
}

// TestExecuteContext_CancellationPropagates verifies that context cancellation
// from ExecuteContext propagates to command handlers.
func TestExecuteContext_CancellationPropagates(t *testing.T) {
	// Track whether context was cancelled
	var contextWasCancelled atomic.Bool

	// Signal when the command handler has started waiting on ctx.Done()
	handlerStarted := make(chan struct{})

	// Create a fresh root command for this test
	testRoot := newTestRootCmd()

	// Create a test command that waits for context cancellation
	testCmd := &cobra.Command{
		Use:   "test-cancel",
		Short: "Test command for context cancellation",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			// Signal that we're now waiting for cancellation
			close(handlerStarted)
			select {
			case <-ctx.Done():
				contextWasCancelled.Store(true)
				return ctx.Err()
			case <-time.After(5 * time.Second):
				return nil
			}
		},
	}

	testRoot.AddCommand(testCmd)

	// Create a cancellable context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // Ensure cleanup even if test fails early

	// Start ExecuteContext in a goroutine
	done := make(chan error, 1)
	go func() {
		testRoot.SetArgs([]string{"test-cancel"})
		done <- testRoot.ExecuteContext(ctx)
	}()

	// Wait for handler to start (synchronization instead of sleep)
	select {
	case <-handlerStarted:
		// Handler is now waiting on ctx.Done()
	case <-time.After(2 * time.Second):
		t.Fatal("command handler did not start in time")
	}

	// Cancel the context (simulates SIGINT/SIGTERM)
	cancel()

	// Wait for execution to complete
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("expected context.Canceled error, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ExecuteContext did not return after context cancellation")
	}

	// Verify the command observed the cancellation
	if !contextWasCancelled.Load() {
		t.Error("command did not observe context cancellation")
	}
}

// TestExecute_UsesBackgroundContext verifies Execute() works with background context.
func TestExecute_UsesBackgroundContext(t *testing.T) {
	// Create a fresh root command for this test
	testRoot := newTestRootCmd()

	// Create a simple command that completes immediately
	completed := make(chan struct{})
	testCmd := &cobra.Command{
		Use:   "test-execute",
		Short: "Test command for Execute",
		RunE: func(cmd *cobra.Command, args []string) error {
			close(completed)
			return nil
		},
	}

	testRoot.AddCommand(testCmd)

	testRoot.SetArgs([]string{"test-execute"})
	err := testRoot.Execute()
	if err != nil {
		t.Fatalf("Execute() returned error: %v", err)
	}

	select {
	case <-completed:
		// Success
	case <-time.After(time.Second):
		t.Fatal("command did not complete")
	}
}

// TestExecuteContext_PropagatesContext verifies ExecuteContext passes context to command handlers.
//
// NOTE: This test modifies the package-level rootCmd variable and must NOT use t.Parallel().
// Running this test in parallel with other tests that access rootCmd would cause data races.
func TestExecuteContext_PropagatesContext(t *testing.T) {
	// Save and restore global rootCmd to avoid state leakage between tests.
	// This pattern requires sequential test execution - do not add t.Parallel().
	savedRootCmd := rootCmd
	defer func() { rootCmd = savedRootCmd }()

	// Create a test root command
	testRoot := newTestRootCmd()

	// Track the context received by the command
	type ctxKey string
	var receivedCtx context.Context
	testCmd := &cobra.Command{
		Use:   "test-ctx",
		Short: "Test command for context verification",
		RunE: func(cmd *cobra.Command, args []string) error {
			receivedCtx = cmd.Context()
			return nil
		},
	}
	testRoot.AddCommand(testCmd)

	// Replace global rootCmd for this test
	rootCmd = testRoot

	// Create a context with a custom value
	testKey := ctxKey("test-key")
	testValue := "test-value"
	ctx := context.WithValue(context.Background(), testKey, testValue)

	testRoot.SetArgs([]string{"test-ctx"})
	err := ExecuteContext(ctx)
	if err != nil {
		t.Fatalf("ExecuteContext returned unexpected error: %v", err)
	}

	// Verify the context was propagated
	if receivedCtx == nil {
		t.Fatal("command did not receive context")
	}
	if got := receivedCtx.Value(testKey); got != testValue {
		t.Errorf("context value mismatch: got %v, want %v", got, testValue)
	}
}

// TestExecute_UsesBackgroundContextInHandler verifies Execute provides background context to handlers.
//
// NOTE: This test modifies the package-level rootCmd variable and must NOT use t.Parallel().
// Running this test in parallel with other tests that access rootCmd would cause data races.
func TestExecute_UsesBackgroundContextInHandler(t *testing.T) {
	// Save and restore global rootCmd to avoid state leakage between tests.
	// This pattern requires sequential test execution - do not add t.Parallel().
	savedRootCmd := rootCmd
	defer func() { rootCmd = savedRootCmd }()

	// Create a test root command
	testRoot := newTestRootCmd()

	// Track the context received by the command
	var receivedCtx context.Context
	testCmd := &cobra.Command{
		Use:   "test-bg-ctx",
		Short: "Test command for background context",
		RunE: func(cmd *cobra.Command, args []string) error {
			receivedCtx = cmd.Context()
			return nil
		},
	}
	testRoot.AddCommand(testCmd)

	// Replace global rootCmd for this test
	rootCmd = testRoot

	testRoot.SetArgs([]string{"test-bg-ctx"})
	err := Execute()
	if err != nil {
		t.Fatalf("Execute returned unexpected error: %v", err)
	}

	// Verify the command received a non-nil context (should be background context)
	if receivedCtx == nil {
		t.Fatal("command did not receive context")
	}

	// Background context should not have any deadline
	if deadline, ok := receivedCtx.Deadline(); ok {
		t.Errorf("expected no deadline from background context, got %v", deadline)
	}

	// Background context should not be cancelled
	select {
	case <-receivedCtx.Done():
		t.Error("background context should not be done")
	default:
		// Expected: context is not done
	}
}

func TestIsAuthInvalidError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "generic error",
			err:  errors.New("something went wrong"),
			want: false,
		},
		{
			name: "invalid_grant RetrieveError",
			err:  &extOAuth2.RetrieveError{ErrorCode: "invalid_grant"},
			want: true,
		},
		{
			name: "other RetrieveError code",
			err:  &extOAuth2.RetrieveError{ErrorCode: "invalid_client"},
			want: false,
		},
		{
			name: "empty ErrorCode RetrieveError",
			err:  &extOAuth2.RetrieveError{},
			want: false,
		},
		{
			name: "wrapped invalid_grant",
			err: fmt.Errorf(
				"refresh token: %w",
				&extOAuth2.RetrieveError{ErrorCode: "invalid_grant"},
			),
			want: true,
		},
		{
			name: "network error",
			err: &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: errors.New("connection refused"),
			},
			want: false,
		},
		{
			name: "context.Canceled",
			err:  context.Canceled,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isAuthInvalidError(tt.err)
			if got != tt.want {
				t.Errorf("isAuthInvalidError() = %v, want %v", got, tt.want)
			}
		})
	}
}

// mockReauthorizer implements tokenReauthorizer for testing.
type mockReauthorizer struct {
	tokenSourceFn func(ctx context.Context, email string) (extOAuth2.TokenSource, error)
	hasTokenVal   bool
	authorizeFn   func(ctx context.Context, email string) error

	authorizeCount       int
	authorizeManualCount int

	// tokenSourceCall tracks how many times TokenSource was called,
	// allowing the mock to return different results on each call.
	tokenSourceCall int
}

func (m *mockReauthorizer) TokenSource(ctx context.Context, email string) (extOAuth2.TokenSource, error) {
	m.tokenSourceCall++
	return m.tokenSourceFn(ctx, email)
}

func (m *mockReauthorizer) HasToken(email string) bool {
	return m.hasTokenVal
}

func (m *mockReauthorizer) Authorize(ctx context.Context, email string) error {
	m.authorizeCount++
	if m.authorizeFn != nil {
		return m.authorizeFn(ctx, email)
	}
	return nil
}

func (m *mockReauthorizer) AuthorizeManual(ctx context.Context, email string) error {
	m.authorizeManualCount++
	if m.authorizeFn != nil {
		return m.authorizeFn(ctx, email)
	}
	return nil
}

// fakeTokenSource implements extOAuth2.TokenSource for tests.
type fakeTokenSource struct{}

func (fakeTokenSource) Token() (*extOAuth2.Token, error) {
	return &extOAuth2.Token{AccessToken: "fake"}, nil
}

func TestGetTokenSourceWithReauth(t *testing.T) {
	invalidGrant := &extOAuth2.RetrieveError{ErrorCode: "invalid_grant"}
	genericErr := errors.New("transient network error")

	tests := []struct {
		name                string
		mock                *mockReauthorizer
		interactive         bool
		wantErr             bool
		errContains         string
		wantAuthorize       int
		wantAuthorizeManual int
	}{
		{
			name: "token valid",
			mock: &mockReauthorizer{
				tokenSourceFn: func(_ context.Context, _ string) (extOAuth2.TokenSource, error) {
					return fakeTokenSource{}, nil
				},
				hasTokenVal: true,
			},
			interactive: true,
			wantErr:     false,
		},
		{
			name: "no token at all",
			mock: &mockReauthorizer{
				tokenSourceFn: func(_ context.Context, _ string) (extOAuth2.TokenSource, error) {
					return nil, errors.New("no token")
				},
				hasTokenVal: false,
			},
			interactive: true,
			wantErr:     true,
			errContains: "add-account",
		},
		{
			name: "transient error, token exists",
			mock: &mockReauthorizer{
				tokenSourceFn: func(_ context.Context, _ string) (extOAuth2.TokenSource, error) {
					return nil, genericErr
				},
				hasTokenVal: true,
			},
			interactive: true,
			wantErr:     true,
			errContains: "transient network error",
		},
		{
			name: "invalid_grant, interactive — manual reauth",
			mock: func() *mockReauthorizer {
				m := &mockReauthorizer{hasTokenVal: true}
				m.tokenSourceFn = func(_ context.Context, _ string) (extOAuth2.TokenSource, error) {
					if m.tokenSourceCall == 1 {
						return nil, fmt.Errorf("refresh: %w", invalidGrant)
					}
					return fakeTokenSource{}, nil
				}
				return m
			}(),
			interactive:         true,
			wantErr:             false,
			wantAuthorizeManual: 1,
		},
		{
			name: "invalid_grant, non-interactive",
			mock: &mockReauthorizer{
				tokenSourceFn: func(_ context.Context, _ string) (extOAuth2.TokenSource, error) {
					return nil, invalidGrant
				},
				hasTokenVal: true,
			},
			interactive: false,
			wantErr:     true,
			errContains: "non-interactive session",
		},
		{
			name: "invalid_grant, reauth fails",
			mock: &mockReauthorizer{
				tokenSourceFn: func(_ context.Context, _ string) (extOAuth2.TokenSource, error) {
					return nil, invalidGrant
				},
				hasTokenVal: true,
				authorizeFn: func(_ context.Context, _ string) error {
					return errors.New("browser flow failed")
				},
			},
			interactive:         true,
			wantErr:             true,
			errContains:         "browser flow failed",
			wantAuthorizeManual: 1,
		},
		{
			name: "invalid_grant, retry TokenSource fails",
			mock: func() *mockReauthorizer {
				m := &mockReauthorizer{hasTokenVal: true}
				m.tokenSourceFn = func(_ context.Context, _ string) (extOAuth2.TokenSource, error) {
					if m.tokenSourceCall == 1 {
						return nil, invalidGrant
					}
					return nil, errors.New("still broken")
				}
				return m
			}(),
			interactive:         true,
			wantErr:             true,
			errContains:         "after re-authorization",
			wantAuthorizeManual: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ts, err := getTokenSourceWithReauth(ctx, tt.mock, "test@gmail.com", tt.interactive)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("error %q should contain %q", err.Error(), tt.errContains)
				}
				if ts != nil {
					t.Error("expected nil token source on error")
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if ts == nil {
					t.Fatal("expected non-nil token source")
				}
			}

			if tt.mock.authorizeCount != tt.wantAuthorize {
				t.Errorf("Authorize called %d times, want %d",
					tt.mock.authorizeCount, tt.wantAuthorize)
			}
			if tt.mock.authorizeManualCount != tt.wantAuthorizeManual {
				t.Errorf("AuthorizeManual called %d times, want %d",
					tt.mock.authorizeManualCount, tt.wantAuthorizeManual)
			}
		})
	}

	// Verify that when AuthorizeManual returns a TokenMismatchError, the
	// error message includes recovery instructions for re-adding the account.
	t.Run("token mismatch error includes recovery instructions", func(t *testing.T) {
		mismatch := &oauth.TokenMismatchError{
			Expected: "user@example.com",
			Actual:   "other@example.com",
		}
		mock := &mockReauthorizer{
			hasTokenVal: true,
			tokenSourceFn: func(_ context.Context, _ string) (extOAuth2.TokenSource, error) {
				return nil, invalidGrant
			},
			authorizeFn: func(_ context.Context, _ string) error {
				return mismatch
			},
		}
		_, err := getTokenSourceWithReauth(context.Background(), mock, "user@example.com", true)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		msg := err.Error()
		for _, want := range []string{"remove-account", "add-account", "primary address"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error message missing %q: %q", want, msg)
			}
		}
		// Confirm the underlying TokenMismatchError is preserved.
		var mismatchErr *oauth.TokenMismatchError
		if !errors.As(err, &mismatchErr) {
			t.Errorf("expected error to wrap *oauth.TokenMismatchError, got %T: %v", err, err)
		}
	})

	// Additional assertion for non-interactive case: verify the error
	// mentions running from an interactive terminal
	t.Run("non-interactive error mentions interactive terminal", func(t *testing.T) {
		mock := &mockReauthorizer{
			tokenSourceFn: func(_ context.Context, _ string) (extOAuth2.TokenSource, error) {
				return nil, invalidGrant
			},
			hasTokenVal: true,
		}
		_, err := getTokenSourceWithReauth(context.Background(), mock, "x@gmail.com", false)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "interactive terminal") {
			t.Errorf("error should mention 'interactive terminal': %q", err.Error())
		}
	})
}
