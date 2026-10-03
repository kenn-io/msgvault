package cmd

import (
	"context"
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

func newAddMatrixCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-matrix",
		Short: "Add a read-only Matrix account as an archive source",
		Long: `Add a Matrix account using a dedicated read-only msgvault device.

Password login is the default. For SSO accounts, obtain a single-use
m.login.token from the homeserver login flow and pass --login-token-file.
The recovery key or passphrase is used once to restore server-side key backup;
it is never written to config or the credential file. The backup decryption key
it unlocks is kept in the device's encrypted crypto store so later syncs can
fetch room keys that other devices add to the backup.`,
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
			if !isDaemonCLISubprocess() {
				loginSecret, err := readMatrixLoginSecret()
				if err != nil {
					return err
				}
				recoverySecret, err := readMatrixRecoverySecret(cmd)
				if err != nil {
					return err
				}
				return runDaemonCLICommandHTTPFromCobraWithEnv(cmd, args, map[string]string{
					clirun.EnvMatrixLoginSecret: loginSecret, clirun.EnvMatrixRecoverySecret: recoverySecret,
				})
			}

			loginSecret := os.Getenv(clirun.EnvMatrixLoginSecret)
			if loginSecret == "" {
				return errors.New("missing Matrix login secret in daemon subprocess")
			}
			// /cli/run can reach this subprocess without the client-side prompt,
			// so enforce the recovery-secret requirement before any login.
			recoverySecret := os.Getenv(clirun.EnvMatrixRecoverySecret)
			if recoverySecret == "" && !addMatrixSkipKeyBackup {
				return errors.New("missing Matrix recovery secret: pass --recovery-file (or explicitly --skip-key-backup)")
			}
			return matrixsource.WithCredentialLifecycleLock(state.cfg.TokensDir(), func() error {
				creds, err := matrixsource.Login(cmd.Context(), addMatrixHomeserver, addMatrixUserID, loginSecret, addMatrixLoginTokenFile != "")
				if err != nil {
					return err
				}
				keepDevice := false
				defer func() {
					if keepDevice {
						return
					}
					cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(cmd.Context()), 10*time.Second)
					defer cancel()
					_ = matrixsource.Logout(cleanupCtx, creds)
					_ = matrixsource.DeleteCryptoStore(state.cfg.Data.DataDir, creds.UserID, creds.DeviceID)
				}()
				runtime, err := matrixsource.Open(cmd.Context(), creds, matrixsource.CryptoStorePath(state.cfg.Data.DataDir, creds.UserID, creds.DeviceID))
				if err != nil {
					return err
				}
				defer func() { _ = runtime.Close() }()
				if !addMatrixSkipKeyBackup {
					if err := runtime.RestoreKeyBackup(cmd.Context(), recoverySecret, addMatrixRecoveryIsPassphrase); err != nil {
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
				if err := s.UpdateSourceDisplayName(source.ID, "Matrix "+creds.UserID); err != nil {
					return fmt.Errorf("set Matrix source name: %w", err)
				}
				if !noDefaultIdentityAddMatrix {
					confirmDefaultIdentity(cmd.OutOrStdout(), s, source.ID, creds.UserID, creds.UserID, "account-identifier", state.logger)
				}
				if err := runPostSourceCreateMigrationsForInvocation(s, state); err != nil {
					return fmt.Errorf("post-source-create migrations: %w", err)
				}
				// Revoke the earlier device before its token is replaced, so a failed
				// logout leaves the old login in place for a retry.
				// Accepted design decision: if saving the new login fails after this
				// logout, rerunning add-matrix restores it. Saving first would instead
				// leave the old device signed in with nothing on disk to revoke it.
				exists, err := matrixsource.CredentialsExist(state.cfg.TokensDir(), creds.UserID)
				if err != nil {
					return err
				}
				if exists {
					previous, err := matrixsource.LoadCredentials(state.cfg.TokensDir(), creds.UserID)
					if err != nil {
						return fmt.Errorf("read the existing Matrix login before replacing it (fix or remove that file, then retry): %w", err)
					}
					if previous.DeviceID != creds.DeviceID {
						logoutCtx, cancel := context.WithTimeout(context.WithoutCancel(cmd.Context()), 10*time.Second)
						err := matrixsource.Logout(logoutCtx, previous)
						cancel()
						if err != nil && !matrixsource.IsUnknownToken(err) {
							return fmt.Errorf("log out previous Matrix device %s (login unchanged, retry add-matrix): %w", previous.DeviceID, err)
						}
					}
				}
				if err := matrixsource.SaveCredentials(state.cfg.TokensDir(), creds); err != nil {
					// A published file already replaced the old login, so keep its device.
					keepDevice = errors.Is(err, atomicfile.ErrPublished)
					return err
				}
				keepDevice = true
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Added Matrix account %s with device %s\n", creds.UserID, creds.DeviceID)
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Run: msgvault sync-matrix")
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "The dedicated archive device remains unverified; interactive SAS/QR verification is not supported.")
				return nil
			})
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&addMatrixHomeserver, "homeserver", "", "Matrix homeserver URL")
	flags.StringVar(&addMatrixUserID, "user-id", "", "full Matrix user ID (for example @archive:example.org)")
	flags.StringVar(&addMatrixPasswordFile, "password-file", "", "read the Matrix password from a file")
	flags.StringVar(&addMatrixLoginTokenFile, "login-token-file", "", "read a single-use m.login.token from a file")
	flags.StringVar(&addMatrixRecoveryFile, "recovery-file", "", "read the recovery key or passphrase from a file")
	flags.BoolVar(&addMatrixRecoveryIsPassphrase, "recovery-passphrase", false, "interpret the recovery secret as a passphrase")
	flags.BoolVar(&addMatrixSkipKeyBackup, "skip-key-backup", false, "skip server-side key-backup restore")
	flags.BoolVar(&noDefaultIdentityAddMatrix, "no-default-identity", false, noDefaultIdentityHelp)
	return cmd
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
		return readPasswordInteractive("Matrix password:", output)
	case passwordPipe:
		return readPasswordFromPipe(os.Stdin)
	default:
		return "", errors.New("cannot read Matrix password: use --password-file or --login-token-file")
	}
}

func readMatrixRecoverySecret(cmd *cobra.Command) (string, error) {
	if addMatrixSkipKeyBackup {
		return "", nil
	}
	if addMatrixRecoveryFile != "" {
		return readMatrixSecretFile(addMatrixRecoveryFile, !addMatrixRecoveryIsPassphrase)
	}
	if !isatty.IsTerminal(os.Stdin.Fd()) && !isatty.IsCygwinTerminal(os.Stdin.Fd()) {
		return "", errors.New("key-backup restore needs --recovery-file (or explicitly --skip-key-backup)")
	}
	prompt := "Matrix recovery key:"
	if addMatrixRecoveryIsPassphrase {
		prompt = "Matrix recovery passphrase:"
	}
	return readPasswordInteractive(prompt, cmd.ErrOrStderr())
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
