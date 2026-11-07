package postaladdress

import (
	"reflect"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestAssessPreservesInternationalInput(t *testing.T) {
	for _, tc := range []struct {
		name        string
		input, want Input
	}{
		{"Japan", Input{AddressKind: "postal", StreetAddress: "  東京都千代田区\n丸の内１−２  ", ExtendedAddress: " ３０１号室 ", PostalCode: " 100-0000 ", CountryCode: " jp "}, Input{AddressKind: "postal", StreetAddress: "東京都千代田区\n丸の内１−２", ExtendedAddress: "３０１号室", PostalCode: "100-0000", CountryCode: "JP"}},
		{"PO box", Input{PostOfficeBox: "  PO Box 42 ", ExtendedAddress: "Suite 8", CountryCode: "gb", OriginalValue: "  my original\n text  "}, Input{PostOfficeBox: "PO Box 42", ExtendedAddress: "Suite 8", CountryCode: "GB", OriginalValue: "  my original\n text  "}},
		{"Unicode", Input{StreetAddress: "12, Rue de l’Été", Locality: "Montréal", CountryCode: "ca"}, Input{StreetAddress: "12, Rue de l’Été", Locality: "Montréal", CountryCode: "CA"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.input
			got := Assess(tc.input)
			assert := assert.New(t)
			assert.Equal(before, tc.input)
			assert.Equal(before, got.Original)
			assert.Equal(tc.want, got.Proposed)
			assert.Equal(Fingerprint(before), got.Fingerprint)
			assert.Equal(StatusNormalizedUnverified, got.Status)
		})
	}
}

func TestAssessDoesNotAssertValidity(t *testing.T) {
	for _, input := range []Input{{}, {StreetAddress: "somewhere"}, {StreetAddress: "12 Example Street", CountryCode: "ZZ"}, {CountryCode: "US"}, {StreetAddress: "12 Example Street", CountryCode: "US"}, {FreeText: "unknown conventions"}} {
		got := Assess(input)
		assert.Contains(t, []Status{StatusNormalizedUnverified, StatusNeedsReview}, got.Status)
		assert.Contains(t, got.ReasonCodes, "not_verified")
	}
	for _, input := range []Input{{}, {CountryCode: "US"}, {StreetAddress: "somewhere"}, {StreetAddress: "somewhere", CountryCode: "ZZ"}} {
		assert.Equal(t, StatusNeedsReview, Assess(input).Status)
	}
}

// The browser pins this same wire vector in postal-review.svelte.test.ts.
func TestFingerprintMatchesBrowserVector(t *testing.T) {
	input := Input{AddressKind: "postal", StreetAddress: " 12 Example St ", ExtendedAddress: "Unit 7", CountryCode: "ca", OriginalValue: " raw\ntext "}
	assert.Equal(t, "f7897d1e0b2a3120740b2882ec8937539e022e5ce8ccf969cfef6b253a0871a3", Fingerprint(input))
}

func TestFingerprintIncludesEveryField(t *testing.T) {
	base := Input{}
	for i := range reflect.TypeFor[Input]().NumField() {
		changed := base
		reflect.ValueOf(&changed).Elem().Field(i).SetString("changed")
		assert.NotEqual(t, Fingerprint(base), Fingerprint(changed), reflect.TypeFor[Input]().Field(i).Name)
	}
	assert.NotEqual(t, Fingerprint(Input{StreetAddress: "a", Locality: "bc"}), Fingerprint(Input{StreetAddress: "ab", Locality: "c"}))
}

func drawPostalInput(ht *hegel.T) Input {
	var input Input
	for i := range reflect.TypeFor[Input]().NumField() {
		reflect.ValueOf(&input).Elem().Field(i).SetString(hegel.Draw(ht, hegel.Text()))
	}
	return input
}

func TestAssessmentNormalizationIdempotent(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		input := drawPostalInput(ht)
		proposed := Assess(input).Proposed
		assert.Equal(ht, proposed, Assess(proposed).Proposed)
	}, hegel.WithTestCases(300), hegel.WithDatabase(""))
}
func TestAssessmentRetainsInput(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		input := drawPostalInput(ht)
		before := input
		got := Assess(input)
		assert.Equal(ht, before, input)
		assert.Equal(ht, before, got.Original)
	}, hegel.WithTestCases(300), hegel.WithDatabase(""))
}
func TestAssessmentNeverLocallyValidates(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		assert.Contains(ht, []Status{StatusNormalizedUnverified, StatusNeedsReview}, Assess(drawPostalInput(ht)).Status)
	}, hegel.WithTestCases(300), hegel.WithDatabase(""))
}
func TestOriginalSnapshotProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		input := drawPostalInput(ht)
		encoded, err := EncodeOriginalInput(input)
		require.NoError(ht, err)
		decoded, ok := DecodeOriginalInput(encoded)
		require.True(ht, ok)
		assert.Equal(ht, input, decoded)
	}, hegel.WithTestCases(300), hegel.WithDatabase(""))
}

func FuzzAssessment(f *testing.F) {
	for _, text := range []string{"", "東京\n３０１号室", "\x00", " msgvault:postal-input:v1\n{} ", "a\r\nb", "msgvault:postal-input:v1\n" + `{"schema":1,"input":{"original_value":"literal"}}`} {
		f.Add(text)
	}
	f.Fuzz(func(t *testing.T, text string) {
		input := Input{StreetAddress: text, OriginalValue: text, CountryCode: text}
		got := Assess(input)
		assert.Equal(t, got.Proposed, Assess(got.Proposed).Proposed)
		assert.Equal(t, input, got.Original)
		encoded, err := EncodeOriginalInput(input)
		if !utf8.ValidString(text) {
			require.Error(t, err)
			return
		}
		require.NoError(t, err)
		decoded, ok := DecodeOriginalInput(encoded)
		require.True(t, ok)
		assert.Equal(t, input, decoded)
	})
}
