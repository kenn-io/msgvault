package beeperidentity

import "strings"

// ProviderKey builds the length-delimited stable provider identity key from
// its four components: service slug, scope kind, scope value, and the
// provider-native user ID. This is the one encoding for provider identity
// keys; beeper.providerIdentityKey and every caller that must produce a
// byte-identical key (today the Telegram export importer, and the archive
// migration that rewrites pre-shared-scope keys) call this function instead
// of restating the format.
//
// The components are length-prefixed with EncodePart so a value containing
// the delimiter cannot forge a different component split.
func ProviderKey(serviceSlug, scopeKind, scopeValue, nativeUserID string) string {
	return "provider:" + EncodePart(serviceSlug) + ":" +
		EncodePart(scopeKind) + ":" + EncodePart(scopeValue) + ":" +
		EncodePart(nativeUserID)
}

// DecodeProviderKey splits a key built by ProviderKey back into its
// components. ok is false unless key is exactly the four-part encoding, so a
// caller can distinguish a malformed or foreign value from an empty
// component (which is representable).
func DecodeProviderKey(key string) (serviceSlug, scopeKind, scopeValue, nativeUserID string, ok bool) {
	rest, found := strings.CutPrefix(key, "provider:")
	if !found {
		return "", "", "", "", false
	}
	var parts [4]string
	for i := range parts {
		part, remainder, found := decodePart(rest)
		if !found {
			return "", "", "", "", false
		}
		parts[i] = part
		rest = remainder
		if i < len(parts)-1 {
			if after, separated := strings.CutPrefix(rest, ":"); separated {
				rest = after
			} else {
				return "", "", "", "", false
			}
		}
	}
	if rest != "" {
		return "", "", "", "", false
	}
	return parts[0], parts[1], parts[2], parts[3], true
}

// NumericTelegramID reports whether value is a bare decimal Telegram user
// ID: digits only, with no sign, separator, or other punctuation. Telegram
// native user IDs have exactly this form, and every stored identity derived
// from one — the Beeper bridge, the Telegram export importer, and the
// archive migration that rewrites account-scoped keys — must accept the
// same set so one person cannot fork into two identities.
func NumericTelegramID(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// TelegramStableProviderKey returns the one service-scoped stable provider
// key for a numeric Telegram user ID — the key both a Telegram export
// importer and every Beeper bridge account produce for the same person, so
// the stable-ID matcher resolves them to one participant. Any other shape
// returns "": Telegram channel-style IDs are not user IDs, and an opaque
// value must not enter the shared namespace.
func TelegramStableProviderKey(nativeUserID string) string {
	if !NumericTelegramID(nativeUserID) {
		return ""
	}
	return ProviderKey("telegram", "service", "", nativeUserID)
}

// TelegramSharedProviderKey reports whether key is the shared
// service-scoped Telegram form. The key's own namespace is the whole
// service, so any scope columns stamped beside it must be empty: an account
// scope would split one identity into per-account edges that the matcher
// treats as unrelated pairs.
func TelegramSharedProviderKey(key string) bool {
	if key == "" {
		return false
	}
	service, _, _, native, ok := DecodeProviderKey(key)
	return ok && service == "telegram" && key == TelegramStableProviderKey(native)
}

// decodePart reads one EncodePart value from the front of rest. An empty
// length field is not something EncodePart produces, so it is rejected.
func decodePart(rest string) (part, remainder string, ok bool) {
	lengthSpec, after, found := strings.Cut(rest, ":")
	if !found || lengthSpec == "" {
		return "", "", false
	}
	length := 0
	for _, r := range lengthSpec {
		if r < '0' || r > '9' {
			return "", "", false
		}
		length = length*10 + int(r-'0')
		if length > len(after) {
			return "", "", false
		}
	}
	if length > len(after) {
		return "", "", false
	}
	return after[:length], after[length:], true
}
