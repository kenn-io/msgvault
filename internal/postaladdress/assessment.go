// Package postaladdress assesses postal input without asserting deliverability.
package postaladdress

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"time"
)

// Input contains postal components and original text, never provider metadata.
// Strings preserve the source's script, punctuation and line boundaries.
type Input struct {
	AddressKind        string `json:"address_kind"`
	PostOfficeBox      string `json:"post_office_box,omitempty"`
	ExtendedAddress    string `json:"extended_address,omitempty"`
	StreetAddress      string `json:"street_address,omitempty"`
	Locality           string `json:"locality,omitempty"`
	Region             string `json:"region,omitempty"`
	PostalCode         string `json:"postal_code,omitempty"`
	CountryName        string `json:"country_name,omitempty"`
	ExtendedComponents string `json:"extended_components,omitempty"`
	FreeText           string `json:"free_text,omitempty"`
	Label              string `json:"label,omitempty"`
	GeoURI             string `json:"geo_uri,omitempty"`
	Timezone           string `json:"timezone,omitempty"`
	CountryCode        string `json:"country_code,omitempty"`
	PlaceURI           string `json:"place_uri,omitempty"`
	OriginalValue      string `json:"original_value,omitempty"`
}

type Status string

const (
	StatusNormalizedUnverified Status = "normalized_unverified"
	StatusNeedsReview          Status = "needs_review"
	StatusValidated            Status = "validated"
	StatusUnavailable          Status = "unavailable"
)

// Assessment is derived for local reads or a transient explicit external check.
// It is not a durable verification flag and never promises deliverability.
type Assessment struct {
	Original    Input      `json:"original"`
	Proposed    Input      `json:"proposed"`
	Status      Status     `json:"status"`
	ReasonCodes []string   `json:"reason_codes"`
	Fingerprint string     `json:"fingerprint"`
	Provider    string     `json:"provider,omitempty"`
	CheckedAt   *time.Time `json:"checked_at,omitzero" nullable:"false"`
}

func (in *Input) fields() []*string {
	return []*string{&in.AddressKind, &in.PostOfficeBox, &in.ExtendedAddress, &in.StreetAddress, &in.Locality, &in.Region, &in.PostalCode, &in.CountryName, &in.ExtendedComponents, &in.FreeText, &in.Label, &in.GeoURI, &in.Timezone, &in.CountryCode, &in.PlaceURI, &in.OriginalValue}
}

// Assess only trims component boundaries and uppercases two-letter country
// codes. It does not impose one country's street or postal-code conventions.
func Assess(input Input) Assessment {
	proposed := input
	for _, field := range proposed.fields() {
		*field = strings.TrimSpace(*field)
	}
	proposed.OriginalValue = input.OriginalValue
	if asciiCountryCode(proposed.CountryCode) {
		proposed.CountryCode = strings.ToUpper(proposed.CountryCode)
	}
	result := Assessment{Original: input, Proposed: proposed, Status: StatusNormalizedUnverified, ReasonCodes: []string{"not_verified"}, Fingerprint: Fingerprint(input)}
	if proposed.CountryCode == "" {
		result.Status = StatusNeedsReview
		result.ReasonCodes = append(result.ReasonCodes, "country_missing")
	} else if !knownCountry(proposed.CountryCode) {
		result.Status = StatusNeedsReview
		result.ReasonCodes = append(result.ReasonCodes, "country_unknown")
	}
	if proposed.StreetAddress == "" && proposed.PostOfficeBox == "" {
		result.Status = StatusNeedsReview
		result.ReasonCodes = append(result.ReasonCodes, "address_incomplete")
	}
	return result
}

// Fingerprint binds every draft component, including whitespace and original
// text. Length framing prevents ambiguous component boundaries.
func Fingerprint(input Input) string {
	hash := sha256.New()
	for _, field := range input.fields() {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(*field)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(*field))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func asciiCountryCode(code string) bool {
	if len(code) != 2 {
		return false
	}
	for i := range len(code) {
		if (code[i] < 'a' || code[i] > 'z') && (code[i] < 'A' || code[i] > 'Z') {
			return false
		}
	}
	return true
}

// ISO 3166-1 alpha-2 membership is only a syntax aid; it says nothing about
// provider coverage or the validity of an address in a country.
func knownCountry(code string) bool {
	const codes = " AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW "
	return len(code) == 2 && strings.Contains(codes, " "+code+" ")
}
