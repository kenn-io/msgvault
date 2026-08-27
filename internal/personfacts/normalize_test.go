package personfacts

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeClaimValueSupportedGenericTypes(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	tests := []struct {
		name      string
		valueType ValueType
		choices   []ChoiceDescriptor
		submitted string
		want      string
	}{
		{name: "text trims surrounding whitespace", valueType: ValueText, submitted: `"  Alice  "`, want: `"Alice"`},
		{name: "integer", valueType: ValueInteger, submitted: `42`, want: `42`},
		{name: "integer accepts semantic exponent form", valueType: ValueInteger, submitted: `4.2e1`, want: `42`},
		{name: "real canonicalizes semantic numeric form", valueType: ValueReal, submitted: `1.2500e1`, want: `12.5`},
		{name: "boolean", valueType: ValueBoolean, submitted: `true`, want: `true`},
		{name: "full calendar date", valueType: ValueDate, submitted: `"2024-02-29"`, want: `"2024-02-29"`},
		{name: "timestamp converts to UTC", valueType: ValueTimestamp, submitted: `"2024-01-02T03:04:05-05:00"`, want: `"2024-01-02T08:04:05Z"`},
		{name: "closed choice", valueType: ValueText, choices: []ChoiceDescriptor{{Value: "active", Label: "Active"}}, submitted: `"active"`, want: `"active"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			target := testAttributeTarget(test.valueType)
			target.Choices = test.choices
			normalized, failure, err := NormalizeClaimValue(target, json.RawMessage(test.submitted))
			require.NoError(err)
			require.Nil(failure)
			require.NotNil(normalized)
			assert.JSONEq(test.want, string(normalized.JSON))
			assert.NotEmpty(normalized.Fingerprint)
		})
	}

	integerA, failure, err := NormalizeClaimValue(testAttributeTarget(ValueInteger), json.RawMessage(`42`))
	requirements.NoError(err)
	requirements.Nil(failure)
	realA, failure, err := NormalizeClaimValue(testAttributeTarget(ValueReal), json.RawMessage(`42.0`))
	requirements.NoError(err)
	requirements.Nil(failure)
	realB, failure, err := NormalizeClaimValue(testAttributeTarget(ValueReal), json.RawMessage(`4.2e1`))
	requirements.NoError(err)
	requirements.Nil(failure)
	assertions.Equal(integerA.Fingerprint, realA.Fingerprint)
	assertions.Equal(realA.Fingerprint, realB.Fingerprint)
}

func TestNormalizeClaimValueRejectsUnsupportedAndUnknownFields(t *testing.T) {
	tests := []struct {
		name      string
		target    TargetDescriptor
		submitted string
		reason    DecisionReason
	}{
		{name: "unsupported generic type", target: testAttributeTarget(ValueOrganization), submitted: `{"name":"Example"}`, reason: ReasonUnsupportedTarget},
		{name: "integer rejects fractional form", target: testAttributeTarget(ValueInteger), submitted: `1.5`, reason: ReasonMalformedValue},
		{name: "invalid calendar date", target: testAttributeTarget(ValueDate), submitted: `"2023-02-29"`, reason: ReasonMalformedValue},
		{name: "timestamp requires zone", target: testAttributeTarget(ValueTimestamp), submitted: `"2024-01-02T03:04:05"`, reason: ReasonMalformedValue},
		{name: "choice is closed", target: choiceTarget(), submitted: `"unknown"`, reason: ReasonMalformedValue},
		{name: "employment unknown field", target: testEmploymentTarget(), submitted: `{"organization":{"name":"Example"},"titel":"Engineer"}`, reason: ReasonMalformedValue},
		{name: "employment case variant field", target: testEmploymentTarget(), submitted: `{"Organization":{"name":"Example"}}`, reason: ReasonMalformedValue},
		{name: "partial date day requires month", target: testEmploymentTarget(), submitted: `{"organization":{"name":"Example"},"start_date":{"year":2024,"day":1}}`, reason: ReasonMalformedValue},
		{name: "partial date rejects invalid day", target: testEmploymentTarget(), submitted: `{"organization":{"name":"Example"},"start_date":{"year":2023,"month":2,"day":29}}`, reason: ReasonMalformedValue},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			normalized, failure, err := NormalizeClaimValue(test.target, json.RawMessage(test.submitted))
			require.NoError(err)
			assert.Nil(normalized)
			require.NotNil(failure)
			assert.Equal(DecisionInvalid, failure.Action)
			assert.Equal(test.reason, failure.Reason)
			assert.NotEmpty(failure.Detail)
		})
	}
}

func TestNormalizeClaimValueRejectsOversizedIntegerExponentAsOutOfRange(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	normalized, failure, err := NormalizeClaimValue(
		testAttributeTarget(ValueInteger), json.RawMessage(`1e1000000000`))
	require.NoError(err)
	assert.Nil(normalized)
	require.NotNil(failure)
	assert.Equal(ReasonMalformedValue, failure.Reason)
	assert.Contains(failure.Detail, "outside int64 range")
}

func TestNormalizeClaimValueRejectsTextOverTargetMaxLength(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	target := testAttributeTarget(ValueText)
	target.MaxLength = 2

	normalized, failure, err := NormalizeClaimValue(target, json.RawMessage(`"🙂🙂🙂"`))
	require.NoError(err)
	assert.Nil(normalized)
	require.NotNil(failure)
	assert.Equal(DecisionInvalid, failure.Action)
	assert.Equal(ReasonMalformedValue, failure.Reason)
	assert.Contains(failure.Detail, "max_length 2")
}

func TestNormalizeClaimValueRetainsSubmittedJSONOnFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	input := validGenerationInput()
	input.Claims = []ProposedClaim{{
		Target: testAttributeTarget(ValueInteger), Relation: RelationSupport,
		SubmittedValue: json.RawMessage(`{"not":"an integer"`), Origin: OriginExtraction,
		Confidence: ConfidenceInputs{ReportedScore: 700},
	}}

	prepared, err := PreparePersonFactGeneration(t.Context(), input, nil)
	require.NoError(err)
	claims := prepared.Claims()
	require.Len(claims, 1)
	assert.True(bytes.Equal(json.RawMessage(`{"not":"an integer"`), claims[0].SubmittedValue),
		"malformed submitted bytes changed")
	assert.NotEmpty(claims[0].SubmittedFingerprint)
	assert.Nil(claims[0].Normalized)
	require.NotNil(claims[0].Failure)
	assert.Equal(ReasonMalformedValue, claims[0].Failure.Reason)
}

func TestCanonicalizeRawJSONNumbersLosslessly(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "deterministic nested value",
			input: `{"z":1.2300e+2,"a":[9007199254740993,0.00100,-0.0]}`,
			want:  `{"a":[9007199254740993,1e-3,0],"z":123}`,
		},
		{
			name:  "very large positive exponent stays compact",
			input: `1.00e1000000000000000000000`,
			want:  `1e1000000000000000000000`,
		},
		{
			name:  "very large negative exponent stays compact",
			input: `1e-1000000000000000000000`,
			want:  `1e-1000000000000000000000`,
		},
		{
			name:  "signed scaled zero is canonical zero",
			input: `-0.000e999999999999999999999`,
			want:  `0`,
		},
		{
			name:  "negative sign stays on nonzero coefficient",
			input: `-1.2300e2`,
			want:  `-123`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			canonical, err := canonicalizeRawJSON(json.RawMessage(test.input))
			require.NoError(t, err)
			assert.Equal(t, test.want, string(canonical))
		})
	}
}

func TestCanonicalizeRawJSONDistinguishesArbitraryPrecisionNumbers(t *testing.T) {
	tests := []struct {
		name  string
		left  string
		right string
	}{
		{
			name:  "adjacent integers above exact float64 range",
			left:  `9007199254740992`,
			right: `9007199254740993`,
		},
		{
			name:  "large decimals differing in final digit",
			left:  `12345678901234567890.12345678901234567890`,
			right: `12345678901234567890.12345678901234567891`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			left, err := canonicalizeRawJSON(json.RawMessage(test.left))
			require.NoError(err)
			right, err := canonicalizeRawJSON(json.RawMessage(test.right))
			require.NoError(err)

			assert.NotEqual(string(left), string(right))
			assert.NotEqual(fingerprint(left), fingerprint(right))
		})
	}
}

func TestCanonicalizeRawJSONUnifiesEquivalentNumbers(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   string
	}{
		{
			name: "large integer exponent forms",
			values: []string{
				`9223372036854775808000`,
				`9.223372036854775808e21`,
				`9223372036854775808e3`,
			},
			want: `9223372036854775808e3`,
		},
		{
			name:   "decimal exponent forms",
			values: []string{`123.4500`, `1.2345e2`, `12345e-2`},
			want:   `12345e-2`,
		},
		{
			name:   "zero forms",
			values: []string{`0`, `-0`, `0.000`, `-0e-999999999999999999999`},
			want:   `0`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, value := range test.values {
				canonical, err := canonicalizeRawJSON(json.RawMessage(value))
				require.NoError(t, err)
				assert.Equal(t, test.want, string(canonical), value)
			}
		})
	}
}

func TestCanonicalizeRawJSONRejectsMalformedInput(t *testing.T) {
	for _, input := range []string{"", `{"n":1`, `[1,]`, `1 2`} {
		t.Run(input, func(t *testing.T) {
			_, err := canonicalizeRawJSON(json.RawMessage(input))
			require.Error(t, err)
		})
	}
}

func TestNormalizeEmploymentValueCanonicalizesOrganization(t *testing.T) {
	require := require.New(t)

	submitted := json.RawMessage(`{
		"organization":{"id":42,"name":"  Example   GmbH ","domain":"https://www.bücher.example/jobs"},
		"title":"  Staff Engineer ","role":" Platform ","department":" R&D ","location":" Berlin ",
		"start_date":{"year":2020,"month":2},"end_date":{"year":2024,"month":2,"day":29}
	}`)
	normalized, failure, err := NormalizeClaimValue(testEmploymentTarget(), submitted)
	require.NoError(err)
	require.Nil(failure)
	require.NotNil(normalized)
	assert.JSONEq(t, `{
		"organization":{"id":42,"name":"Example GmbH","domain":"xn--bcher-kva.example"},
		"title":"Staff Engineer","role":"Platform","department":"R&D","location":"Berlin",
		"start_date":{"year":2020,"month":2},"end_date":{"year":2024,"month":2,"day":29}
	}`, string(normalized.JSON))
}

func TestNormalizeEmploymentValueRejectsReversedPartialDatesAtSharedPrecision(t *testing.T) {
	tests := []struct {
		name      string
		submitted string
		wantFail  bool
	}{
		{name: "reversed year", submitted: `{"organization":{"name":"Example"},"start_date":{"year":2025},"end_date":{"year":2024}}`, wantFail: true},
		{name: "reversed shared month", submitted: `{"organization":{"name":"Example"},"start_date":{"year":2025,"month":7},"end_date":{"year":2025,"month":6}}`, wantFail: true},
		{name: "reversed shared day", submitted: `{"organization":{"name":"Example"},"start_date":{"year":2025,"month":6,"day":2},"end_date":{"year":2025,"month":6,"day":1}}`, wantFail: true},
		{name: "end month is more precise", submitted: `{"organization":{"name":"Example"},"start_date":{"year":2025},"end_date":{"year":2025,"month":1}}`},
		{name: "start month is more precise", submitted: `{"organization":{"name":"Example"},"start_date":{"year":2025,"month":12},"end_date":{"year":2025}}`},
		{name: "end day is more precise", submitted: `{"organization":{"name":"Example"},"start_date":{"year":2025,"month":6},"end_date":{"year":2025,"month":6,"day":1}}`},
		{name: "equal date", submitted: `{"organization":{"name":"Example"},"start_date":{"year":2025,"month":6,"day":1},"end_date":{"year":2025,"month":6,"day":1}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			_, failure, err := NormalizeClaimValue(testEmploymentTarget(), json.RawMessage(test.submitted))
			require.NoError(err)
			if test.wantFail {
				require.NotNil(failure)
				assert.Equal(ReasonMalformedValue, failure.Reason)
				assert.Contains(failure.Detail, "start_date must not be after end_date")
				return
			}
			assert.Nil(failure)
		})
	}
}

func testAttributeTarget(valueType ValueType) TargetDescriptor {
	target := TargetDescriptor{
		Kind: TargetAttribute, Key: "attribute-id",
		UniversalID: "attribute-id", Slug: "attribute", Description: "Fixture attribute",
		ValueType: valueType, Cardinality: CardinalitySingle,
	}
	target.Revision = testDescriptorRevision(target)
	return target
}

func choiceTarget() TargetDescriptor {
	target := testAttributeTarget(ValueText)
	target.Choices = []ChoiceDescriptor{{Value: "active", Label: "Active"}, {Value: "inactive", Label: "Inactive"}}
	target.Revision = testDescriptorRevision(target)
	return target
}

func testEmploymentTarget() TargetDescriptor {
	target := employmentDescriptor()
	target.Revision = testDescriptorRevision(target)
	return target
}

func testDescriptorRevision(target TargetDescriptor) string {
	revision, err := DescriptorRevision(target)
	if err != nil {
		panic(err)
	}
	return revision
}

func utcTime(year, day, hour int) time.Time {
	return time.Date(year, time.January, day, hour, 0, 0, 0, time.UTC)
}
