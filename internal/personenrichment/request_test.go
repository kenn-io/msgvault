package personenrichment_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestBuildRequestEmitsOnlyMinimumAllowedIdentityAndExactTargets(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	target := requestTarget("attribute:bio", false)
	input := personenrichment.RequestInput{
		PersonID: 41, PersonRevision: 7,
		Names:             []personenrichment.IdentityCandidate{{StableID: 10, Value: " Alice Example ", Primary: true, ActiveFrom: requestTime(1)}},
		Emails:            []personenrichment.IdentityCandidate{{StableID: 20, Value: " ALICE@EXAMPLE.COM ", Primary: true, ActiveFrom: requestTime(2)}},
		Phones:            []personenrichment.IdentityCandidate{{StableID: 30, Value: "+1 415 555 0123", Primary: true, ActiveFrom: requestTime(3)}},
		CurrentCompanies:  []personenrichment.IdentityCandidate{{StableID: 40, Value: "Private Attribute Sentinel LLC", Primary: true, ActiveFrom: requestTime(4)}},
		PublicProfileURLs: []personenrichment.IdentityCandidate{{StableID: 50, Value: "https://example.com/CHAT_SENTINEL", Primary: true, ActiveFrom: requestTime(5)}},
		Catalog:           personfacts.Catalog{Version: "1", Targets: []personfacts.TargetDescriptor{target}},
		Trigger:           personenrichment.Trigger{Kind: personenrichment.TriggerIdentity, Generation: "person-revision:7"},
	}
	profile := personenrichment.ProviderProfile{
		Fingerprint: "profile-fingerprint", AllowedIdentifiers: []personenrichment.IdentifierClass{
			personenrichment.IdentifierEmail, personenrichment.IdentifierName,
		}, Targets: []personfacts.TargetDescriptor{target},
	}

	request, hashes, err := personenrichment.BuildRequest(input, profile)
	requirements.NoError(err)
	checks.NotEmpty(hashes.PayloadHash)
	checks.Regexp(`^[a-f0-9]{64}$`, hashes.RequestHash)
	checks.Equal(hashes.RequestHash, request.RequestHash)

	encoded, err := json.Marshal(request)
	requirements.NoError(err)
	want := `{"request_hash":"` + hashes.RequestHash + `","identity":{"name":"alice example","email":"alice@example.com"},"targets":[{"kind":"attribute","key":"attribute:bio","revision":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","universal_id":"attribute:bio","slug":"bio","description":"Public bio","value_type":"text","cardinality":"single","choices":[],"fields":[],"sensitive":false}]}`
	checks.JSONEq(want, string(encoded))
	checks.NotContains(string(encoded), "phone")
	checks.NotContains(string(encoded), "current_company")
	checks.NotContains(string(encoded), "public_profile_urls")
	checks.NotContains(string(encoded), "CHAT_SENTINEL")
	checks.NotContains(string(encoded), "Private Attribute Sentinel")
}

func TestBuildRequestTypesCannotRepresentArchiveOrLocalPersonContent(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[personenrichment.Request](),
		reflect.TypeFor[personenrichment.Identity](),
	} {
		for field := range typ.Fields() {
			searchable := strings.ToLower(field.Name + " " + field.Tag.Get("json"))
			for _, forbidden := range []string{
				"chat", "message", "email_body", "emailbody", "meeting", "archive",
				"excerpt", "private", "attribute", "person_id", "personid",
			} {
				assert.NotContains(t, searchable, forbidden, "%s.%s exposes forbidden content", typ.Name(), field.Name)
			}
		}
	}
}

func TestBuildRequestSelectsOneDeterministicEligibleIdentityPerClass(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	target := requestTarget("attribute:bio", false)
	input := personenrichment.RequestInput{
		PersonID: 41,
		Names: []personenrichment.IdentityCandidate{
			{StableID: 9, Value: "Newest Secondary", ActiveFrom: requestTime(9)},
			{StableID: 7, Value: "Older Primary", Primary: true, ActiveFrom: requestTime(3)},
			{StableID: 3, Value: "Chosen Primary", Primary: true, ActiveFrom: requestTime(8)},
			{StableID: 4, Value: "Higher ID Primary", Primary: true, ActiveFrom: requestTime(8)},
		},
		Emails: []personenrichment.IdentityCandidate{
			{StableID: 1, Value: "not selected@example.com", ActiveFrom: requestTime(10)},
			{StableID: 2, Value: " CHOSEN@EXAMPLE.COM ", Primary: true, ActiveFrom: requestTime(1)},
		},
		Phones: []personenrichment.IdentityCandidate{
			{StableID: 1, Value: "invalid", Primary: true, ActiveFrom: requestTime(10)},
			{StableID: 2, Value: "415-555-0123", ActiveFrom: requestTime(1)},
		},
		CurrentCompanies: []personenrichment.IdentityCandidate{{StableID: 1, Value: " Acme Labs ", ActiveFrom: requestTime(1)}},
		PublicProfileURLs: []personenrichment.IdentityCandidate{
			{StableID: 4, Value: "https://example.com/b?utm_source=x"},
			{StableID: 3, Value: "HTTPS://EXAMPLE.COM/a/#fragment"},
			{StableID: 2, Value: "https://example.com/b"},
			{StableID: 1, Value: "not a URL", Primary: true},
		},
		Catalog: personfacts.Catalog{Targets: []personfacts.TargetDescriptor{target}},
		Trigger: personenrichment.Trigger{Kind: personenrichment.TriggerTracked, Generation: "tracked:41"},
	}
	profile := personenrichment.ProviderProfile{
		Fingerprint: "profile", AllowedIdentifiers: []personenrichment.IdentifierClass{
			personenrichment.IdentifierPublicProfileURL, personenrichment.IdentifierCurrentCompany,
			personenrichment.IdentifierPhone, personenrichment.IdentifierEmail, personenrichment.IdentifierName,
		}, Targets: []personfacts.TargetDescriptor{target},
	}

	request, hashes, err := personenrichment.BuildRequest(input, profile)
	requirements.NoError(err)
	checks.Equal(personenrichment.Identity{
		Name: "chosen primary", Email: "chosen@example.com", Phone: "+14155550123",
		CurrentCompany: "acme labs", PublicProfileURLs: []string{
			"https://example.com/a", "https://example.com/b",
		},
	}, request.Identity)

	reorderedInput := input
	reorderedInput.Names = slices.Clone(input.Names)
	reorderedInput.Emails = slices.Clone(input.Emails)
	reorderedInput.Phones = slices.Clone(input.Phones)
	reorderedInput.CurrentCompanies = slices.Clone(input.CurrentCompanies)
	reorderedInput.PublicProfileURLs = slices.Clone(input.PublicProfileURLs)
	slices.Reverse(reorderedInput.Names)
	slices.Reverse(reorderedInput.Emails)
	slices.Reverse(reorderedInput.Phones)
	slices.Reverse(reorderedInput.CurrentCompanies)
	slices.Reverse(reorderedInput.PublicProfileURLs)
	reorderedProfile := profile
	reorderedProfile.AllowedIdentifiers = slices.Clone(profile.AllowedIdentifiers)
	slices.Reverse(reorderedProfile.AllowedIdentifiers)

	reorderedRequest, reorderedHashes, err := personenrichment.BuildRequest(reorderedInput, reorderedProfile)
	requirements.NoError(err)
	checks.Equal(request, reorderedRequest)
	checks.Equal(hashes, reorderedHashes)
}

func TestBuildRequestRejectsNoEligibleIdentity(t *testing.T) {
	target := requestTarget("attribute:bio", false)
	_, _, err := personenrichment.BuildRequest(personenrichment.RequestInput{
		PersonID: 1,
		Emails:   []personenrichment.IdentityCandidate{{StableID: 1, Value: " "}},
		Catalog:  personfacts.Catalog{Targets: []personfacts.TargetDescriptor{target}},
		Trigger:  personenrichment.Trigger{Kind: personenrichment.TriggerTracked, Generation: "tracked:1"},
	}, personenrichment.ProviderProfile{
		Fingerprint: "profile", AllowedIdentifiers: []personenrichment.IdentifierClass{personenrichment.IdentifierEmail},
		Targets: []personfacts.TargetDescriptor{target},
	})
	require.ErrorContains(t, err, "eligible identity")
}

func TestBuildRequestEnforcesExaModeIdentityRequirements(t *testing.T) {
	target := requestTarget("attribute:bio", false)
	base := personenrichment.RequestInput{
		PersonID:          1,
		Names:             []personenrichment.IdentityCandidate{{StableID: 1, Value: "Alice Example"}},
		Emails:            []personenrichment.IdentityCandidate{{StableID: 2, Value: "alice@example.com"}},
		CurrentCompanies:  []personenrichment.IdentityCandidate{{StableID: 3, Value: "Example Labs"}},
		PublicProfileURLs: []personenrichment.IdentityCandidate{{StableID: 4, Value: "https://example.com/alice"}},
		Catalog:           personfacts.Catalog{Targets: []personfacts.TargetDescriptor{target}},
		Trigger:           personenrichment.Trigger{Kind: personenrichment.TriggerTracked, Generation: "tracked:1"},
	}
	tests := []struct {
		name    string
		mode    string
		allowed []personenrichment.IdentifierClass
		wantErr string
	}{
		{name: "people rejects email only", mode: "people",
			allowed: []personenrichment.IdentifierClass{personenrichment.IdentifierEmail},
			wantErr: "public profile URL or name and current company"},
		{name: "people accepts name and company", mode: "people",
			allowed: []personenrichment.IdentifierClass{
				personenrichment.IdentifierName, personenrichment.IdentifierCurrentCompany,
			}},
		{name: "people accepts public profile URL", mode: "people",
			allowed: []personenrichment.IdentifierClass{personenrichment.IdentifierPublicProfileURL}},
		{name: "deep rejects name and company", mode: "deep",
			allowed: []personenrichment.IdentifierClass{
				personenrichment.IdentifierName, personenrichment.IdentifierCurrentCompany,
			}, wantErr: "public profile URL"},
		{name: "deep reasoning accepts public profile URL", mode: "deep-reasoning",
			allowed: []personenrichment.IdentifierClass{personenrichment.IdentifierPublicProfileURL}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := personenrichment.BuildRequest(base, personenrichment.ProviderProfile{
				Kind: personenrichment.ProviderExa, Mode: test.mode, Fingerprint: "profile",
				AllowedIdentifiers: test.allowed, Targets: []personfacts.TargetDescriptor{target},
			})
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestBuildRequestOmitsIncompleteOptionalNamePairWhenExaUsesProfileURL(t *testing.T) {
	target := requestTarget("attribute:bio", false)
	request, _, err := personenrichment.BuildRequest(personenrichment.RequestInput{
		PersonID:          1,
		Names:             []personenrichment.IdentityCandidate{{StableID: 1, Value: "Alice Example"}},
		PublicProfileURLs: []personenrichment.IdentityCandidate{{StableID: 2, Value: "https://example.com/alice"}},
		Catalog:           personfacts.Catalog{Targets: []personfacts.TargetDescriptor{target}},
		Trigger:           personenrichment.Trigger{Kind: personenrichment.TriggerTracked, Generation: "tracked:1"},
	}, personenrichment.ProviderProfile{
		Kind: personenrichment.ProviderExa, Mode: "people", Fingerprint: "profile",
		AllowedIdentifiers: []personenrichment.IdentifierClass{
			personenrichment.IdentifierName, personenrichment.IdentifierCurrentCompany,
			personenrichment.IdentifierPublicProfileURL,
		}, Targets: []personfacts.TargetDescriptor{target},
	})
	require.NoError(t, err)
	assert.Empty(t, request.Identity.Name)
	assert.Empty(t, request.Identity.CurrentCompany)
	assert.Equal(t, []string{"https://example.com/alice"}, request.Identity.PublicProfileURLs)
}

func TestBuildRequestEnforcesSixtyfourSuppressionBoundIdentity(t *testing.T) {
	target := requestTarget("attribute:bio", false)
	base := personenrichment.RequestInput{
		PersonID:          1,
		Names:             []personenrichment.IdentityCandidate{{StableID: 1, Value: "Alice Example"}},
		Emails:            []personenrichment.IdentityCandidate{{StableID: 2, Value: "alice@example.com"}},
		Phones:            []personenrichment.IdentityCandidate{{StableID: 3, Value: "+14155550123"}},
		CurrentCompanies:  []personenrichment.IdentityCandidate{{StableID: 4, Value: "Example Labs"}},
		PublicProfileURLs: []personenrichment.IdentityCandidate{{StableID: 5, Value: "https://example.com/alice"}},
		Catalog:           personfacts.Catalog{Targets: []personfacts.TargetDescriptor{target}},
		Trigger:           personenrichment.Trigger{Kind: personenrichment.TriggerTracked, Generation: "tracked:1"},
	}
	tests := []struct {
		name    string
		allowed []personenrichment.IdentifierClass
		wantErr string
	}{
		{name: "rejects profile URL only", allowed: []personenrichment.IdentifierClass{
			personenrichment.IdentifierPublicProfileURL,
		}, wantErr: "Sixtyfour"},
		{name: "rejects name only", allowed: []personenrichment.IdentifierClass{
			personenrichment.IdentifierName,
		}, wantErr: "name and current company"},
		{name: "rejects company only", allowed: []personenrichment.IdentifierClass{
			personenrichment.IdentifierCurrentCompany,
		}, wantErr: "name and current company"},
		{name: "rejects email without verifiable response binding", allowed: []personenrichment.IdentifierClass{
			personenrichment.IdentifierEmail,
		}, wantErr: "name and current company"},
		{name: "rejects phone without verifiable response binding", allowed: []personenrichment.IdentifierClass{
			personenrichment.IdentifierPhone,
		}, wantErr: "name and current company"},
		{name: "accepts name and company", allowed: []personenrichment.IdentifierClass{
			personenrichment.IdentifierName, personenrichment.IdentifierCurrentCompany,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := personenrichment.BuildRequest(base, personenrichment.ProviderProfile{
				Kind: personenrichment.ProviderSixtyfour, Fingerprint: "profile",
				AllowedIdentifiers: test.allowed, Targets: []personfacts.TargetDescriptor{target},
			})
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestBuildRequestRejectsSensitiveOrStaleTargetDescriptors(t *testing.T) {
	sensitive := requestTarget("attribute:sensitive", true)
	baseInput := personenrichment.RequestInput{
		PersonID: 1, Names: []personenrichment.IdentityCandidate{{StableID: 1, Value: "Alice Example"}},
		Catalog: personfacts.Catalog{Targets: []personfacts.TargetDescriptor{sensitive}},
		Trigger: personenrichment.Trigger{Kind: personenrichment.TriggerTracked, Generation: "tracked:1"},
	}
	profile := personenrichment.ProviderProfile{
		Fingerprint: "profile", AllowedIdentifiers: []personenrichment.IdentifierClass{personenrichment.IdentifierName},
		Targets: []personfacts.TargetDescriptor{sensitive},
	}
	_, _, err := personenrichment.BuildRequest(baseInput, profile)
	require.ErrorContains(t, err, "sensitive")

	profile.AllowSensitiveTargets = true
	staleInput := baseInput
	staleInput.Catalog.Targets = slices.Clone(baseInput.Catalog.Targets)
	staleInput.Catalog.Targets[0].Description = "Changed current descriptor"
	_, _, err = personenrichment.BuildRequest(staleInput, profile)
	require.ErrorContains(t, err, "descriptor")
}

func TestBuildRequestUsesOnlyExactProfileTargetScope(t *testing.T) {
	bio := requestTarget("attribute:bio", false)
	timezone := requestTarget("attribute:timezone", false)
	input := personenrichment.RequestInput{
		PersonID: 1, Names: []personenrichment.IdentityCandidate{{StableID: 1, Value: "Alice Example"}},
		Catalog: personfacts.Catalog{Targets: []personfacts.TargetDescriptor{timezone, bio}},
		Trigger: personenrichment.Trigger{Kind: personenrichment.TriggerTracked, Generation: "tracked:1"},
	}
	request, _, err := personenrichment.BuildRequest(input, personenrichment.ProviderProfile{
		Fingerprint: "profile", AllowedIdentifiers: []personenrichment.IdentifierClass{personenrichment.IdentifierName},
		Targets: []personfacts.TargetDescriptor{bio},
	})
	require.NoError(t, err)
	assert.Equal(t, []personfacts.TargetDescriptor{bio}, request.Targets)
}

func TestPayloadHashAndRequestHashAreCanonicalAndTriggerScoped(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	targetA := requestTarget("attribute:bio", false)
	targetB := requestTarget("attribute:timezone", false)
	identity := personenrichment.Identity{
		Email: "alice@example.com", PublicProfileURLs: []string{"https://example.com/a", "https://example.com/b"},
	}
	payload, err := personenrichment.PayloadHash("profile", identity, []personfacts.TargetDescriptor{targetB, targetA})
	requirements.NoError(err)
	reorderedPayload, err := personenrichment.PayloadHash("profile", personenrichment.Identity{
		Email: "alice@example.com", PublicProfileURLs: []string{"https://example.com/b", "https://example.com/a"},
	}, []personfacts.TargetDescriptor{targetA, targetB})
	requirements.NoError(err)
	checks.Equal(payload, reorderedPayload)

	first, err := personenrichment.RequestHash(41, payload, personenrichment.Trigger{
		Kind: personenrichment.TriggerRefresh, Generation: "2026-08-23T00:00:00Z/24h",
	})
	requirements.NoError(err)
	retry, err := personenrichment.RequestHash(41, payload, personenrichment.Trigger{
		Kind: personenrichment.TriggerRefresh, Generation: "2026-08-23T00:00:00Z/24h",
	})
	requirements.NoError(err)
	later, err := personenrichment.RequestHash(41, payload, personenrichment.Trigger{
		Kind: personenrichment.TriggerRefresh, Generation: "2026-08-24T00:00:00Z/24h",
	})
	requirements.NoError(err)
	checks.Equal(first, retry)
	checks.NotEqual(first, later)
}

func TestRequestHashRequiresLocalPersonPayloadAndTriggerGeneration(t *testing.T) {
	tests := []struct {
		name      string
		personID  int64
		payload   string
		trigger   personenrichment.Trigger
		wantError string
	}{
		{name: "person", payload: strings.Repeat("a", 64), trigger: personenrichment.Trigger{Kind: personenrichment.TriggerManual, Generation: "operator-key"}, wantError: "person"},
		{name: "payload", personID: 1, payload: "", trigger: personenrichment.Trigger{Kind: personenrichment.TriggerManual, Generation: "operator-key"}, wantError: "payload"},
		{name: "kind", personID: 1, payload: strings.Repeat("a", 64), trigger: personenrichment.Trigger{Generation: "operator-key"}, wantError: "trigger kind"},
		{name: "generation", personID: 1, payload: strings.Repeat("a", 64), trigger: personenrichment.Trigger{Kind: personenrichment.TriggerManual}, wantError: "generation"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := personenrichment.RequestHash(test.personID, test.payload, test.trigger)
			require.ErrorContains(t, err, test.wantError)
		})
	}
}

func TestAssessIdentityAcceptsOnlyExactStrongEvidenceOrNameCompanyThreshold(t *testing.T) {
	request := personenrichment.Request{Identity: personenrichment.Identity{
		Name: "alice example", Email: "alice@example.com", Phone: "+14155550123",
		CurrentCompany: "acme labs", PublicProfileURLs: []string{"https://example.com/alice"},
	}}
	tests := []struct {
		name     string
		result   personenrichment.Result
		verified []personenrichment.ProviderPersonID
		want     personenrichment.IdentityAssessment
	}{
		{
			name:     "previously verified opaque provider ID exact bytes",
			result:   personenrichment.Result{ProviderPersonIDs: []personenrichment.ProviderPersonID{{ID: " Provider/AbC "}}},
			verified: []personenrichment.ProviderPersonID{{ID: "Provider/AbC"}},
			want:     personenrichment.IdentityAssessment{Accepted: true, Score: 1000, Reason: "verified_provider_person_id"},
		},
		{
			name:   "strong email exact after normalization",
			result: personenrichment.Result{IdentityMatches: []personenrichment.IdentityMatch{{Class: personenrichment.IdentifierEmail, Value: " ALICE@EXAMPLE.COM ", Confidence: 200}}},
			want:   personenrichment.IdentityAssessment{Accepted: true, Score: 1000, Reason: "strong_identifier_match", MatchedClasses: []personenrichment.IdentifierClass{personenrichment.IdentifierEmail}},
		},
		{
			name: "name company composite at threshold",
			result: personenrichment.Result{IdentityConfidence: 900, IdentityMatches: []personenrichment.IdentityMatch{
				{Class: personenrichment.IdentifierCurrentCompany, Value: " ACME  LABS "},
				{Class: personenrichment.IdentifierName, Value: "Alice Example"},
			}},
			want: personenrichment.IdentityAssessment{Accepted: true, Score: 900, Reason: "name_company_match", MatchedClasses: []personenrichment.IdentifierClass{personenrichment.IdentifierName, personenrichment.IdentifierCurrentCompany}},
		},
		{
			name:     "provider ID case mismatch",
			result:   personenrichment.Result{ProviderPersonIDs: []personenrichment.ProviderPersonID{{ID: "provider/abc"}}},
			verified: []personenrichment.ProviderPersonID{{ID: "Provider/AbC"}},
			want:     personenrichment.IdentityAssessment{Reason: "identity_not_verified"},
		},
		{
			name:   "name alone",
			result: personenrichment.Result{IdentityConfidence: 1000, IdentityMatches: []personenrichment.IdentityMatch{{Class: personenrichment.IdentifierName, Value: "alice example"}}},
			want:   personenrichment.IdentityAssessment{Reason: "identity_not_verified"},
		},
		{
			name:   "company alone",
			result: personenrichment.Result{IdentityConfidence: 1000, IdentityMatches: []personenrichment.IdentityMatch{{Class: personenrichment.IdentifierCurrentCompany, Value: "acme labs"}}},
			want:   personenrichment.IdentityAssessment{Reason: "identity_not_verified"},
		},
		{
			name:   "confidence alone",
			result: personenrichment.Result{IdentityConfidence: 1000},
			want:   personenrichment.IdentityAssessment{Reason: "identity_not_verified"},
		},
		{
			name: "name company below threshold",
			result: personenrichment.Result{IdentityConfidence: 899, IdentityMatches: []personenrichment.IdentityMatch{
				{Class: personenrichment.IdentifierName, Value: "alice example"},
				{Class: personenrichment.IdentifierCurrentCompany, Value: "acme labs"},
			}},
			want: personenrichment.IdentityAssessment{Reason: "identity_not_verified"},
		},
		{
			name:   "strong identifier differs",
			result: personenrichment.Result{IdentityConfidence: 1000, IdentityMatches: []personenrichment.IdentityMatch{{Class: personenrichment.IdentifierPhone, Value: "+14155559999"}}},
			want:   personenrichment.IdentityAssessment{Reason: "identity_not_verified"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := personenrichment.AssessIdentity(request, test.result, test.verified)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestAssessIdentityIsStableUnderReturnedMatchReordering(t *testing.T) {
	request := personenrichment.Request{Identity: personenrichment.Identity{
		Name: "alice example", CurrentCompany: "acme labs",
	}}
	matches := []personenrichment.IdentityMatch{
		{Class: personenrichment.IdentifierName, Value: "alice example"},
		{Class: personenrichment.IdentifierName, Value: "different person"},
		{Class: personenrichment.IdentifierCurrentCompany, Value: "different company"},
		{Class: personenrichment.IdentifierCurrentCompany, Value: "acme labs"},
	}
	want := personenrichment.IdentityAssessment{
		Accepted: true, Score: 900, Reason: "name_company_match",
		MatchedClasses: []personenrichment.IdentifierClass{
			personenrichment.IdentifierName, personenrichment.IdentifierCurrentCompany,
		},
	}

	forward := personenrichment.AssessIdentity(request, personenrichment.Result{
		IdentityConfidence: 900, IdentityMatches: matches,
	}, nil)
	slices.Reverse(matches)
	reversed := personenrichment.AssessIdentity(request, personenrichment.Result{
		IdentityConfidence: 900, IdentityMatches: matches,
	}, nil)
	assert.Equal(t, want, forward)
	assert.Equal(t, want, reversed)
}

func TestAssessIdentityDoesNotConflateEscapedSlashWithPathSeparator(t *testing.T) {
	request := personenrichment.Request{Identity: personenrichment.Identity{
		PublicProfileURLs: []string{"https://example.com/people/alice%2Fexample"},
	}}

	exact := personenrichment.AssessIdentity(request, personenrichment.Result{
		IdentityMatches: []personenrichment.IdentityMatch{{
			Class: personenrichment.IdentifierPublicProfileURL,
			Value: "https://example.com/people/alice%2fexample",
		}},
	}, nil)
	assert.Equal(t, personenrichment.IdentityAssessment{
		Accepted: true, Score: 1000, Reason: "strong_identifier_match",
		MatchedClasses: []personenrichment.IdentifierClass{personenrichment.IdentifierPublicProfileURL},
	}, exact)

	separator := personenrichment.AssessIdentity(request, personenrichment.Result{
		IdentityMatches: []personenrichment.IdentityMatch{{
			Class: personenrichment.IdentifierPublicProfileURL,
			Value: "https://example.com/people/alice/example",
		}},
	}, nil)
	assert.Equal(t, personenrichment.IdentityAssessment{Reason: "identity_not_verified"}, separator)
}

func requestTarget(key string, sensitive bool) personfacts.TargetDescriptor {
	slug := strings.TrimPrefix(key, "attribute:")
	return personfacts.TargetDescriptor{
		Kind: personfacts.TargetAttribute, Key: key,
		Revision:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		UniversalID: key, Slug: slug, Description: "Public " + slug,
		ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle,
		Choices: []personfacts.ChoiceDescriptor{}, Fields: []personfacts.FieldDescriptor{}, Sensitive: sensitive,
	}
}

func requestTime(day int) time.Time {
	return time.Date(2026, 8, day, 0, 0, 0, 0, time.UTC)
}
