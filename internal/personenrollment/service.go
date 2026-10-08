package personenrollment

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

var (
	ErrCheckRequired   = errors.New("an exact successful people provider check is required")
	ErrConsentRequired = errors.New("exact people provider consent is required")
	ErrProfileExists   = errors.New("people provider profile already exists")
	ErrProfileMissing  = errors.New("people provider profile was not found")
	ErrInvalidProfile  = errors.New("people provider profile is invalid")
	ErrProfileActive   = errors.New("active people provider must be disabled before removal")
	ErrOnlyProfile     = errors.New("selected people provider is the only remaining profile")
)

type CheckConsentStore interface {
	HasSuccessfulPersonInferenceCheck(ctx context.Context, fingerprint string) (bool, error)
	HasActivePersonInferenceConsent(ctx context.Context, fingerprint string) (bool, error)
}

type RevocationStore interface {
	CheckConsentStore
	RevokePersonInferenceConsent(ctx context.Context, fingerprint, actor string) (bool, error)
	InvalidatePersonInferenceCheck(ctx context.Context, fingerprint string) (bool, error)
}

// ProfileRevocationStore also lists saved policies, so authority can be
// revoked for every policy that used a key whose profile is gone.
type ProfileRevocationStore interface {
	RevocationStore
	ListPersonInferenceProfiles(ctx context.Context) ([]peoplesweep.ProviderProfile, error)
}

// UnconfiguredKeyRemoval names a stored key whose profile is no longer in the
// config. Unconfigured reports whether the config still has no profile Name.
type UnconfiguredKeyRemoval struct {
	Name               string
	RunningFingerprint string
	Actor              string
	Store              ProfileRevocationStore
	Credentials        peoplesweep.CredentialStore
	Unconfigured       func() (bool, error)
}

// RemoveUnconfiguredKey revokes consent and discards checks for every saved
// policy that used the key, then deletes it. A new key under the same policy
// would otherwise inherit consent granted for the old one. The key is observed
// before the config is checked and deleted only in that state, so a profile
// added concurrently either keeps its key or is reported.
func RemoveUnconfiguredKey(ctx context.Context, removal UnconfiguredKeyRemoval) error {
	if err := peoplesweep.ValidateProviderProfileName(removal.Name); err != nil {
		return err
	}
	if removal.Store == nil || removal.Credentials == nil || removal.Unconfigured == nil {
		return errors.New("people provider key removal is unavailable")
	}
	revision, present, err := removal.Credentials.UnconfiguredRevision(removal.Name)
	if err != nil {
		return err
	}
	if !present {
		return ErrProfileMissing
	}
	if unconfigured, err := removal.Unconfigured(); err != nil {
		return err
	} else if !unconfigured {
		return ErrProfileExists
	}
	fingerprints, err := storedKeyFingerprints(ctx, removal.Store, removal.Name)
	if err != nil {
		return err
	}
	if removal.RunningFingerprint != "" && !slices.Contains(fingerprints, removal.RunningFingerprint) {
		fingerprints = append(fingerprints, removal.RunningFingerprint)
	}
	for _, fingerprint := range fingerprints {
		if _, err := removal.Store.RevokePersonInferenceConsent(ctx, fingerprint, removal.Actor); err != nil {
			return err
		}
		if _, err := removal.Store.InvalidatePersonInferenceCheck(ctx, fingerprint); err != nil {
			return err
		}
	}
	if _, err := removal.Credentials.DeleteIfRevision(removal.Name, revision); err != nil {
		return err
	}
	if unconfigured, err := removal.Unconfigured(); err != nil {
		return err
	} else if !unconfigured {
		return fmt.Errorf("people provider profile %q was added while its old key was deleted; store its key again", removal.Name)
	}
	return nil
}

func storedKeyFingerprints(ctx context.Context, st ProfileRevocationStore, name string) ([]string, error) {
	profiles, err := st.ListPersonInferenceProfiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("list people provider policies: %w", err)
	}
	fingerprints := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		if profile.Credential == peoplesweep.CredentialStored && profile.CredentialRef == name {
			fingerprints = append(fingerprints, profile.Fingerprint)
		}
	}
	return fingerprints, nil
}

// Service writes named policies with config ETags and gates selection on the
// exact check and consent records. It does not hold credentials in memory.
type Service struct {
	configPath string
	store      CheckConsentStore
}

func NewService(configPath string, store CheckConsentStore) *Service {
	return &Service{configPath: configPath, store: store}
}

type Selection struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	ETag        string `json:"etag"`
}

// CreateProfile publishes a policy but leaves selection and enablement alone.
func (s *Service) CreateProfile(ifMatch, name string, provider peoplesweep.ProviderConfig) (Selection, error) {
	if err := peoplesweep.ValidateProviderProfileName(name); err != nil {
		return Selection{}, err
	}
	snapshot, configured, err := s.readConfig(ifMatch)
	if err != nil {
		return Selection{}, err
	}
	if _, exists := configured.People.Sweep.Providers[name]; exists {
		return Selection{}, ErrProfileExists
	}
	proposed := configured.People.Sweep
	proposed.Providers = make(map[string]peoplesweep.ProviderConfig, len(configured.People.Sweep.Providers)+1)
	maps.Copy(proposed.Providers, configured.People.Sweep.Providers)
	proposed.Providers[name] = provider
	proposed.Provider = peoplesweep.ProviderSelection{Name: name}
	proposed.Enabled = true
	proposed.ApplyDefaults()
	profile, err := proposed.Profile()
	if err != nil {
		return Selection{}, fmt.Errorf("%w: %w", ErrInvalidProfile, err)
	}
	edits := []config.TableEdit{{
		Path:   []string{"people", "sweep", "providers", name},
		Values: peoplesweep.ProviderTOMLValues(provider), InsertOnly: true,
	}}
	if err := config.ValidateConfigTableEdits(snapshot, edits); err != nil {
		return Selection{}, err
	}
	written, err := config.EditConfigTables(s.configPath, ifMatch, edits)
	if err != nil {
		return Selection{}, err
	}
	return Selection{Name: name, Fingerprint: profile.Fingerprint, ETag: written.ETag}, nil
}

// SelectProfile enables an already checked and consented policy. The running
// daemon still uses its startup policy until its owner restarts it.
func (s *Service) SelectProfile(ctx context.Context, ifMatch, name string) (Selection, error) {
	if err := peoplesweep.ValidateProviderProfileName(name); err != nil {
		return Selection{}, err
	}
	_, configured, err := s.readConfig(ifMatch)
	if err != nil {
		return Selection{}, err
	}
	if _, exists := configured.People.Sweep.Providers[name]; !exists {
		return Selection{}, ErrProfileMissing
	}
	selected := configured.People.Sweep
	selected.Provider = peoplesweep.ProviderSelection{Name: name}
	selected.Enabled = true
	profile, err := selected.Profile()
	if err != nil {
		return Selection{}, err
	}
	if s.store == nil {
		return Selection{}, errors.New("people provider check and consent store is unavailable")
	}
	checked, err := s.store.HasSuccessfulPersonInferenceCheck(ctx, profile.Fingerprint)
	if err != nil {
		return Selection{}, err
	}
	if !checked {
		return Selection{}, ErrCheckRequired
	}
	consented, err := s.store.HasActivePersonInferenceConsent(ctx, profile.Fingerprint)
	if err != nil {
		return Selection{}, err
	}
	if !consented {
		return Selection{}, ErrConsentRequired
	}
	written, err := config.EditConfigTables(s.configPath, ifMatch, []config.TableEdit{{
		Path:   []string{"people", "sweep"},
		Values: map[string]any{"enabled": true, "provider": name},
	}})
	if err != nil {
		return Selection{}, err
	}
	return Selection{Name: name, Fingerprint: profile.Fingerprint, ETag: written.ETag}, nil
}

// Disable revokes authority for the configured and still-running policies
// before changing the saved enable flag. A daemon with a pending restart can
// therefore finish in-flight requests under their original policy, while new
// dispatches fail the existing consent gate immediately.
func (s *Service) Disable(
	ctx context.Context, ifMatch, runningFingerprint, actor string,
) (Selection, error) {
	snapshot, configured, err := s.readConfig(ifMatch)
	if err != nil {
		return Selection{}, err
	}
	revocations, ok := s.store.(RevocationStore)
	if !ok {
		return Selection{}, errors.New("people provider consent store is unavailable")
	}
	fingerprints := make(map[string]struct{})
	if runningFingerprint != "" {
		fingerprints[runningFingerprint] = struct{}{}
	}
	configuredName := configured.People.Sweep.Provider.Name
	if configuredName != "" {
		candidate := configured.People.Sweep
		candidate.Enabled = true
		if profile, profileErr := candidate.Profile(); profileErr == nil {
			fingerprints[profile.Fingerprint] = struct{}{}
		}
	}
	for fingerprint := range fingerprints {
		if _, err := revocations.RevokePersonInferenceConsent(ctx, fingerprint, actor); err != nil {
			return Selection{}, err
		}
	}
	written, err := config.EditConfigTables(s.configPath, snapshot.ETag, []config.TableEdit{{
		Path: []string{"people", "sweep"}, Values: map[string]any{"enabled": false},
	}})
	if err != nil {
		return Selection{}, err
	}
	return Selection{Name: configuredName, ETag: written.ETag}, nil
}

// RemoveProfile revokes saved and running authority and discards their checks
// before removing the saved policy. For a profile no longer in the config, it
// removes the leftover stored key instead; see RemoveUnconfiguredKey. The stored key observed before the config
// edit is deleted after the edit succeeds, and only if it is unchanged.
func (s *Service) RemoveProfile(
	ctx context.Context, ifMatch, name, runningFingerprint, actor string, credentials peoplesweep.CredentialStore,
) (Selection, error) {
	if err := peoplesweep.ValidateProviderProfileName(name); err != nil {
		return Selection{}, err
	}
	snapshot, configured, err := s.readConfig(ifMatch)
	if err != nil {
		return Selection{}, err
	}
	provider, exists := configured.People.Sweep.Providers[name]
	if !exists {
		if err := s.removeUnconfiguredKey(ctx, name, runningFingerprint, actor, credentials); err != nil {
			return Selection{}, err
		}
		return Selection{Name: name, ETag: snapshot.ETag}, nil
	}
	selected := configured.People.Sweep.Provider.Name == name
	if selected && configured.People.Sweep.Enabled {
		return Selection{}, ErrProfileActive
	}
	edits := make([]config.TableEdit, 0, 2)
	if selected {
		names := make([]string, 0, len(configured.People.Sweep.Providers)-1)
		for candidate := range configured.People.Sweep.Providers {
			if candidate != name {
				names = append(names, candidate)
			}
		}
		if len(names) == 0 {
			return Selection{}, ErrOnlyProfile
		}
		slices.Sort(names)
		edits = append(edits, config.TableEdit{
			Path: []string{"people", "sweep"}, Values: map[string]any{"provider": names[0]},
		})
	}
	edits = append(edits, config.TableEdit{Path: []string{"people", "sweep", "providers", name}, Remove: true})
	profileConfig := configured.People.Sweep
	profileConfig.Enabled = true
	profileConfig.Provider = peoplesweep.ProviderSelection{Name: name}
	profile, err := profileConfig.Profile()
	if err != nil {
		return Selection{}, err
	}
	if err := config.ValidateConfigTableEdits(snapshot, edits); err != nil {
		return Selection{}, err
	}
	var revision string
	present := false
	if provider.Credential == peoplesweep.CredentialStored {
		if credentials == nil {
			return Selection{}, errors.New("people provider credential store is unavailable")
		}
		revision, present, err = credentials.Revision(name, provider.Endpoint)
		if err != nil {
			return Selection{}, err
		}
	}
	revocations, ok := s.store.(RevocationStore)
	if !ok {
		return Selection{}, errors.New("people provider consent store is unavailable")
	}
	fingerprints := []string{profile.Fingerprint}
	if runningFingerprint != "" && runningFingerprint != profile.Fingerprint {
		fingerprints = append(fingerprints, runningFingerprint)
	}
	for _, fingerprint := range fingerprints {
		if _, err := revocations.RevokePersonInferenceConsent(ctx, fingerprint, actor); err != nil {
			return Selection{}, err
		}
		if _, err := revocations.InvalidatePersonInferenceCheck(ctx, fingerprint); err != nil {
			return Selection{}, err
		}
	}
	written, err := config.EditConfigTables(s.configPath, snapshot.ETag, edits)
	if err != nil {
		return Selection{}, err
	}
	if present {
		if _, err := credentials.DeleteIfRevision(name, revision); err != nil {
			_, restoreErr := config.RestoreConfigFile(s.configPath, written, snapshot)
			return Selection{}, errors.Join(err, restoreErr)
		}
	}
	return Selection{Name: name, Fingerprint: profile.Fingerprint, ETag: written.ETag}, nil
}

func (s *Service) removeUnconfiguredKey(
	ctx context.Context, name, runningFingerprint, actor string, credentials peoplesweep.CredentialStore,
) error {
	if credentials == nil {
		return ErrProfileMissing
	}
	st, ok := s.store.(ProfileRevocationStore)
	if !ok {
		return errors.New("people provider consent store is unavailable")
	}
	return RemoveUnconfiguredKey(ctx, UnconfiguredKeyRemoval{
		Name: name, RunningFingerprint: runningFingerprint, Actor: actor,
		Store: st, Credentials: credentials,
		Unconfigured: func() (bool, error) {
			current, err := config.ReadConfigFile(s.configPath)
			if err != nil {
				return false, err
			}
			loaded, err := config.LoadConfigFile(current, "")
			if err != nil {
				return false, err
			}
			_, exists := loaded.People.Sweep.Providers[name]
			return !exists, nil
		},
	})
}

func (s *Service) readConfig(ifMatch string) (config.ConfigFile, *config.Config, error) {
	snapshot, err := config.ReadConfigFile(s.configPath)
	if err != nil {
		return config.ConfigFile{}, nil, err
	}
	if snapshot.ETag != ifMatch {
		return config.ConfigFile{}, nil, config.ErrConfigConflict
	}
	loaded, err := config.LoadConfigFile(snapshot, "")
	if err != nil {
		return config.ConfigFile{}, nil, err
	}
	return snapshot, loaded, nil
}
