package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	xapi "go.kenn.io/msgvault/internal/x"
)

// removeXSource keeps execution and both credential lifecycle fences through
// the existing source cascade. owned is used by remove-account, which must take
// execution before its analytics-cache lock, matching importer lock ordering.
func removeXSource(ctx context.Context, st *store.Store, cfg *config.Config, source *store.Source, d xCommandDeps, owned bool) (hadActive bool, packed int64, retErr error) {
	if source.SourceType != sourceTypeX || xapi.ValidateID(source.Identifier) != nil {
		return false, 0, errors.New("invalid X source for removal")
	}
	if !owned {
		release, err := st.AcquireSourceRemovalLockContext(ctx, source.ID)
		if err != nil {
			return false, 0, err
		}
		defer func() { retErr = errors.Join(retErr, release()) }()
	}
	main, staged := d.manager(cfg.TokensDir()), d.manager(xStagedTokensDir(cfg))
	retErr = withXCredentialOwnership(ctx, main, staged, source.Identifier, func() error {
		current, err := st.GetSourceByIDContext(ctx, source.ID)
		if err != nil {
			return err
		}
		if current.SourceType != sourceTypeX || current.Identifier != source.Identifier {
			return errors.New("X source identity changed before removal")
		}
		// Malformed/missing known files cannot prevent local account removal. Load
		// only for best-effort revoke; unlink below accepts neither paths nor JSON.
		var grants []xapi.TokenRecord
		for _, manager := range []*xapi.TokenManager{main, staged} {
			record, err := manager.Load(ctx, source.Identifier)
			if err == nil {
				grants = append(grants, record)
			}
		}
		hadActive, packed, err = st.RemoveSourceSerialized(ctx, source.ID)
		if err != nil {
			return err
		}
		// Once the cascade commits, cancellation or provider denial must not retain
		// credentials. Revocation uses no refresh/probe/retry and is bounded once.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		type grantKey struct{ client, token string }
		seen := map[grantKey]bool{}
		for _, record := range grants {
			key := grantKey{record.ClientID, record.RefreshToken()}
			if seen[key] {
				continue
			}
			seen[key] = true
			if err := xapi.RevokeCredential(cleanupCtx, record, d.revokeURL, d.httpClient); err != nil {
				fmt.Fprintln(os.Stderr, "Warning: X remote revocation was not confirmed; removing local credentials.")
			}
		}
		// Local unlink gets its own bounded context even if remote revoke used up
		// its timeout. The lifecycle locks remain held until both known files go.
		localCtx, localCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer localCancel()
		return errors.Join(main.RemoveCredentials(localCtx, source.Identifier), staged.RemoveCredentials(localCtx, source.Identifier))
	})
	return hadActive, packed, retErr
}
