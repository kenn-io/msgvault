package line

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

// UserIdentifier qualifies an opaque ID with a validated decimal channel ID.
func UserIdentifier(channelID, userID string) string {
	return "channel:" + channelID + ":user:" + userID
}

// Register proves channel credentials before publishing its source and account
// identity. It does not set a webhook until the daemon receiver is activated.
func Register(ctx context.Context, st *store.Store, client *Client, channelID string) (*store.Source, error) {
	if st == nil || client == nil {
		return nil, errors.New("LINE registration unavailable")
	}
	if !config.ValidLineChannelID(channelID) || client.channelID != channelID {
		return nil, errors.New("invalid LINE channel ID")
	}
	service, _, err := st.EnsureCommunicationServiceContext(ctx, store.CommunicationServiceInput{Slug: "line", DisplayLabel: "LINE", ScopePolicy: store.ScopePolicyOptional, DefaultScopeKind: new("line_channel"), Normalization: store.NormalizationLower, NormalizationVersion: 1})
	if err != nil {
		return nil, err
	}
	if err = store.ValidateServiceScope(service, new("line_channel"), new(channelID)); err != nil {
		return nil, fmt.Errorf("LINE service must accept line_channel scope; update its scope policy before registration: %w", err)
	}
	bot, err := client.BotInfo(ctx)
	if err != nil {
		return nil, err
	}
	if bot.UserID == "" {
		return nil, errors.New("LINE bot identity missing from authenticated response")
	}
	source, err := st.GetOrCreateSourceContext(ctx, SourceType, channelID)
	if err != nil {
		return nil, err
	}
	prior, err := st.LineBotUserIDContext(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	if prior != "" && prior != bot.UserID {
		return nil, errors.New("LINE channel bot identity changed; check the configured credentials")
	}
	if err = st.SetLineBotContext(ctx, source.ID, bot.UserID); err != nil {
		return nil, err
	}
	if err = st.UpdateSourceDisplayNameContext(ctx, source.ID, bot.DisplayName); err != nil && !errors.Is(err, store.ErrSourceSettingsInvalid) {
		return nil, err
	}
	if _, err = st.EnsureParticipantByIdentifierContext(ctx, "line_user_id", UserIdentifier(channelID, bot.UserID), bot.DisplayName); err != nil {
		return nil, err
	}
	if err = st.AddAccountIdentityContext(ctx, source.ID, UserIdentifier(channelID, bot.UserID), "line-bot"); err != nil {
		return nil, err
	}
	return st.GetSourceByIDContext(ctx, source.ID)
}

// participantIdentifier also gives unidentified senders a stable per-chat key.
func participantIdentifier(channelID, conversationKey, userID string) string {
	if userID == "" {
		return "channel:" + channelID + ":" + conversationKey
	}
	return UserIdentifier(channelID, userID)
}

func (p *Processor) participant(ctx context.Context, conversationKey, userID, name string) (int64, error) {
	identifier := participantIdentifier(p.channelID, conversationKey, userID)
	if userID == "" {
		return p.store.EnsureParticipantByIdentifierContext(ctx, "line_anonymous", identifier, "")
	}
	pid, err := p.store.EnsureParticipantByIdentifierContext(ctx, "line_user_id", identifier, name)
	if err != nil {
		return 0, err
	}
	_, err = p.store.RecordContactObservationContext(ctx, pid, store.ParticipantContactObservationInput{SourceID: &p.sourceID, AddressKind: store.ContactAddressProviderIdentity, ServiceSlug: new("line"), ScopeKind: new("line_channel"), ScopeValue: &p.channelID, ProviderUserID: &userID, OriginalValue: userID, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation, SourceRef: &identifier}})
	return pid, err
}

func (p *Processor) cachedProfile(ctx context.Context, userID string) (Profile, bool, error) {
	raw, at, err := p.store.LineProfileContext(ctx, p.sourceID, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, false, nil
	}
	if err != nil {
		return Profile{}, false, err
	}
	var profile Profile
	if err = json.Unmarshal([]byte(raw), &profile); err != nil {
		return Profile{}, false, err
	}
	return profile, p.now().Sub(at) < 24*time.Hour, nil
}

func (p *Processor) profile(ctx context.Context, source Source, userID string) (Profile, error) {
	if userID == "" {
		return Profile{}, nil
	}
	profile, fresh, err := p.cachedProfile(ctx, userID)
	if err != nil {
		return profile, err
	}
	if fresh {
		return profile, nil
	}
	previousName := profile.DisplayName
	profile, err = p.client.Profile(ctx, source, userID)
	if err != nil {
		return profile, retryEvent(err)
	}
	if profile.UserID != userID {
		return Profile{}, retryEvent(errors.New("LINE profile identity does not match the requested user"))
	}
	key, _, err := source.ConversationID()
	if err != nil {
		return Profile{}, invalidEvent("invalid LINE profile source")
	}
	pid, err := p.participant(ctx, key, userID, profile.DisplayName)
	if err != nil {
		return Profile{}, err
	}
	if err = p.store.UpdateLineParticipantNameContext(ctx, pid, previousName, profile.DisplayName); err != nil {
		return Profile{}, err
	}
	raw, err := json.Marshal(profile)
	if err != nil {
		return Profile{}, err
	}
	if err = p.store.CacheLineProfileContext(ctx, p.sourceID, userID, string(raw), p.now()); err != nil {
		return Profile{}, err
	}
	return profile, nil
}
