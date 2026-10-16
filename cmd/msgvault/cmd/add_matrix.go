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
	addMatrixHomeserver        string
	addMatrixUserID            string
	addMatrixPasswordFile      string
	addMatrixLoginTokenFile    string
	noDefaultIdentityAddMatrix bool
)

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
			tokensDir := state.cfg.TokensDir()
			if !isDaemonCLISubprocess() {
				// The daemon may be remote, so only it can tell whether a pending
				// renewal will make this secret unnecessary.
				loginSecret, err := readMatrixLoginSecret()
				if err != nil {
					return err
				}
				return runDaemonCLICommandHTTPFromCobraWithEnv(cmd, args, map[string]string{
					clirun.EnvMatrixLoginSecret: loginSecret,
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
	creds, resumed, err := matrixsource.LoadPendingCredentials(tokensDir, addMatrixUserID)
	if err != nil {
		return fmt.Errorf("%w (fix or remove that file, then retry)", err)
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
		creds, err = matrixsource.Login(ctx, addMatrixHomeserver, addMatrixUserID, loginSecret, addMatrixLoginTokenFile != "")
		if err != nil {
			return err
		}
	}
	defer func() {
		if keepDevice {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), matrixRequestTimeout)
		defer cancel()
		_ = matrixsource.Logout(cleanupCtx, creds)
	}()
	s, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
	if err != nil {
		return err
	}
	defer cleanup()
	source, err := s.GetOrCreateSource(sourceTypeMatrix, creds.UserID)
	if err != nil {
		return fmt.Errorf("create Matrix source: %w", err)
	}
	if err := updateSourceDisplayNameForRegistration(s, source.ID, "Matrix "+creds.UserID, state.logger); err != nil {
		return fmt.Errorf("set Matrix source name: %w", err)
	}
	if !noDefaultIdentityAddMatrix {
		confirmDefaultIdentity(cmd.OutOrStdout(), s, source.ID, creds.UserID, creds.UserID, "account-identifier", state.logger)
	}
	if err := runPostSourceCreateMigrationsForInvocation(s, state); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}
	exists, err := matrixsource.CredentialsExist(tokensDir, creds.UserID)
	if err != nil {
		return err
	}
	if exists {
		previous, err := matrixsource.LoadCredentials(tokensDir, creds.UserID)
		if err != nil {
			return fmt.Errorf("read the existing Matrix login before replacing it (fix or remove that file, then retry): %w", err)
		}
		if previous.DeviceID != creds.DeviceID {
			// Rewrite even a resumed login so it is durable before the old one goes.
			if err := savePendingMatrixCredentials(tokensDir, creds); err != nil {
				// A published pending file can still be resumed, so keep its device.
				keepDevice = keepDevice || errors.Is(err, atomicfile.ErrPublished)
				return fmt.Errorf("save the new Matrix login (login unchanged, retry add-matrix): %w", err)
			}
			keepDevice, staged = true, true
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
		return readPasswordInteractive("Matrix password:", output)
	case passwordPipe:
		return readPasswordFromPipe(os.Stdin)
	default:
		return "", errors.New("cannot read Matrix password: use --password-file or --login-token-file")
	}
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
