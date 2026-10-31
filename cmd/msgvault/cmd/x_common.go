package cmd

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	xapi "go.kenn.io/msgvault/internal/x"
)

const sourceTypeX = "x"

type xCommandDeps struct {
	revokeURL    string
	cleanup      *XContentCleanup
	apiBaseURL   string
	httpClient   *http.Client
	oauthOptions xapi.OAuthOptions
}

func defaultXCommandDeps() xCommandDeps { return xCommandDeps{apiBaseURL: xapi.DefaultBaseURL} }
func (d xCommandDeps) baseURL() string {
	if d.apiBaseURL != "" {
		return d.apiBaseURL
	}
	return xapi.DefaultBaseURL
}
func (d xCommandDeps) manager(dir string) *xapi.TokenManager {
	return xapi.NewTokenManager(dir, xapi.TokenManagerOptions{TokenURL: d.oauthOptions.TokenURL, HTTPClient: d.httpClient, Now: d.oauthOptions.Now})
}
func xStagedTokensDir(cfg *config.Config) string  { return filepath.Join(cfg.TokensDir(), ".x-staged") }
func xRegisterError(code string, err error) error { return &api.CLIRunCodedError{Code: code, Err: err} }

type xCredentialCandidate struct {
	record            xapi.TokenRecord
	staged            bool
	mainGeneration    int64
	cleanupGeneration int64
}
type xVerifiedCredential struct {
	candidate xCredentialCandidate
	user      xapi.User
	fetchedAt time.Time
}

func loadXCredentialCandidate(ctx context.Context, main, staged *xapi.TokenManager, userID string) (xCredentialCandidate, error) {
	current, err := main.Load(ctx, userID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return xCredentialCandidate{}, err
	}
	pending, pendingErr := staged.Load(ctx, userID)
	if pendingErr == nil {
		if err == nil {
			consumed, bindErr := current.ConsumesStage(pending)
			if bindErr != nil {
				return xCredentialCandidate{}, bindErr
			}
			if consumed {
				return xCredentialCandidate{record: current, mainGeneration: current.Generation, cleanupGeneration: pending.Generation}, nil
			}
		}
		return xCredentialCandidate{record: pending, staged: true, mainGeneration: current.Generation, cleanupGeneration: pending.Generation}, nil
	}
	if !errors.Is(pendingErr, os.ErrNotExist) {
		return xCredentialCandidate{}, pendingErr
	}
	if err != nil {
		return xCredentialCandidate{}, err
	}
	return xCredentialCandidate{record: current, mainGeneration: current.Generation}, nil
}
func sameXCredential(a, b xapi.TokenRecord) bool {
	return a.OwnerID == b.OwnerID && a.Generation == b.Generation && a.ClientID == b.ClientID && a.ClientType == b.ClientType && a.Expiry.Equal(b.Expiry) && slices.Equal(a.GrantedScopes, b.GrantedScopes) && a.AccessToken() == b.AccessToken() && a.RefreshToken() == b.RefreshToken() && a.ClientSecret() == b.ClientSecret()
}
func withXCredentialOwnership(ctx context.Context, main, staged *xapi.TokenManager, userID string, fn func() error) error {
	return main.WithLifecycleLock(ctx, userID, func() error { return staged.WithLifecycleLock(ctx, userID, fn) })
}
func verifyXCredential(ctx context.Context, d xCommandDeps, candidate xCredentialCandidate, userID string) (xVerifiedCredential, error) {
	user, err := xapi.NewClient(d.baseURL(), candidate.record.BearerClient(d.httpClient)).Me(ctx)
	if err != nil {
		return xVerifiedCredential{}, xRegisterError("credential_verification_failed", err)
	}
	if user.ID != userID || candidate.record.OwnerID != userID {
		return xVerifiedCredential{}, xRegisterError("credential_owner_mismatch", errors.New("X credential owner does not match requested source"))
	}
	now := time.Now()
	if d.oauthOptions.Now != nil {
		now = d.oauthOptions.Now()
	}
	return xVerifiedCredential{candidate: candidate, user: user, fetchedAt: now}, nil
}

// registerXAccount reads only this daemon host's known credential locations.
func registerXAccount(ctx context.Context, st *store.Store, cfg *config.Config, userID string) (*store.Source, error) {
	return registerXAccountWithDeps(ctx, st, cfg, userID, defaultXCommandDeps())
}

func registerXAccountWithDeps(ctx context.Context, st *store.Store, cfg *config.Config, userID string, d xCommandDeps) (result *store.Source, retErr error) {
	if cfg == nil || st == nil {
		return nil, xRegisterError("registration_unavailable", errors.New("X registration configuration is unavailable"))
	}
	if err := xapi.ValidateID(userID); err != nil {
		return nil, xRegisterError("invalid_args", err)
	}
	sources, err := st.ListSourcesContext(ctx, sourceTypeX)
	if err != nil {
		return nil, err
	}
	var source *store.Source
	for _, candidate := range sources {
		if candidate.Identifier == userID {
			source = candidate
			break
		}
	}
	if source != nil && source.MergedIntoSourceID != 0 {
		return nil, store.ErrSourceRetired
	}
	main, staged := d.manager(cfg.TokensDir()), d.manager(xStagedTokensDir(cfg))
	var verified *xVerifiedCredential
	if source == nil {
		// Verify without a source first. Release credentials before creating/taking
		// source execution; a cached result is reused only for the identical record.
		err = withXCredentialOwnership(ctx, main, staged, userID, func() error {
			candidate, err := loadXCredentialCandidate(ctx, main, staged, userID)
			if err != nil {
				return xRegisterError("credentials_missing", err)
			}
			checked, err := verifyXCredential(ctx, d, candidate, userID)
			if err != nil {
				return err
			}
			verified = &checked
			return nil
		})
		if err != nil {
			return nil, err
		}
		source, err = st.GetOrCreateSourceContext(ctx, sourceTypeX, userID)
		if err != nil {
			return nil, err
		}
	}
	execution, err := st.AcquireSyncExecutionContext(ctx, source.ID)
	if err != nil {
		return nil, xRegisterError("source_busy", err)
	}
	cleaner := d.cleanup
	if cleaner == nil {
		cleaner = NewXContentCleanup(st, cfg, nil)
	}
	needsCleanup := false
	defer func() {
		releaseErr := execution.Release()
		retErr = errors.Join(retErr, releaseErr)
		// Credential ownership has also been released here. Connected copies
		// may now take sorted source ownership without nesting our source lock.
		if needsCleanup && releaseErr == nil {
			retErr = errors.Join(retErr, cleaner.Resume(ctx))
		}
	}()
	err = withXCredentialOwnership(ctx, main, staged, userID, func() error {
		currentSource, err := st.GetSourceByIDContext(ctx, source.ID)
		if err != nil {
			return err
		}
		if currentSource.SourceType != sourceTypeX || currentSource.Identifier != userID || currentSource.MergedIntoSourceID != 0 {
			return store.ErrSourceRetired
		}
		state, err := st.LoadXControlContext(ctx, source.ID)
		if err != nil {
			return err
		}
		candidate, err := loadXCredentialCandidate(ctx, main, staged, userID)
		if err != nil {
			return xRegisterError("credentials_missing", err)
		}
		if verified != nil {
			if candidate.staged != verified.candidate.staged || candidate.mainGeneration != verified.candidate.mainGeneration || candidate.cleanupGeneration != verified.candidate.cleanupGeneration || !sameXCredential(candidate.record, verified.candidate.record) {
				return xRegisterError("credential_changed", errors.New("X credential changed during verification"))
			}
		} else {
			checked, err := verifyXCredential(ctx, d, candidate, userID)
			if err != nil {
				return err
			}
			verified = &checked
		}
		checked, err := loadXCredentialCandidate(ctx, main, staged, userID)
		if err != nil {
			return err
		}
		if checked.staged != candidate.staged || checked.mainGeneration != candidate.mainGeneration || checked.cleanupGeneration != candidate.cleanupGeneration || !sameXCredential(checked.record, candidate.record) {
			return xRegisterError("credential_changed", errors.New("X credential changed during verification"))
		}
		if verified.user.ProtectedKnown && verified.user.Protected {
			// Queue every known owner chain before publishing any profile/token
			// replacement. A failed or cross-source cleanup remains durably fenced.
			needsCleanup = true
			if err := st.QueueXAuthorRemovalContext(ctx, source.ID, userID); err != nil {
				return err
			}
			pending, err := st.PendingXRemovalsContext(ctx, source.ID)
			if err != nil {
				return err
			}
			for _, work := range pending {
				if err := cleaner.DuringSync(ctx, work); err != nil && !errors.Is(err, store.ErrXRemovalNeedsOwner) {
					return err
				}
			}
		}
		// Profile evidence is real and owner-verified; no file replacement occurs
		// until these short Store operations succeed. None resets source history.
		if _, err := xapi.RecordAuthor(ctx, st, source.ID, verified.user, verified.fetchedAt); err != nil {
			return err
		}
		if err := st.AddAccountIdentityContext(ctx, source.ID, userID, "x_owner"); err != nil {
			return err
		}
		if err := st.UpdateSourceDisplayNameContext(ctx, source.ID, "@"+verified.user.Username); err != nil && !errors.Is(err, store.ErrSourceSettingsInvalid) {
			return err
		}
		record := candidate.record
		record.Username = verified.user.Username
		if candidate.staged {
			record, err = record.WithConsumedStage(candidate.record)
			if err != nil {
				return xRegisterError("credential_install_failed", err)
			}
		}
		installed, err := main.InstallVerified(ctx, record, candidate.mainGeneration, state.Generation)
		if err != nil {
			return xRegisterError("credential_install_failed", err)
		}
		clear := ""
		if _, err := st.UpdateXControlContext(ctx, source.ID, state.Revision, store.XControlPatch{OwnerID: userID, InitializeInstance: true, Username: &verified.user.Username, Generation: &installed.Generation, CredentialError: &clear}); err != nil {
			return err
		}
		// On a failed Store publication the staged file remains for explicit
		// recovery. Consumption itself is CAS-bound so a newer stage is never lost.
		if candidate.cleanupGeneration > 0 {
			if err := staged.Delete(ctx, userID, candidate.cleanupGeneration); err != nil {
				return xRegisterError("staged_cleanup_failed", err)
			}
		}
		result, err = st.GetSourceByIDContext(ctx, source.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (a *storeAPIAdapter) runCLIRegisterX(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	if req.Grant != nil {
		return xRegisterError("not_permitted", errors.New("X registration requires owner authority"))
	}
	request, err := api.ParseCLIRunXRegister(req)
	if err != nil {
		return xRegisterError("invalid_args", err)
	}
	d := defaultXCommandDeps()
	if a.xDeps != nil {
		d = *a.xDeps
	}
	d.cleanup = a.xCleanup
	source, err := registerXAccountWithDeps(ctx, a.store, a.config, request.UserID, d)
	if err != nil {
		return err
	}
	state, err := a.store.LoadXControlContext(ctx, source.ID)
	if err != nil {
		return err
	}
	output := xRegistrationOutput{SourceID: source.ID, UserID: source.Identifier, Username: strings.TrimPrefix(source.DisplayName.String, "@"), Generation: state.Generation}
	return emitXRegistration(output, request.JSON, emit)
}
