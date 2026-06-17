package cmd

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var (
	exportTokenTo       string
	exportTokenAPIKey   string
	exportAllowInsecure bool
)

var exportTokenCmd = &cobra.Command{
	Use:   "export-token <email>",
	Short: "Export OAuth token to a remote msgvault instance",
	Long: `Export an OAuth token to a remote msgvault server for headless deployment.

This command reads your local token and uploads it to a remote msgvault
instance via the API. Use this to set up msgvault on a NAS or server
without a browser.

SECURITY: HTTPS is required by default to protect OAuth tokens in transit.
Use --allow-insecure only for trusted local networks (e.g., Tailscale).

Environment variables:
  MSGVAULT_REMOTE_URL      Remote server URL (alternative to --to)
  MSGVAULT_REMOTE_API_KEY  API key (alternative to --api-key)

Examples:
  # Export token to NAS over HTTPS
  msgvault export-token user@gmail.com --to https://nas:8080 --api-key YOUR_KEY

  # Using environment variables
  export MSGVAULT_REMOTE_URL=https://nas:8080
  export MSGVAULT_REMOTE_API_KEY=your-key
  msgvault export-token user@gmail.com

  # With Tailscale (trusted network, HTTP allowed)
  msgvault export-token user@gmail.com --to http://homebase.tail49367.ts.net:8080 --api-key KEY --allow-insecure`,
	Args: cobra.ExactArgs(1),
	RunE: runExportToken,
}

func init() {
	exportTokenCmd.Flags().StringVar(&exportTokenTo, "to", "", "Remote msgvault URL (or MSGVAULT_REMOTE_URL env var)")
	exportTokenCmd.Flags().StringVar(&exportTokenAPIKey, "api-key", "", "API key (or MSGVAULT_REMOTE_API_KEY env var)")
	exportTokenCmd.Flags().BoolVar(&exportAllowInsecure, "allow-insecure", false, "Allow HTTP (insecure) connections for trusted networks")
	rootCmd.AddCommand(exportTokenCmd)
}

// tokenExporter uploads OAuth tokens to a remote msgvault server.
type tokenExporter struct {
	httpClient *http.Client
	tokensDir  string
	stdout     io.Writer
	stderr     io.Writer
}

// exportResult holds the resolved parameters after a successful export,
// so the caller can decide whether to persist them.
type exportResult struct {
	remoteURL     string
	apiKey        string
	allowInsecure bool
}

// export validates inputs, reads the local token, uploads it to the
// remote server, and registers the account.
func (e *tokenExporter) export(
	email, remoteURL, apiKey string, allowInsecure bool,
) (*exportResult, error) {
	// Parse and validate URL
	parsedURL, err := url.Parse(remoteURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if parsedURL.Scheme == "http" && !allowInsecure {
		return nil, errors.New("HTTPS required for security (OAuth tokens contain sensitive credentials)\n\n" +
			"Options:\n" +
			"  1. Use HTTPS: --to https://nas:8080\n" +
			"  2. For trusted networks (e.g., Tailscale): --allow-insecure")
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return nil, fmt.Errorf("URL scheme must be http or https, got: %s", parsedURL.Scheme)
	}

	if err := validateExportEmail(email); err != nil {
		return nil, err
	}

	// Read local token
	tokenPath := sanitizeExportTokenPath(e.tokensDir, email)
	if _, err := os.Stat(tokenPath); os.IsNotExist(err) {
		return nil, fmt.Errorf(
			"no token found for %s\n\nRun 'msgvault add-account %s' first to authenticate",
			email, email)
	}
	tokenData, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read token: %w", err)
	}

	baseURL := strings.TrimSuffix(remoteURL, "/")

	// Upload token
	_, _ = fmt.Fprintf(e.stdout, "Uploading token to %s...\n", remoteURL)
	if parsedURL.Scheme == "http" {
		_, _ = fmt.Fprintf(e.stderr, "WARNING: Sending credentials over insecure HTTP connection\n")
	}
	if err := e.uploadToken(baseURL, apiKey, email, tokenData); err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(e.stdout, "Token uploaded successfully for %s\n", email)

	// Register account (best-effort)
	e.addAccount(baseURL, apiKey, email)

	return &exportResult{
		remoteURL:     remoteURL,
		apiKey:        apiKey,
		allowInsecure: allowInsecure,
	}, nil
}

// uploadToken POSTs the token data to the remote server.
func (e *tokenExporter) uploadToken(
	baseURL, apiKey, email string, tokenData []byte,
) error {
	reqURL := baseURL + "/api/v1/auth/token/" + url.PathEscape(email)

	req, err := http.NewRequest(http.MethodPost, reqURL, bytes.NewReader(tokenData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", apiKey)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to remote server: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("upload failed (HTTP %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

// addAccount registers the email on the remote server. Failures are
// logged as warnings since the token upload already succeeded.
func (e *tokenExporter) addAccount(baseURL, apiKey, email string) {
	_, _ = fmt.Fprintf(e.stdout, "Adding account to remote config...\n")
	accountURL := baseURL + "/api/v1/accounts"
	accountBody := fmt.Sprintf(
		`{"email":%q,"schedule":"0 2 * * *","enabled":true}`, email)

	req, err := http.NewRequest(http.MethodPost, accountURL, strings.NewReader(accountBody))
	if err != nil {
		_, _ = fmt.Fprintf(e.stderr, "Warning: Could not create account request: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", apiKey)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		_, _ = fmt.Fprintf(e.stderr, "Warning: Could not add account to remote config: %v\n", err)
		return
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusCreated:
		_, _ = fmt.Fprintf(e.stdout, "Account added to remote config\n")
	case http.StatusOK:
		_, _ = fmt.Fprintf(e.stdout, "Account already configured on remote\n")
	default:
		_, _ = fmt.Fprintf(e.stderr,
			"Warning: Could not add account (HTTP %d): %s\n",
			resp.StatusCode, string(respBody))
	}
}

func runExportToken(_ *cobra.Command, args []string) error {
	email := args[0]

	// Resolution order: flag > env var > config file
	remoteURL := resolveParam(exportTokenTo, "MSGVAULT_REMOTE_URL", cfg.Remote.URL)
	apiKey := resolveParam(exportTokenAPIKey, "MSGVAULT_REMOTE_API_KEY", cfg.Remote.APIKey)

	if remoteURL == "" {
		return errors.New("remote URL required: use --to flag, MSGVAULT_REMOTE_URL env var, or [remote] url in config.toml")
	}
	if apiKey == "" {
		return errors.New("API key required: use --api-key flag, MSGVAULT_REMOTE_API_KEY env var, or [remote] api_key in config.toml")
	}

	exporter := &tokenExporter{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		tokensDir:  cfg.TokensDir(),
		stdout:     os.Stdout,
		stderr:     os.Stderr,
	}

	allowInsecure := exportAllowInsecure || cfg.Remote.AllowInsecure
	result, err := exporter.export(email, remoteURL, apiKey, allowInsecure)
	if err != nil {
		return err
	}

	// Save remote config for future use
	if cfg.Remote.URL != result.remoteURL ||
		cfg.Remote.APIKey != result.apiKey ||
		(result.allowInsecure && !cfg.Remote.AllowInsecure) {
		cfg.Remote.URL = result.remoteURL
		cfg.Remote.APIKey = result.apiKey
		if result.allowInsecure {
			cfg.Remote.AllowInsecure = true
		}
		if err := cfg.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "Note: Could not save remote config: %v\n", err)
		} else {
			fmt.Printf("Remote server saved to %s (future exports won't need --to/--api-key)\n",
				cfg.ConfigFilePath())
		}
	}

	fmt.Println("\nSetup complete! The remote server will sync daily at 2am.")
	fmt.Printf("To trigger an immediate sync:\n")
	fmt.Printf("  curl -X POST -H 'X-API-Key: ...' %s/api/v1/sync/%s\n",
		result.remoteURL, email)

	return nil
}

// resolveParam returns the first non-empty value from: flag, env var, config.
func resolveParam(flag, envKey, configVal string) string {
	if flag != "" {
		return flag
	}
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return configVal
}

// validateExportEmail checks that an email address is well-formed
// and doesn't contain path traversal characters.
func validateExportEmail(email string) error {
	if !strings.Contains(email, "@") || !strings.Contains(email, ".") {
		return fmt.Errorf("invalid email format: %s", email)
	}
	if strings.ContainsAny(email, "/\\") || strings.Contains(email, "..") {
		return errors.New("invalid email format: contains path characters")
	}
	return nil
}

// sanitizeExportTokenPath returns a safe file path for the token.
// Matches the server-side sanitizeTokenPath function in handlers.go.
func sanitizeExportTokenPath(tokensDir, email string) string {
	// Remove dangerous characters
	safe := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == '\x00' {
			return -1
		}
		return r
	}, email)

	// Build path and verify it's within tokensDir
	path := filepath.Join(tokensDir, safe+".json")
	cleanPath := filepath.Clean(path)
	cleanTokensDir := filepath.Clean(tokensDir)

	// If path escapes tokensDir, use hash-based fallback
	if !strings.HasPrefix(cleanPath, cleanTokensDir+string(os.PathSeparator)) {
		return filepath.Join(tokensDir,
			fmt.Sprintf("%x.json", sha256.Sum256([]byte(email))))
	}

	return cleanPath
}
