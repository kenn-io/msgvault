package cmd

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/threads"
)

type threadsOAuthPayload struct {
	AppID       string `json:"app_id"`
	RedirectURI string `json:"redirect_uri"`
	Code        string `json:"code"`
	AppSecret   string `json:"app_secret"`
}

func (threadsOAuthPayload) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "Threads OAuth handoff [redacted]")
}

func newAddThreadsCmd(deps threadsCommandDeps) *cobra.Command {
	cmd := newAddThreadsLocalCmd(deps)
	local := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if isAgentMode(invocationFromCommand(cmd)) {
			return errors.New("threads connection requires the owner")
		}
		if isDaemonCLISubprocess() {
			return local(cmd, args)
		}
		appID, _ := cmd.Flags().GetString("app-id")
		redirect, _ := cmd.Flags().GetString("redirect-uri")
		state, err := threads.NewOAuthState()
		if err != nil {
			return err
		}
		authorization, err := threads.AuthorizationURL(appID, redirect, state)
		if err != nil {
			return usageErr(cmd, err)
		}
		daemon, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		err = daemon.ValidateThreadsOAuthHandoff()
		_ = daemon.Close()
		if err != nil {
			return err
		}
		reader := bufio.NewReader(cmd.InOrStdin())
		secret := os.Getenv("MSGVAULT_THREADS_APP_SECRET")
		if secret == "" {
			file, terminal := cmd.InOrStdin().(*os.File)
			if !terminal || !isatty.IsTerminal(file.Fd()) {
				return errors.New("set MSGVAULT_THREADS_APP_SECRET or use a terminal for the masked prompt")
			}
			secret, err = readPasswordInteractive("Threads app secret:", cmd.ErrOrStderr(), false)
			if err != nil {
				return errors.New("read Threads app secret")
			}
			secret = strings.TrimSpace(secret)
		}
		if secret == "" {
			return errors.New("threads app secret is required")
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Open this authorization URL in your browser:\n%s\n", authorization)
		_, _ = fmt.Fprint(cmd.ErrOrStderr(), "Paste the full returned callback URL: ")
		callback, err := reader.ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || callback == "") {
			return errors.New("read Threads callback")
		}
		code, err := threads.ParseOAuthCallback(redirect, strings.TrimSpace(callback), state)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(threadsOAuthPayload{AppID: appID, RedirectURI: redirect, Code: code, AppSecret: secret})
		if err != nil {
			return errors.New("encode Threads authorization handoff")
		}
		return runDaemonCLICommandHTTPFromCobraWithEnv(cmd, args, map[string]string{clirun.EnvThreadsOAuth: string(payload)})
	}
	return cmd
}

func newAddThreadsLocalCmd(deps threadsCommandDeps) *cobra.Command {
	var appID, redirectURI string
	cmd := &cobra.Command{
		Use: "add-threads", Short: "Connect your Threads account", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if invocationFromCommand(cmd) != nil && deps.bind != nil {
				deps = deps.bind(cmd.Context())
			}
			return runAddThreads(cmd, deps, appID, redirectURI)
		},
	}
	cmd.Flags().StringVar(&appID, "app-id", "", "Threads app ID")
	cmd.Flags().StringVar(&redirectURI, "redirect-uri", "", "Registered HTTPS callback")
	cmd.Flags().Bool("no-default-identity", false, savedDefaultIdentityHelp)
	return cmd
}

func runAddThreads(cmd *cobra.Command, deps threadsCommandDeps, appID, redirectURI string) error {
	if _, err := threads.AuthorizationURL(appID, redirectURI, "validate-registration"); err != nil {
		return usageErr(cmd, err)
	}
	raw := os.Getenv(clirun.EnvThreadsOAuth)
	if deps.oauthPayload != nil {
		raw = deps.oauthPayload()
	}
	if len(raw) > 64<<10 {
		return errors.New("threads OAuth handoff is too large")
	}
	var payload threadsOAuthPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil || payload.AppID != appID || payload.RedirectURI != redirectURI || payload.Code == "" || payload.AppSecret == "" {
		return errors.New("run msgvault add-threads from your terminal to connect Threads")
	}
	st, cleanup, err := deps.openStore()
	if err != nil {
		return err
	}
	defer cleanup()
	sources, err := st.ListSourcesContext(cmd.Context(), sourceTypeThreads)
	if err != nil {
		return err
	}
	initial := make(map[string]int64, len(sources))
	for _, source := range sources {
		initial[source.Identifier] = source.ID
	}
	ctx := cmd.Context()
	manager := deps.tokenManager()
	options := deps.oauthOptions()
	options.WithOwnerLock = func(ctx context.Context, identifier string, exchange func() (threads.TokenRecord, error)) (record threads.TokenRecord, retErr error) {
		retErr = manager.WithSourceLock(ctx, identifier, func() (retErr error) {
			var execution *store.SyncExecution
			defer func() {
				if execution != nil {
					retErr = errors.Join(retErr, execution.Release())
				}
			}()
			latest, err := st.ListSourcesContext(ctx, sourceTypeThreads)
			if err != nil {
				return err
			}
			var current *store.Source
			for _, candidate := range latest {
				if candidate.Identifier == identifier {
					current = candidate
					break
				}
			}
			if oldID, existed := initial[identifier]; existed && (current == nil || current.ID != oldID) {
				return errors.New("the Threads account changed while connecting; rerun add-threads")
			}
			if current != nil {
				if current.HistoryOnly || current.MergedIntoSourceID != 0 {
					return errors.New("this Threads account was removed or merged and can't be reconnected")
				}
				execution, err = st.AcquireSyncExecutionContext(ctx, current.ID)
				if err != nil {
					return err
				}
			}
			record, err = exchange()
			if err != nil {
				return err
			}
			_, priorErr := manager.Load(record.AppID, record.OwnerID)
			if priorErr != nil && !errors.Is(priorErr, threads.ErrTokenNotFound) {
				return fmt.Errorf("the Threads token for %s could not be read; rerun add-threads, or inspect its token file using the add-threads documentation", identifier)
			}
			save := deps.saveToken
			if save == nil {
				save = func(ctx context.Context, manager *threads.TokenManager, record threads.TokenRecord) error {
					return manager.Save(ctx, record)
				}
			}
			if err := save(ctx, manager, record); err != nil {
				if errors.Is(err, atomicfile.ErrPublished) {
					return fmt.Errorf("the Threads token may have been saved for %s; rerun add-threads, or locate its token file using the add-threads documentation: %w", identifier, atomicfile.ErrPublished)
				}
				message := fmt.Sprintf("the Threads token for %s could not be saved; rerun add-threads, or inspect its token file using the add-threads documentation", identifier)
				if errors.Is(err, context.Canceled) {
					return fmt.Errorf("%s: %w", message, context.Canceled)
				}
				if errors.Is(err, context.DeadlineExceeded) {
					return fmt.Errorf("%s: %w", message, context.DeadlineExceeded)
				}
				return errors.New(message)
			}
			source, err := deps.registerSource(ctx, st, record)
			if err != nil {
				if current == nil && errors.Is(priorErr, threads.ErrTokenNotFound) {
					// Cleanup must complete under the lifecycle lock even if the request was cancelled.
					cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
					_, lookupErr := st.GetSourceByTypeAndIdentifier(cleanupCtx, sourceTypeThreads, identifier)
					if errors.Is(lookupErr, store.ErrSourceNotFound) {
						lookupErr = manager.Delete(cleanupCtx, record.AppID, record.OwnerID)
						if lookupErr == nil {
							cancel()
							return fmt.Errorf("adding the Threads account failed; rerun add-threads: %w", err)
						}
					}
					cancel()
				}
				return fmt.Errorf("the Threads token was saved for %s, but adding the account failed; rerun add-threads, or delete that account's token file from the tokens directory (see docs: add-threads): %w", identifier, err)
			}
			if execution == nil {
				execution, err = st.AcquireSyncExecutionContext(ctx, source.ID)
				if err != nil {
					return fmt.Errorf("the Threads account %s and its token were saved, but finishing setup failed; rerun add-threads: %w", identifier, err)
				}
			}
			logger := slog.New(slog.DiscardHandler)
			if state := invocationFromCommand(cmd); state != nil && state.logger != nil {
				logger = state.logger
			}
			optOut, err := cmd.Flags().GetBool("no-default-identity")
			if err != nil {
				return fmt.Errorf("read default identity preference: %w", err)
			}
			if err := setDefaultIdentityOptOut(ctx, cmd, st, source, optOut); err != nil {
				return fmt.Errorf("the Threads account %s and its token were saved, but finishing setup failed; rerun add-threads: %w", identifier, err)
			}
			if !optOut {
				confirmDefaultIdentity(ctx, cmd.OutOrStdout(), st, source.ID, identifier, identifier, "account-identifier", logger)
			}
			if deps.postSourceMigrations != nil {
				if err := deps.postSourceMigrations(st); err != nil {
					return fmt.Errorf("the Threads account %s and its token were saved, but finishing setup failed; rerun add-threads: %w", identifier, err)
				}
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Connected Threads account: %s (token expires %s)\n", identifier, record.ExpiresAt.UTC().Format(time.RFC3339))
			return nil
		})
		return record, retErr
	}
	_, err = threads.ExchangeCredential(ctx, options, threads.NewOAuthRequest(appID, redirectURI, payload.Code, payload.AppSecret))
	return err
}

func registerThreadsSource(ctx context.Context, st *store.Store, record threads.TokenRecord) (*store.Source, error) {
	identifier, err := threads.SourceIdentifier(record.AppID, record.OwnerID)
	if err != nil {
		return nil, err
	}
	source, err := st.GetOrCreateSourceContext(ctx, sourceTypeThreads, identifier)
	if err != nil {
		return nil, err
	}
	if source.HistoryOnly || source.MergedIntoSourceID != 0 {
		return nil, errors.New("this Threads account was removed or merged and can't be reconnected")
	}
	label := record.Name
	if label == "" {
		label = record.Username
	}
	if label == "" {
		label = "Threads account " + record.OwnerID
	}
	if !source.DisplayName.Valid || strings.TrimSpace(source.DisplayName.String) == "" {
		if err := updateSourceDisplayNameForRegistration(st, source.ID, label, nil); err != nil {
			return nil, err
		}
	}
	return st.GetSourceByIDContext(ctx, source.ID)
}

func init() { rootCmd.AddCommand(newAddThreadsCmd(defaultThreadsCommandDeps())) }
