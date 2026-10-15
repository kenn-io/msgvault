package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/msgvault/internal/clirun"
	matrixsource "go.kenn.io/msgvault/internal/matrix"
	"maunium.net/go/mautrix/crypto/ssss"
	"maunium.net/go/mautrix/crypto/utils"
)

var (
	addMatrixHomeserver           string
	addMatrixUserID               string
	addMatrixPasswordFile         string
	addMatrixLoginTokenFile       string
	addMatrixRecoveryFile         string
	addMatrixRecoveryIsPassphrase bool
	addMatrixSkipKeyBackup        bool
	noDefaultIdentityAddMatrix    bool
)

type matrixSyncPreferences struct {
	SkipKeyBackup bool                      `json:"skip_key_backup"`
	Extra         map[string]jsontext.Value `json:",embed"`
}

func newAddMatrixCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-matrix",
		Short: "Add a read-only Matrix account as an archive source",
		Long: `Add a Matrix account using a dedicated read-only msgvault device.

Password login is the default. For SSO accounts, obtain a single-use
m.login.token from the homeserver login flow and pass --login-token-file.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			if strings.TrimSpace(addMatrixHomeserver) == "" || strings.TrimSpace(addMatrixUserID) == "" {
				return errors.New("--homeserver and --user-id are required")
			}
			if addMatrixPasswordFile != "" && addMatrixLoginTokenFile != "" {
				return errors.New("use only one of --password-file and --login-token-file")
			}
			if addMatrixSkipKeyBackup && (addMatrixRecoveryFile != "" || addMatrixRecoveryIsPassphrase) {
				return errors.New("--skip-key-backup cannot be combined with a recovery secret")
			}
			tokensDir := state.cfg.TokensDir()
			if !isDaemonCLISubprocess() {
				// The daemon may be remote, so only it can tell whether a pending
				// renewal will make this secret unnecessary.
				loginSecret, err := readMatrixLoginSecret()
				if err != nil {
					return err
				}
				recoverySecret, err := readMatrixRecoverySecret()
				if err != nil {
					return err
				}
				return runDaemonCLICommandHTTPFromCobraWithEnv(cmd, args, map[string]string{
					clirun.EnvMatrixLoginSecret:    loginSecret,
					clirun.EnvMatrixRecoverySecret: recoverySecret,
				})
			}

			loginSecret := os.Getenv(clirun.EnvMatrixLoginSecret)
			return matrixsource.WithCredentialLifecycleLock(tokensDir, func() error {
				return addMatrixAccount(cmd, state, tokensDir, loginSecret)
			})
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&addMatrixHomeserver, "homeserver", "", "Matrix homeserver URL")
	flags.StringVar(&addMatrixUserID, "user-id", "", "full Matrix user ID (for example @archive:example.org)")
	flags.StringVar(&addMatrixPasswordFile, "password-file", "", "read the Matrix password from a file")
	flags.StringVar(&addMatrixLoginTokenFile, "login-token-file", "", "read a single-use m.login.token from a file")
	flags.StringVar(&addMatrixRecoveryFile, "recovery-file", "", "read the Matrix recovery key or passphrase from a file")
	flags.BoolVar(&addMatrixRecoveryIsPassphrase, "recovery-passphrase", false, "use a recovery passphrase instead of a recovery key")
	flags.BoolVar(&addMatrixSkipKeyBackup, "skip-key-backup", false, "archive encrypted placeholders without a saved backup key")
	flags.BoolVar(&noDefaultIdentityAddMatrix, "no-default-identity", false, noDefaultIdentityHelp)
	return cmd
}

// Test seams for injected credential write failures and logout timeouts.
var (
	saveMatrixCredentials        = matrixsource.SaveCredentials
	savePendingMatrixCredentials = matrixsource.SavePendingCredentials
	matrixRequestTimeout         = 10 * time.Second
)

// addMatrixAccount runs under the credential lifecycle lock. When it replaces
// an existing device, it saves the new login to a pending file before revoking
// the old one, so a failure at any step leaves one usable or resumable login.
func addMatrixAccount(cmd *cobra.Command, state *invocation, tokensDir, loginSecret string) error {
	ctx := cmd.Context()
	recoverySecret := os.Getenv(clirun.EnvMatrixRecoverySecret)
	var recoveryKey []byte
	if recoverySecret != "" && !addMatrixRecoveryIsPassphrase {
		recoveryKey = utils.DecodeBase58RecoveryKey(recoverySecret)
		if recoveryKey == nil {
			return fmt.Errorf("unlock Matrix secret storage: %w", ssss.ErrInvalidRecoveryKey)
		}
	}
	exists, err := matrixsource.CredentialsExist(tokensDir, addMatrixUserID)
	if err != nil {
		return err
	}
	var previous matrixsource.Credentials
	if exists {
		previous, err = matrixsource.LoadCredentials(tokensDir, addMatrixUserID)
		if err != nil {
			return fmt.Errorf("read the existing Matrix login before replacing it: %w (fix or remove that file, then retry)", err)
		}
	}
	creds, resumed, err := matrixsource.LoadPendingCredentials(tokensDir, addMatrixUserID)
	if err != nil {
		return fmt.Errorf("%w (fix or remove that file, then retry)", err)
	}
	if recoverySecret != "" || addMatrixSkipKeyBackup {
		creds.BackupKey = ""
	} else if !resumed {
		creds.BackupKey = previous.BackupKey
	}
	if recoverySecret == "" && !addMatrixSkipKeyBackup && creds.BackupKey == "" && !exists && !resumed {
		return errors.New("matrix recovery secret required; use --recovery-file or --skip-key-backup")
	}
	// keepDevice stays false until the new login is recorded on disk; until
	// then a failure revokes it.
	hadPending := resumed
	if resumed {
		checkCtx, cancel := context.WithTimeout(ctx, matrixRequestTimeout)
		err := matrixsource.CheckLogin(checkCtx, creds)
		cancel()
		switch {
		case matrixsource.IsUnknownToken(err):
			// A revoked pending login must never replace the working one.
			if err := matrixsource.DeletePendingCredentials(tokensDir, addMatrixUserID); err != nil {
				return fmt.Errorf("remove revoked pending Matrix login: %w", err)
			}
			resumed = false
		case err != nil:
			return fmt.Errorf("%w (login unchanged, retry add-matrix)", err)
		}
	}
	keepDevice := resumed
	staged := resumed
	if resumed {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Resuming the saved Matrix login for device %s\n", creds.DeviceID)
	} else {
		if loginSecret == "" {
			if hadPending {
				return errors.New("the saved Matrix login was revoked; run add-matrix again to log in")
			}
			return errors.New("missing Matrix login secret in daemon subprocess")
		}
		login, err := matrixsource.Login(ctx, addMatrixHomeserver, addMatrixUserID, loginSecret, addMatrixLoginTokenFile != "")
		if err != nil {
			return err
		}
		login.BackupKey = creds.BackupKey
		creds = login
	}
	defer func() {
		if keepDevice {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), matrixRequestTimeout)
		defer cancel()
		_ = matrixsource.Logout(cleanupCtx, creds)
	}()
	if recoverySecret != "" && !addMatrixSkipKeyBackup {
		creds.BackupKey, err = matrixsource.LoadBackupKey(ctx, creds, recoverySecret, addMatrixRecoveryIsPassphrase, recoveryKey)
		if err != nil {
			return err
		}
	}
	s, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
	if err != nil {
		return err
	}
	defer cleanup()
	source, err := s.GetOrCreateSource(sourceTypeMatrix, creds.UserID)
	if err != nil {
		return fmt.Errorf("create Matrix source: %w", err)
	}
	if cmd.Flags().Changed("skip-key-backup") || recoverySecret != "" {
		var preferences matrixSyncPreferences
		if source.SyncConfig.Valid {
			if err := json.Unmarshal([]byte(source.SyncConfig.String), &preferences); err != nil {
				return fmt.Errorf("read Matrix key backup preference: %w", err)
			}
		}
		preferences.SkipKeyBackup = addMatrixSkipKeyBackup
		encoded, err := json.Marshal(preferences)
		if err != nil {
			return fmt.Errorf("encode Matrix key backup preference: %w", err)
		}
		if err := s.UpdateSourceSyncConfig(source.ID, string(encoded)); err != nil {
			return fmt.Errorf("save Matrix key backup preference: %w", err)
		}
	}
	if err := s.UpdateSourceDisplayName(source.ID, "Matrix "+creds.UserID); err != nil {
		return fmt.Errorf("set Matrix source name: %w", err)
	}
	if !noDefaultIdentityAddMatrix {
		confirmDefaultIdentity(cmd.OutOrStdout(), s, source.ID, creds.UserID, creds.UserID, "account-identifier", state.logger)
	}
	if err := runPostSourceCreateMigrationsForInvocation(s, state); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}
	if resumed || (exists && previous.DeviceID != creds.DeviceID) {
		// Rewrite even a resumed login so it is durable before the old one goes.
		if err := savePendingMatrixCredentials(tokensDir, creds); err != nil {
			// A published pending file can still be resumed, so keep its device.
			keepDevice = keepDevice || errors.Is(err, atomicfile.ErrPublished)
			return fmt.Errorf("save the new Matrix login (login unchanged, retry add-matrix): %w", err)
		}
		keepDevice, staged = true, true
	}
	if exists {
		if previous.DeviceID != creds.DeviceID {
			logoutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), matrixRequestTimeout)
			err := matrixsource.Logout(logoutCtx, previous)
			cancel()
			if err != nil && !matrixsource.IsUnknownToken(err) {
				return fmt.Errorf("log out previous Matrix device %s (login unchanged, retry add-matrix to finish): %w", previous.DeviceID, err)
			}
		}
	}
	if err := saveMatrixCredentials(tokensDir, creds); err != nil {
		// A published file already replaced the old login, so keep its device.
		keepDevice = keepDevice || errors.Is(err, atomicfile.ErrPublished)
		if staged {
			return fmt.Errorf("%w (the new login is saved; retry add-matrix to finish)", err)
		}
		return err
	}
	keepDevice = true
	if err := matrixsource.DeletePendingCredentials(tokensDir, creds.UserID); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not remove the pending Matrix login file: %v\n", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Added Matrix account %s with device %s\n", creds.UserID, creds.DeviceID)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Run: msgvault sync-matrix")
	return nil
}

func readMatrixLoginSecret() (string, error) {
	path := addMatrixPasswordFile
	if addMatrixLoginTokenFile != "" {
		path = addMatrixLoginTokenFile
	}
	if path != "" {
		return readMatrixSecretFile(path, addMatrixLoginTokenFile != "")
	}
	method, output := choosePasswordStrategy(
		isatty.IsTerminal(os.Stdin.Fd()), isatty.IsCygwinTerminal(os.Stdin.Fd()),
		isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd()),
		isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()),
	)
	switch method {
	case passwordInteractive:
		return readPasswordInteractive("Matrix password:", output, false)
	case passwordPipe:
		return readPasswordFromPipe(os.Stdin)
	default:
		return "", errors.New("cannot read Matrix password: use --password-file or --login-token-file")
	}
}

func readMatrixRecoverySecret() (string, error) {
	if addMatrixSkipKeyBackup {
		return "", nil
	}
	if addMatrixRecoveryFile != "" {
		return readMatrixSecretFile(addMatrixRecoveryFile, !addMatrixRecoveryIsPassphrase)
	}
	method, output := choosePasswordStrategy(
		isatty.IsTerminal(os.Stdin.Fd()), isatty.IsCygwinTerminal(os.Stdin.Fd()),
		isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd()),
		isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()),
	)
	if method != passwordInteractive {
		if addMatrixRecoveryIsPassphrase {
			return "", errors.New("--recovery-passphrase requires --recovery-file when an interactive prompt is unavailable")
		}
		return "", nil
	}
	prompt := "Matrix recovery key"
	if addMatrixRecoveryIsPassphrase {
		prompt = "Matrix recovery passphrase"
	}
	prompt += ", blank keeps a saved backup key if available"
	secret, err := readPasswordInteractive(prompt+":", output, true)
	if err != nil {
		return "", fmt.Errorf("read Matrix recovery secret: %w", err)
	}
	if !addMatrixRecoveryIsPassphrase {
		secret = strings.TrimSpace(secret)
	}
	return secret, nil
}

func readMatrixSecretFile(path string, normalize bool) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read Matrix secret file: %w", err)
	}
	secret := string(data)
	if normalize {
		secret = strings.TrimSpace(secret)
	} else {
		secret = strings.TrimSuffix(secret, "\n")
		secret = strings.TrimSuffix(secret, "\r")
	}
	if secret == "" {
		return "", fmt.Errorf("matrix secret file %s is empty", path)
	}
	return secret, nil
}

func init() { rootCmd.AddCommand(newAddMatrixCmd()) }
