package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/msmail"
	"go.kenn.io/msgvault/internal/store"
)

// msmailQPS is the Graph mail request rate. Microsoft documents 10,000
// requests per 10 minutes per mailbox; 15 per second stays under it.
const msmailQPS = 15

func newGraphMailManager(state *invocation, src *store.Source) (*microsoft.GraphManager, error) {
	app, dir, err := microsoftMailApp(state.cfg, sourceOAuthApp(src))
	if err != nil {
		return nil, err
	}
	return microsoft.NewGraphMailManager(app.ClientID, app.EffectiveTenantID(), app.EffectiveRedirectURI(), dir, state.logger), nil
}

func newGraphMailWriteManager(state *invocation, src *store.Source) (*microsoft.GraphManager, error) {
	app, dir, err := microsoftMailApp(state.cfg, sourceOAuthApp(src))
	if err != nil {
		return nil, err
	}
	return microsoft.NewGraphMailWriteManager(app.ClientID, app.EffectiveTenantID(), app.EffectiveRedirectURI(), dir, state.logger), nil
}

// runMSMailSync syncs one Graph mail account. The first run downloads every
// folder; later runs fetch only the changes.
func runMSMailSync(ctx context.Context, s *store.Store, src *store.Source, progress func(string), state *invocation) (*msmail.Summary, error) {
	cfg := state.cfg
	mgr, err := newGraphMailManager(state, src)
	if err != nil {
		return nil, err
	}
	email := src.Identifier
	tokenFn, err := mgr.TokenSource(ctx, email)
	if err != nil {
		return nil, err
	}
	client := msmail.NewClient(msmail.GraphBaseURL, tokenFn, msmailQPS)
	return msmail.Import(ctx, s, client, msmail.Options{
		Email:          email,
		AttachmentsDir: cfg.AttachmentsDir(),
		RelayedSenders: cfg.RelayedSenders,
		Progress:       progress,
	}, state.logger)
}

// runScheduledMSMailSync is the daemon path. Like runScheduledTeamsSync, it
// seeds the "me" identity and runs pending migrations before the sync.
func runScheduledMSMailSync(ctx context.Context, src *store.Source, s *store.Store, state *invocation) error {
	confirmDefaultIdentity(ctx, io.Discard, s, src.ID, src.Identifier, src.Identifier, "account-identifier", state.logger)
	if err := runPostSourceCreateMigrationsForInvocation(ctx, s, state); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}
	_, err := runMSMailSync(ctx, s, src, nil, state)
	return err
}

func writeMSMailSyncSummary(out io.Writer, email string, sum *msmail.Summary) {
	_, _ = fmt.Fprintf(out, "\nMicrosoft Graph mail sync complete for %s\n", email)
	_, _ = fmt.Fprintf(out, "  Duration:        %s\n", sum.Duration.Round(time.Second))
	_, _ = fmt.Fprintf(out, "  Folders:         %d\n", sum.Folders)
	_, _ = fmt.Fprintf(out, "  Messages added:  %d\n", sum.Added)
	_, _ = fmt.Fprintf(out, "  Updated:         %d\n", sum.Updated)
	_, _ = fmt.Fprintf(out, "  Moved:           %d\n", sum.Moved)
	_, _ = fmt.Fprintf(out, "  Deleted:         %d\n", sum.Deleted)
	if sum.Errors > 0 {
		_, _ = fmt.Fprintf(out, "  Errors:          %d\n", sum.Errors)
	}
}
