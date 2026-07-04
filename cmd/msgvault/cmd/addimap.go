package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/huh/v2"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/clirun"
	imapclient "go.kenn.io/msgvault/internal/imap"
)

// passwordMethod describes how to read the password.
type passwordMethod int

const (
	// passwordInteractive uses huh masked input with asterisk echo.
	passwordInteractive passwordMethod = iota
	// passwordNoPrompt means stdin is a TTY but no output TTY is
	// available for the prompt. Fail with a clear error.
	passwordNoPrompt
	// passwordPipe reads from piped (non-terminal) stdin.
	passwordPipe
)

// choosePasswordStrategy selects the password input method based on
// which file descriptors are terminals. Returns the method and, for
// passwordInteractive, the output file to render the TUI to.
func choosePasswordStrategy(
	stdinNative, stdinCygwin, stderrTTY, stdoutTTY bool,
) (passwordMethod, *os.File) {
	stdinTTY := stdinNative || stdinCygwin
	if !stdinTTY {
		return passwordPipe, nil
	}
	// Prefer stderr (keeps stdout clean); fall back to stdout.
	switch {
	case stderrTTY:
		return passwordInteractive, os.Stderr
	case stdoutTTY:
		return passwordInteractive, os.Stdout
	default:
		return passwordNoPrompt, nil
	}
}

var (
	imapHost                 string
	imapPort                 int
	imapUsername             string
	imapNoTLS                bool
	imapSTARTTLS             bool
	noDefaultIdentityAddImap bool
)

func newAddIMAPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-imap",
		Short: "Add an IMAP account",
		Long: `Add an IMAP email account using username/password authentication.

By default, connects using implicit TLS (IMAPS, port 993).
Use --starttls for STARTTLS upgrade on port 143.
Use --no-tls for a plain unencrypted connection (not recommended).

You will be prompted to enter your password interactively.
For scripting, pipe the password via stdin or set the environment variable:
  read -s PASS && echo "$PASS" | msgvault add-imap --host ... --username ...
  MSGVAULT_IMAP_PASSWORD="..." msgvault add-imap --host ... --username ...

Security note: Your password is stored on disk with restricted file
permissions (0600). For stronger security, use an app-specific password
instead of your primary account password.

Examples:
  msgvault add-imap --host imap.example.com --username user@example.com
  msgvault add-imap --host mail.example.com --port 993 --username user@example.com
  msgvault add-imap --host mail.example.com --username user@example.com --starttls
  msgvault add-imap --host mail.example.com --username user@example.com --no-tls`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if imapHost == "" {
				return usageErr(cmd, errors.New("--host is required"))
			}
			if imapUsername == "" {
				return usageErr(cmd, errors.New("--username is required"))
			}
			if imapNoTLS && imapSTARTTLS {
				return usageErr(cmd, errors.New("--no-tls and --starttls are mutually exclusive"))
			}
			if !isDaemonCLISubprocess() {
				password, err := readAddIMAPPassword(cmd, true)
				if err != nil {
					return err
				}
				return runDaemonCLICommandHTTPFromCobraWithEnv(cmd, args, map[string]string{
					clirun.EnvIMAPPassword: password,
				})
			}

			// Build IMAP config
			imapCfg := &imapclient.Config{
				Host:     imapHost,
				Port:     imapPort,
				TLS:      !imapNoTLS && !imapSTARTTLS,
				STARTTLS: imapSTARTTLS,
				Username: imapUsername,
			}

			password, err := readAddIMAPPassword(cmd, false)
			if err != nil {
				return err
			}

			// Test connection
			fmt.Printf("Testing connection to %s...\n", imapCfg.Addr())
			imapClient := imapclient.NewClient(imapCfg, password, imapclient.WithLogger(logger))
			profile, err := imapClient.GetProfile(cmd.Context())
			_ = imapClient.Close()
			if err != nil {
				return fmt.Errorf("connection test failed: %w", err)
			}
			fmt.Printf("Connected successfully as %s\n", profile.EmailAddress)

			s, cleanup, err := openWritableStoreAndInitForIngest()
			if err != nil {
				return err
			}
			defer cleanup()

			// Build identifier and save credentials
			identifier := imapCfg.Identifier()

			if err := imapclient.SaveCredentials(cfg.TokensDir(), identifier, password); err != nil {
				return fmt.Errorf("save credentials: %w", err)
			}

			// Create source record
			source, err := s.GetOrCreateSource(sourceTypeIMAP, identifier)
			if err != nil {
				return fmt.Errorf("create source: %w", err)
			}

			// Store config JSON
			cfgJSON, err := imapCfg.ToJSON()
			if err != nil {
				return fmt.Errorf("serialize config: %w", err)
			}
			if err := s.UpdateSourceSyncConfig(source.ID, cfgJSON); err != nil {
				return fmt.Errorf("store config: %w", err)
			}

			// Set display name from username
			if err := s.UpdateSourceDisplayName(source.ID, imapUsername); err != nil {
				return fmt.Errorf("set display name: %w", err)
			}

			// Auto-default-identity must run BEFORE the legacy migration
			// retry — see comment in account_identity.go.
			if !noDefaultIdentityAddImap {
				confirmDefaultIdentity(cmd.OutOrStdout(), s, source.ID, imapUsername, imapUsername, "account-identifier")
			}
			if err := runPostSourceCreateMigrations(s); err != nil {
				return fmt.Errorf("post-source-create migrations: %w", err)
			}

			fmt.Printf("\nIMAP account added successfully!\n")
			fmt.Printf("  Identifier: %s\n", identifier)
			fmt.Printf("  Note: Password stored on disk at %s\n", imapclient.CredentialsPath(cfg.TokensDir(), identifier))
			fmt.Println()
			fmt.Println("You can now run:")
			fmt.Printf("  msgvault sync-full %s\n", identifier)

			return nil
		},
	}
	cmd.Flags().StringVar(&imapHost, "host", "", "IMAP server hostname (required)")
	cmd.Flags().IntVar(&imapPort, "port", 0, "IMAP server port (default: 993 for TLS, 143 otherwise; matches defaults in internal/microsoft/imap package)")
	cmd.Flags().StringVar(&imapUsername, "username", "", "IMAP username / email address (required)")
	cmd.Flags().BoolVar(&imapNoTLS, "no-tls", false, "Disable TLS (plain connection, not recommended)")
	cmd.Flags().BoolVar(&imapSTARTTLS, "starttls", false, "Use STARTTLS instead of implicit TLS")
	cmd.Flags().BoolVar(&noDefaultIdentityAddImap, "no-default-identity", false, noDefaultIdentityHelp)
	return cmd
}

func readAddIMAPPassword(cmd *cobra.Command, announceEnv bool) (string, error) {
	if envPass := os.Getenv(clirun.EnvIMAPPassword); envPass != "" {
		if announceEnv {
			if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Using password from %s environment variable\n", clirun.EnvIMAPPassword); err != nil {
				return "", fmt.Errorf("write password source notice: %w", err)
			}
		}
		return envPass, nil
	}

	prompt := fmt.Sprintf("Password for %s@%s:", imapUsername, imapHost)
	method, promptOut := choosePasswordStrategy(
		isatty.IsTerminal(os.Stdin.Fd()),
		isatty.IsCygwinTerminal(os.Stdin.Fd()),
		isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd()),
		isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()),
	)
	switch method {
	case passwordInteractive:
		return readPasswordInteractive(prompt, promptOut)
	case passwordNoPrompt:
		return "", errors.New("cannot read password: no terminal available for prompt (try piping the password via stdin or setting MSGVAULT_IMAP_PASSWORD)")
	case passwordPipe:
		return readPasswordFromPipe(os.Stdin)
	default:
		return "", errors.New("cannot determine password input method")
	}
}

// readPasswordFromPipe reads a password from a non-terminal reader
// (e.g. piped stdin). Uses only the first line.
func readPasswordFromPipe(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		return "", errors.New("password is required")
	}
	password := strings.TrimRight(scanner.Text(), "\r\n")
	if strings.TrimSpace(password) == "" {
		return "", errors.New("password is required")
	}
	return password, nil
}

// readPasswordInteractive prompts for a password using a masked
// input field with asterisk echo. The output writer controls where
// the TUI renders (typically stderr to avoid polluting stdout).
func readPasswordInteractive(prompt string, output io.Writer) (string, error) {
	var password string
	input := huh.NewInput().
		Title(prompt).
		EchoMode(huh.EchoModePassword).
		Value(&password)
	err := huh.NewForm(huh.NewGroup(input)).
		WithOutput(output).
		Run()
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if strings.TrimSpace(password) == "" {
		return "", errors.New("password is required")
	}
	return password, nil
}

func init() {
	rootCmd.AddCommand(newAddIMAPCmd())
}
