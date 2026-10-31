package beeperidentity

// ProviderIdentityKey encodes a provider identity using UTF-8 byte lengths.
// Callers choose the service/scope policy; this encoder preserves every byte.
func ProviderIdentityKey(serviceSlug, scopeKind, scopeValue, native string) string {
	return "provider:" + EncodePart(serviceSlug) + ":" + EncodePart(scopeKind) + ":" + EncodePart(scopeValue) + ":" + EncodePart(native)
}
