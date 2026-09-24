package cmd

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Interactive setup wizard for first-run configuration",
	Long: `Interactive setup wizard to configure msgvault for first use.

This command helps you:
  1. Optionally configure Google OAuth credentials (Gmail and Google
     Calendar only; press Enter to skip)
  2. Optionally configure a remote NAS server for token export
  3. Create the config.toml file

Run this once after installing msgvault to get started quickly. Then run
"msgvault setup providers" to turn on search, attachment, and people lanes
from the API keys you have, and "msgvault setup status" to see what is on.`,
	Args: cobra.NoArgs,
	RunE: runSetup,
}

func init() {
	setupCmd.AddCommand(
		newSetupProvidersCommand(defaultSetupProvidersDeps()),
		newSetupStatusCommand(defaultSetupStatusDeps()),
	)
	rootCmd.AddCommand(setupCmd)
}

func runSetup(cmd *cobra.Command, args []string) error {
	reader := bufio.NewReader(cmd.InOrStdin())

	fmt.Println("Welcome to msgvault setup!")
	fmt.Println()

	// Ensure home directory exists
	if err := cfg.EnsureHomeDir(); err != nil {
		return fmt.Errorf("create home directory: %w", err)
	}

	// Step 1: Find or prompt for OAuth credentials
	secretsPath, err := setupOAuthSecrets(reader)
	if err != nil {
		return err
	}

	// Step 2: Optionally configure remote NAS
	remoteURL, remoteAPIKey, err := setupRemoteServer(reader, secretsPath)
	if err != nil {
		return err
	}

	// Step 3: Update config
	if secretsPath != "" {
		cfg.OAuth.ClientSecrets = secretsPath
	}
	if remoteURL != "" {
		cfg.Remote.URL = remoteURL
		cfg.Remote.APIKey = remoteAPIKey
		// Auto-set for HTTP: target is Tailscale/LAN, not public internet.
		if strings.HasPrefix(remoteURL, "http://") {
			cfg.Remote.AllowInsecure = true
		}
	}

	// Only save if we configured something
	if secretsPath != "" || remoteURL != "" {
		if err := cfg.Save(); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		fmt.Printf("\nConfiguration saved to %s\n", cfg.ConfigFilePath())
	}

	printSetupNextSteps(cmd.OutOrStdout(), setupAddAccountCommand(&cfg.OAuth), remoteURL != "",
		cfg.OAuth.ClientSecrets != "" && cfg.OAuth.ServiceAccountKey == "")
	return nil
}

// setupAddAccountCommand returns the add-account invocation that works
// with the configured Google credentials, or "" when there are none.
// Without a default credential, add-account needs --oauth-app to pick a
// named app.
func setupAddAccountCommand(o *config.OAuthConfig) string {
	const base = "msgvault add-account you@gmail.com"
	if o.ClientSecrets != "" || o.ServiceAccountKey != "" {
		return base
	}
	names := slices.Sorted(maps.Keys(o.Apps))
	for _, name := range names {
		if app := o.Apps[name]; app.ClientSecrets != "" || app.ServiceAccountKey != "" {
			return base + " --oauth-app " + oauth.ShellQuote(name)
		}
	}
	return ""
}

// printSetupNextSteps prints the closing steps. bundleHasSecrets reports
// whether the NAS bundle carries the credential add-account uses. The
// bundle copies only the default [oauth] client_secrets, and add-account
// prefers a service account key, which leaves no token to export.
func printSetupNextSteps(w io.Writer, addAccountCmd string, hasRemote, bundleHasSecrets bool) {
	var b strings.Builder
	b.WriteString("\nSetup complete! Next steps:\n\n")
	if addAccountCmd != "" {
		b.WriteString("  1. Add a Gmail account:\n")
		b.WriteString("     " + addAccountCmd + "\n\n")
		b.WriteString("  2. Sync your emails:\n")
		b.WriteString("     msgvault sync-full you@gmail.com\n\n")
		switch {
		case hasRemote && bundleHasSecrets:
			b.WriteString("  3. Export token to your NAS (after add-account):\n")
			b.WriteString("     msgvault export-token you@gmail.com\n\n")
		case hasRemote:
			b.WriteString("  This account cannot be exported to the NAS: export-token needs a\n")
			b.WriteString("  token from the default [oauth] client_secrets, the only credential\n")
			b.WriteString("  the NAS bundle carries.\n\n")
		}
	} else {
		b.WriteString("  Add a source, for example:\n")
		b.WriteString("     msgvault add-imap --host imap.example.com --username you@example.com\n")
		b.WriteString("     msgvault add-o365 you@example.com\n")
		// A configured remote would receive this local file path.
		if !hasRemote {
			b.WriteString("     msgvault import-mbox you@example.com /path/to/export.mbox\n")
		}
		b.WriteString("\n")
		b.WriteString("  Gmail needs a Google OAuth credential: run msgvault setup again when you have one.\n")
		b.WriteString("  Setup guide: https://msgvault.io/docs/setup/\n\n")
	}
	b.WriteString("For more help: msgvault --help\n")
	_, _ = io.WriteString(w, b.String())
}

func setupOAuthSecrets(reader *bufio.Reader) (string, error) {
	fmt.Println("Step 1: Google OAuth Credentials (Optional)")
	fmt.Println("--------------------------------------------")

	// Check if already configured
	if cfg.OAuth.ClientSecrets != "" {
		fmt.Printf("OAuth credentials already configured: %s\n", cfg.OAuth.ClientSecrets)
		if promptYesNo(reader, "Keep existing configuration?") {
			return "", nil
		}
	}

	fmt.Println()
	fmt.Println("Gmail and Google Calendar need a Google Cloud OAuth credential")
	fmt.Println("(client_secret.json). Other sources do not. Press Enter to skip.")
	fmt.Println()
	fmt.Println("To get one:")
	fmt.Println("  1. Go to https://console.cloud.google.com/apis/credentials")
	fmt.Println("  2. Create OAuth client ID (Desktop app)")
	fmt.Println("  3. Download the JSON file")
	fmt.Println()

	// Prompt for path
	fmt.Print("Path to client_secret.json: ")
	path, _ := reader.ReadString('\n')
	path = strings.TrimSpace(path)

	if path == "" {
		fmt.Println("Skipping Google OAuth credentials.")
		return "", nil
	}

	// Expand ~ in path
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, path[2:])
	}

	// Verify file exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return "", fmt.Errorf("file not found: %s", path)
	}

	fmt.Printf("Using: %s\n", path)
	return path, nil
}

func setupRemoteServer(reader *bufio.Reader, oauthSecretsPath string) (string, string, error) {
	fmt.Println()
	fmt.Println("Step 2: Remote NAS Server (Optional)")
	fmt.Println("-------------------------------------")
	fmt.Println("Configure a remote msgvault server to export tokens for headless deployment.")
	fmt.Println()

	// Check if already configured
	if cfg.Remote.URL != "" {
		fmt.Printf("Remote server already configured: %s\n", cfg.Remote.URL)
		if promptYesNo(reader, "Keep existing configuration?") {
			return cfg.Remote.URL, cfg.Remote.APIKey, nil
		}
	}

	if !promptYesNo(reader, "Configure remote NAS server?") {
		fmt.Println("Skipping remote server configuration.")
		return "", "", nil
	}

	// Get hostname/IP
	fmt.Print("Remote hostname or IP (e.g., nas, 192.168.1.100): ")
	host, _ := reader.ReadString('\n')
	host = strings.TrimSpace(host)

	if host == "" {
		fmt.Println("Skipping remote server configuration.")
		return "", "", nil
	}

	// Get port
	fmt.Print("Port [8080]: ")
	portStr, _ := reader.ReadString('\n')
	portStr = strings.TrimSpace(portStr)
	port := 8080
	if portStr != "" {
		if p, err := strconv.Atoi(portStr); err == nil && p > 0 && p < 65536 {
			port = p
		} else {
			fmt.Println("Invalid port, using default 8080")
		}
	}

	// HTTP, not HTTPS: target deployment is Tailscale or trusted LAN
	// where TLS terminates at the network layer. API key auth over
	// HTTP is acceptable in this threat model.
	url := "http://" + net.JoinHostPort(host, strconv.Itoa(port))

	// Auto-generate API key — printed so the user can copy it.
	// This is an interactive CLI session, not a logged pipeline.
	apiKey, err := generateAPIKey()
	if err != nil {
		return "", "", fmt.Errorf("generate API key: %w", err)
	}

	// codeql[go/clear-text-logging] -- setup is an interactive CLI flow and
	// intentionally prints the generated key once so the user can copy it.
	fmt.Printf("\nGenerated API key: %s\n", apiKey)

	// Create NAS deployment bundle
	// Use existing secrets path if user kept their current OAuth config
	effectiveSecrets := oauthSecretsPath
	if effectiveSecrets == "" {
		effectiveSecrets = cfg.OAuth.ClientSecrets
	}
	bundleDir := filepath.Join(cfg.HomeDir, "nas-bundle")
	if err := createNASBundle(bundleDir, apiKey, effectiveSecrets, port); err != nil {
		fmt.Printf("Warning: Could not create NAS bundle: %v\n", err)
	} else {
		fmt.Printf("\nNAS deployment files created in: %s\n", bundleDir)
		fmt.Println("  - config.toml (ready for NAS)")
		if effectiveSecrets != "" {
			fmt.Println("  - client_secret.json (copy of OAuth credentials)")
		}
		fmt.Println("  - docker-compose.yml (ready to deploy)")
		fmt.Println()
		fmt.Println("To deploy on your NAS:")
		fmt.Println("  1. Copy the nas-bundle folder to your NAS")
		fmt.Printf("  2. scp -r %s nas:/volume1/docker/msgvault\n", bundleDir)
		fmt.Println("  3. SSH to NAS and run: docker-compose up -d")
	}

	return url, apiKey, nil
}

func generateAPIKey() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("read random bytes for API key: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}

func createNASBundle(bundleDir, apiKey, oauthSecretsPath string, port int) error {
	// Create bundle directory
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		return fmt.Errorf("create bundle dir: %w", err)
	}

	// Create NAS config.toml
	oauthBlock := ""
	if oauthSecretsPath != "" {
		oauthBlock = "[oauth]\nclient_secrets = \"/data/client_secret.json\"\n\n"
	}
	nasConfig := fmt.Sprintf(`[server]
bind_addr = "0.0.0.0"
api_port = 8080
api_key = %q

%s[sync]
rate_limit_qps = 5

# Accounts will be added automatically when you export tokens.
# You can also add them manually:
# [[accounts]]
# email = "you@gmail.com"
# schedule = "0 2 * * *"
# enabled = true
`, apiKey, oauthBlock)

	configPath := filepath.Join(bundleDir, "config.toml")
	if err := os.WriteFile(configPath, []byte(nasConfig), 0600); err != nil {
		return fmt.Errorf("write config.toml: %w", err)
	}

	// Copy client_secret.json if available. Otherwise remove a copy left
	// by an earlier run, so the bundle ships no credential it does not use.
	destPath := filepath.Join(bundleDir, "client_secret.json")
	if oauthSecretsPath != "" {
		if err := copyFile(oauthSecretsPath, destPath); err != nil {
			return fmt.Errorf("copy client_secret.json: %w", err)
		}
	} else if err := os.Remove(destPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale client_secret.json: %w", err)
	}

	// Create docker-compose.yml
	dockerCompose := fmt.Sprintf(`services:
  msgvault:
    image: ghcr.io/kenn-io/msgvault:latest
    pull_policy: always
    container_name: msgvault
    user: root  # Required for Synology NAS ACLs
    restart: unless-stopped
    ports:
      - "%d:8080"
    volumes:
      - ./:/data
    environment:
      - TZ=America/Los_Angeles  # Adjust to your timezone
      - MSGVAULT_HOME=/data
    command: ["serve"]
    healthcheck:
      test: ["CMD", "wget", "-q", "--spider", "http://localhost:8080/health"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
`, port)

	composePath := filepath.Join(bundleDir, "docker-compose.yml")
	if err := os.WriteFile(composePath, []byte(dockerCompose), 0600); err != nil {
		return fmt.Errorf("write docker-compose.yml: %w", err)
	}

	return nil
}

func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = srcFile.Close() }()

	dstFile, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = dstFile.Close() }()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return err
	}

	// Secure permissions for credentials
	return os.Chmod(dst, 0600)
}

func promptYesNo(reader *bufio.Reader, prompt string) bool {
	fmt.Printf("%s [Y/n]: ", prompt)
	response, _ := reader.ReadString('\n')
	response = strings.ToLower(strings.TrimSpace(response))
	return response == "" || isYesAnswer(response)
}
