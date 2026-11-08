package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

// prepareICloudRequest resolves the bootstrap before duplicate, ownership and
// generation checks. Public password reuse remains exact-identity-only.
func (c *CardDAVController) prepareICloudRequest(
	ctx context.Context, req CardDAVAccountRequest, configured config.CardDAVConfig, gateAccountID, gateGeneration int64,
) (CardDAVAccountRequest, *carddav.Discovery, error) {
	if req.Provider != "" || !carddav.IsICloudDiscoveryURL(req.BaseURL) {
		return req, nil, nil
	}
	previous := configured
	password, err := c.passwordForRequest(ctx, req)
	if err != nil {
		return req, nil, err
	}
	configured.BaseURL, configured.Username = carddav.ICloudDiscoveryURL, req.Username
	service, err := c.factory(c.store, configured, password)
	if err != nil {
		return req, nil, errors.Join(errCardDAVValidation, err)
	}
	discovery, err := c.discoverICloudWithGate(ctx, service, carddav.ICloudDiscoveryURL, gateAccountID, gateGeneration)
	if err != nil {
		return req, nil, errors.Join(errCardDAVUpstream, err)
	}
	if discovery.HomeURL == nil {
		return req, nil, errors.Join(errCardDAVUpstream, carddav.ErrUnsafeHref)
	}
	origin, ok := carddav.ICloudPartitionOrigin(discovery.HomeURL.String())
	if !ok {
		return req, nil, errors.Join(errCardDAVUpstream, carddav.ErrUnsafeHref)
	}
	// Preserve a saved partition-root binding for the same iCloud account so
	// spelling differences do not replace its imported identities or book roles.
	previousOrigin, root := carddav.ICloudPartitionRootOrigin(previous.BaseURL)
	if previous.Provider == "" && root && previousOrigin == origin && previous.Username != "" && strings.EqualFold(previous.Username, req.Username) {
		origin = previous.BaseURL
		req.Username = previous.Username
	}
	req.BaseURL, req.Password = origin, password
	return req, &discovery, nil
}

type iCloudOperations struct {
	CardDAVOperations

	controller *CardDAVController
	configured config.CardDAVConfig
	credential carddav.Credential
}

func (c *CardDAVController) runtimeForCredential(
	service cardDAVCandidate, credential carddav.Credential, configured config.CardDAVConfig,
) CardDAVOperations {
	service = c.scopedCandidate(service, credential.ConnectionGeneration)
	if !credential.ICloud {
		return &cardDAVRoleOperations{CardDAVOperations: service, controller: c}
	}
	configured.TrustedAddresses = slices.Clone(configured.TrustedAddresses)
	return &iCloudOperations{CardDAVOperations: service, controller: c, configured: configured, credential: credential}
}

// SetBookRoles also covers callers retaining a selected runtime. The manager
// writes directly to Store under the same mutex, so there is no nested lock.
func (o *iCloudOperations) SetBookRoles(ctx context.Context, id int64, roles carddav.BookRoles) error {
	o.controller.saveLock().Lock()
	defer o.controller.saveLock().Unlock()
	return o.CardDAVOperations.SetBookRoles(ctx, id, roles)
}

func (o *iCloudOperations) Sync(ctx context.Context, options carddav.SyncOptions) (carddav.SyncResult, error) {
	c := o.controller
	// Extend the engine's single-run contract to the read-only work before
	// its lease. Concurrent preflights must not race retry gates or saves.
	if !c.iCloudSyncMu.TryLock() {
		return carddav.SyncResult{}, store.ErrCardDAVSyncActive
	}
	defer c.iCloudSyncMu.Unlock()
	return c.syncCurrentICloud(ctx, options)
}

func (c *CardDAVController) syncCurrentICloud(ctx context.Context, options carddav.SyncOptions) (carddav.SyncResult, error) {
	if err := ctx.Err(); err != nil {
		return carddav.SyncResult{}, safeICloudPreflightError(err)
	}
	active := c.Current()
	if active == nil {
		return carddav.SyncResult{}, carddav.ErrConnectionUnavailable
	}
	if current, ok := active.(*iCloudOperations); ok {
		return current.sync(ctx, options)
	}
	return active.Sync(ctx, options)
}

func (o *iCloudOperations) sync(ctx context.Context, options carddav.SyncOptions) (carddav.SyncResult, error) {
	c := o.controller
	active := c.Current()
	if active != o {
		// Scheduled jobs can retain an old wrapper after an explicit save.
		return c.syncCurrentICloud(ctx, options)
	}
	preflightCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := c.checkICloudBinding(preflightCtx, o.configured, o.credential); err != nil {
		return carddav.SyncResult{}, safeICloudPreflightError(err)
	}
	account, err := c.store.GetCardDAVAccountByNameContext(preflightCtx, c.connection())
	if err != nil || account == nil {
		return carddav.SyncResult{}, safeICloudPreflightError(carddav.ErrConnectionUnavailable)
	}
	if err := c.store.CheckCardDAVRetryAfterContext(preflightCtx, account.ID); err != nil {
		return carddav.SyncResult{}, safeICloudPreflightError(err)
	}
	probe, err := c.factory(c.store, o.configured, o.credential.Password)
	if err == nil {
		_, err = c.discoverICloudWithGate(preflightCtx, probe, o.configured.BaseURL, account.ID, o.credential.ConnectionGeneration)
	}
	if err == nil {
		if latest := c.Current(); latest != o {
			if latest == nil {
				return carddav.SyncResult{}, carddav.ErrConnectionUnavailable
			}
			return c.syncCurrentICloud(ctx, options)
		}
		if err := c.checkICloudBinding(preflightCtx, o.configured, o.credential); err != nil {
			return carddav.SyncResult{}, safeICloudPreflightError(err)
		}
		// Never catch this result and rediscover: Sync can write even when
		// its counters are zero. Failed preflight creates no sync-history row.
		return o.CardDAVOperations.Sync(ctx, options)
	}
	if !iCloudHostFailure(preflightCtx, err) {
		return carddav.SyncResult{}, safeICloudPreflightError(err)
	}
	originalErr := err
	req := CardDAVAccountRequest{
		BaseURL: carddav.ICloudDiscoveryURL, Username: o.credential.Username, Password: o.credential.Password,
		Enabled: new(o.configured.Enabled), Schedule: o.configured.Schedule,
	}
	req, _, err = c.prepareICloudRequest(preflightCtx, req, o.configured, account.ID, o.credential.ConnectionGeneration)
	if err != nil {
		return carddav.SyncResult{}, safeICloudPreflightError(err)
	}
	if req.BaseURL == o.configured.BaseURL {
		return carddav.SyncResult{}, safeICloudPreflightError(originalErr)
	}
	err = c.refuseICloudRelocation(preflightCtx, o.configured, o.credential)
	if errors.Is(err, config.ErrConfigConflict) || errors.Is(err, carddav.ErrCredentialNotBound) {
		err = errors.Join(carddav.ErrICloudReconnectRequired, err)
	}
	return carddav.SyncResult{}, safeICloudPreflightError(err)
}

func (c *CardDAVController) refuseICloudRelocation(ctx context.Context, configured config.CardDAVConfig, credential carddav.Credential) error {
	c.saveLock().Lock()
	defer c.saveLock().Unlock()
	if err := c.checkICloudBinding(ctx, configured, credential); err != nil {
		return err
	}
	account, err := c.store.GetCardDAVAccountByNameContext(ctx, c.connection())
	if err != nil {
		return err
	}
	if account == nil {
		return carddav.ErrCredentialNotBound
	}
	if err := c.store.CheckCardDAVRetryAfterContext(ctx, account.ID); err != nil {
		return err
	}
	// Generic identity replacement retires imported contacts and can lose book
	// roles. Refuse it until relocation can preserve both atomically.
	return carddav.ErrICloudReconnectRequired
}

func (c *CardDAVController) checkICloudBinding(ctx context.Context, configured config.CardDAVConfig, expected carddav.Credential) error {
	current, err := c.currentCardDAVConfig()
	if err != nil {
		return err
	}
	if !cardDAVConfigEqual(current, configured) {
		return config.ErrConfigConflict
	}
	credential, err := c.reusableCredential(ctx, configured.BaseURL, configured.Username)
	if err != nil {
		return err
	}
	if credential != expected || !credential.ICloud {
		return carddav.ErrCredentialNotBound
	}
	return nil
}

func (c *CardDAVController) discoverICloudWithGate(ctx context.Context, service cardDAVCandidate, base string, accountID, generation int64) (carddav.Discovery, error) {
	if accountID > 0 {
		if err := c.store.CheckCardDAVRetryAfterContext(ctx, accountID); err != nil {
			return carddav.Discovery{}, err
		}
	}
	discovery, err := service.DiscoverConnection(ctx, base)
	if status, ok := errors.AsType[*carddav.StatusError](err); ok && status.RetryAfter > 0 && accountID > 0 {
		err = errors.Join(err, c.recordICloudRetryAfter(ctx, accountID, generation, status.RetryAfter))
	}
	return discovery, err
}

func (c *CardDAVController) recordICloudRetryAfter(ctx context.Context, accountID, generation int64, delay time.Duration) error {
	// Save replaces credentials and discovery under this lock. A late response
	// from the old generation must not pause the replacement connection.
	c.saveLock().Lock()
	defer c.saveLock().Unlock()
	account, err := c.store.GetCardDAVAccountByIDContext(ctx, accountID)
	if err != nil || account == nil || account.ConnectionGeneration != generation {
		return err
	}
	return c.store.SetCardDAVRetryAfterContext(ctx, time.Now().Add(delay).UTC(), accountID)
}

func iCloudHostFailure(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, carddav.ErrUnsafeTarget) || errors.Is(err, carddav.ErrUnsafeHref) || errors.Is(err, carddav.ErrUnsafeRedirect) ||
		errors.Is(err, carddav.ErrOperationLimit) || errors.Is(err, carddav.ErrResponseLimit) || errors.Is(err, store.ErrCardDAVRetryAfter) {
		return false
	}
	if _, certificateError := errors.AsType[*tls.CertificateVerificationError](err); certificateError {
		return false
	}
	if _, certificateError := errors.AsType[x509.UnknownAuthorityError](err); certificateError {
		return false
	}
	if _, certificateError := errors.AsType[x509.HostnameError](err); certificateError {
		return false
	}
	if _, certificateError := errors.AsType[x509.CertificateInvalidError](err); certificateError {
		return false
	}
	if _, tlsError := errors.AsType[tls.RecordHeaderError](err); tlsError {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		eligible := false
		for _, cause := range joined.Unwrap() {
			if cause == nil {
				continue
			}
			if !iCloudHostFailure(ctx, cause) {
				return false
			}
			eligible = true
		}
		return eligible
	}
	// DNSError's optional cause can be nil; classify the semantic node before
	// unwrapping it, after rejecting unsafe or mixed joined causes above.
	if _, dns := err.(*net.DNSError); dns { //nolint:errorlint // Match only this node; errors.As would cross joined causes.
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		if _, dial := err.(*net.OpError); !dial { //nolint:errorlint // Preserve this node's dial semantics while recursively checking other wrappers.
			return iCloudHostFailure(ctx, wrapped.Unwrap())
		}
	}
	if status, ok := errors.AsType[*carddav.StatusError](err); ok {
		return status.RetryAfter == 0 && (status.StatusCode == http.StatusNotFound || status.StatusCode == http.StatusGone || status.StatusCode == http.StatusMisdirectedRequest)
	}
	if dial, ok := errors.AsType[*net.OpError](err); ok {
		return dial.Op == "dial"
	}
	return false
}

type iCloudPreflightError struct {
	cause   error
	message string
}

func (e *iCloudPreflightError) Error() string { return e.message }
func (e *iCloudPreflightError) Unwrap() error { return e.cause }

func safeICloudPreflightError(err error) error {
	_, message := carddav.SyncFailure(err)
	return &iCloudPreflightError{cause: err, message: message}
}

func (c *CardDAVController) refreshICloudRuntimeSnapshot(configured config.CardDAVConfig) {
	c.configLock().Lock()
	defer c.configLock().Unlock()
	if current, ok := c.service.(*iCloudOperations); ok {
		clone := *current
		configured.TrustedAddresses = slices.Clone(configured.TrustedAddresses)
		clone.configured = configured
		c.service = &clone
	}
}
