package muesli

import "strings"

// NormalizeContactPhone converts a phone number as typed in Contacts to E.164.
// Numbers written with + or 00 always convert. A national number converts only
// when countryCode (for example "1" or "44") is configured; msgvault never
// guesses a country, because a wrong one could match somebody else's phone.
// An extension is dropped. The result has 7 to 15 digits.
func NormalizeContactPhone(raw, countryCode string) (string, bool) {
	// "+44 (0)20 …" marks a trunk zero that is dialed only nationally.
	value := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(raw)), "(0)", "")
	for _, marker := range []string{"ext", "x", ";", ","} {
		if index := strings.Index(value, marker); index > 0 {
			value = strings.TrimSpace(value[:index])
		}
	}
	var digits strings.Builder
	for i, r := range value {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == '+' && i == 0:
		case strings.ContainsRune(" -.()/", r):
		default:
			return "", false
		}
	}
	number := digits.String()
	switch {
	case strings.HasPrefix(value, "+") && strings.HasPrefix(number, "0"):
		return "", false
	case strings.HasPrefix(value, "+"):
	case strings.HasPrefix(number, "00"):
		number = number[2:]
	case countryCode == "":
		return "", false
	case countryCode == "1":
		switch {
		case len(number) == 10:
			number = "1" + number
		case len(number) == 11 && number[0] == '1':
		default:
			return "", false
		}
	default:
		// Most countries drop a national trunk 0 after the country code; Italy
		// keeps it.
		if countryCode != "39" {
			number = strings.TrimPrefix(number, "0")
		}
		number = countryCode + number
	}
	if len(number) < 7 || len(number) > 15 {
		return "", false
	}
	return "+" + number, true
}
