package postaladdress

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
			assert.Equal(before, got.Original)
			assert.Equal(tc.want, got.Proposed)
			assert.Equal(StatusNormalizedUnverified, got.Status)
		})
	}
}

func TestAssessDoesNotAssertValidity(t *testing.T) {
	for _, tc := range []struct {
		input   Input
		reasons []string
	}{
		{Input{}, []string{"not_verified", "country_missing", "address_incomplete"}},
		{Input{CountryCode: "US"}, []string{"not_verified", "address_incomplete"}},
		{Input{StreetAddress: "somewhere"}, []string{"not_verified", "country_missing"}},
		{Input{StreetAddress: "somewhere", CountryCode: "ZZ"}, []string{"not_verified", "country_unknown"}},
	} {
		got := Assess(tc.input)
		assert.Equal(t, StatusNeedsReview, got.Status)
		assert.ElementsMatch(t, tc.reasons, got.ReasonCodes)
	}
}

func TestAssessmentExtendedParts(t *testing.T) {
	got := Assess(Input{ExtendedComponents: `;;12 Example St;City;;;;Room\;3;Floor 2`})
	assert.Equal(t, []string{"Room;3", "Floor 2"}, got.OriginalExtendedParts)
}
