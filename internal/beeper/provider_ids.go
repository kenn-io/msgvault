package beeper

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Provider identity for participant_contact_observations.provider_user_id.
//
// NOT to be confused with internal/beeper/anchors.go, whose AnchorProbe
// "anchors" are the sync-state reinstall guard. This file is about identity:
// the stable per-person key an observation is attached to.
//
// Values are always namespaced. Fallback keys are account-scoped. Native keys
// use length-delimited service, observed scope/account, and provider-ID
// components so delimiter characters cannot create collisions.
//
// Namespacing is load-bearing, not cosmetic. The matcher looks other
// participants up by exact provider_user_id across services, so a bare native
// ID (a numeric Telegram ID, a Discord snowflake) could collide with an
// unrelated ID on another network and auto-link two different people — the one
// outcome the roadmap's matching policy forbids.
const beeperProviderPrefix = "beeper"

// providerNativeKey is the one documented raw-payload key that may carry a
// provider-native immutable user ID. An unknown key must be ignored, not
// guessed at, because a wrong value here is the only input that can auto-link
// two participants.
const providerNativeKey = "providerID"

// providerUserIDScoped returns the namespaced stable identity for u.
// serviceSlug is the resolved communication-service slug, or "" when the
// bridge could not be classified; an unclassified service falls back to the
// account-scoped Beeper user ID because a native ID with no service to
// namespace it is exactly the cross-service collision hazard above.
//
// A provider-native ID is only safe to auto-link inside the service namespace
// and an observed scope. Services that expose a scope (for example a Slack
// account or a Matrix server) add that scope to the key. When no stronger
// scope is observed, the Beeper account supplies the namespace. Beeper's own
// fallback IDs are always account-scoped when an account is available.
func providerUserIDScoped(
	serviceSlug, accountID string, scopeKind, scopeValue *string, u *User,
) string {
	if u == nil || strings.TrimSpace(u.ID) == "" {
		return ""
	}
	if serviceSlug != "" {
		if native, ok := providerNativeUserID(u.Raw); ok {
			kind, value := "service", ""
			if scopeKind != nil && scopeValue != nil &&
				strings.TrimSpace(*scopeKind) != "" && strings.TrimSpace(*scopeValue) != "" {
				kind, value = strings.TrimSpace(*scopeKind), strings.TrimSpace(*scopeValue)
			} else if account := strings.TrimSpace(accountID); account != "" {
				// A service with no stronger observed scope still needs the
				// Beeper account to prevent the same native ID on two logins from
				// becoming an automatic cross-account link.
				kind, value = "account", account
			}
			return providerIdentityKey(serviceSlug, kind, value, native)
		}
	}
	return providerFallbackUserID(accountID, u.ID)
}

// providerFallbackUserID is the one durable namespace for an opaque Beeper
// user ID. The raw ID is only unique inside the selected account, so callers
// that persist fallback identities must use this helper rather than storing
// the payload's ID directly.
func providerFallbackUserID(accountID, userID string) string {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ""
	}
	if account := strings.TrimSpace(accountID); account != "" {
		return beeperProviderPrefix + ":" + encodeIdentityPart(account) + ":" +
			encodeIdentityPart(userID)
	}
	return beeperProviderPrefix + ":" + encodeIdentityPart(userID)
}

// providerIdentityKey uses length-delimited components. Separating fields
// with a raw colon is not collision-safe because service, scope, and provider
// values can contain that character.
func providerIdentityKey(serviceSlug, scopeKind, scopeValue, native string) string {
	return "provider:" + encodeIdentityPart(serviceSlug) + ":" +
		encodeIdentityPart(scopeKind) + ":" + encodeIdentityPart(scopeValue) + ":" +
		encodeIdentityPart(native)
}

func encodeIdentityPart(value string) string {
	return strconv.Itoa(len(value)) + ":" + value
}

// providerNativeUserID reads the documented providerID field from a raw user
// payload. Unknown fields are intentionally ignored: a guessed provider key
// can turn an unrelated value into an automatic identity link.
func providerNativeUserID(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", false
	}
	value, ok := fields[providerNativeKey]
	if !ok {
		return "", false
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return "", false
	}
	if trimmed := strings.TrimSpace(text); trimmed != "" {
		return trimmed, true
	}
	return "", false
}

// bridgePrefixFromUserID extracts the bridge type Beeper encodes in a
// Matrix-style user ID localpart ("@signal_<uuid>:beeper.local" -> "signal").
// It is the last rung of the service-resolution ladder, so it is deliberately
// strict: the prefix must be non-empty and made only of lowercase letters,
// digits, and dashes. Anything else returns "" and leaves the bridge
// unclassified rather than inventing a service.
func bridgePrefixFromUserID(userID string) string {
	localpart, _, ok := splitMatrixUserID(userID)
	if !ok {
		return ""
	}
	prefix, _, found := strings.Cut(localpart, "_")
	if !found || prefix == "" {
		return ""
	}
	for _, r := range prefix {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return ""
		}
	}
	return prefix
}

// matrixServerFromUserID returns the server part of a Matrix-style user ID.
// The matrix service's scope policy is "required" with a default scope kind of
// "server", and this is the only server value the archive actually observes.
func matrixServerFromUserID(userID string) string {
	_, server, ok := splitMatrixUserID(userID)
	if !ok {
		return ""
	}
	return server
}

// splitMatrixUserID splits "@localpart:server" into its two halves.
func splitMatrixUserID(userID string) (localpart, server string, ok bool) {
	trimmed := strings.TrimSpace(userID)
	if !strings.HasPrefix(trimmed, "@") {
		return "", "", false
	}
	localpart, server, found := strings.Cut(trimmed[1:], ":")
	if !found || localpart == "" || server == "" {
		return "", "", false
	}
	return localpart, server, true
}
