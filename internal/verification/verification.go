// Package verification classifies verification content at read time.
package verification

import (
	"html"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/mime"
)

const Masked = "[verification content masked]"
const MaxFieldBytes = 1024 * 1024

const localizedCodeCues = `bestätigungscode|sicherheitscode|code de vérification|code de sécurité|código de verificación|código de seguridad|codice di verifica|код подтверждения|код безопасности|验证码|驗證碼|認証コード|인증 코드`
const englishCodeCues = `\b(code|passcode|otp|2fa|(?:one[- ]time|temporary|verification|security|login|sign[ -]in|authentication) password|verification number|pin)\b`
const englishQualifiedCodeCues = `\b(?:(?:verification|security|login|sign[ -]in|confirmation|authentication|one[- ]time) (?:code|pin|password|number)|temporary password|passcode|otp|2fa)\b`

type Content struct {
	Subject, Snippet, Title, Text, HTML string
	Filenames                           []string
}

var (
	codeCue                 = regexp.MustCompile(`(?i)` + localizedCodeCues + `|` + englishCodeCues)
	englishCodeCue          = regexp.MustCompile(`(?i)` + englishCodeCues)
	linkCue                 = regexp.MustCompile(`(?i)\b(sign[ -]in|log in|login link|magic link|reset (your )?password|password reset|verif(y|ication)( your email| link)|confirm (your )?(email|account|subscription)|activate your account)\b`)
	qualifiedCodeCue        = regexp.MustCompile(`(?i)` + localizedCodeCues + `|` + englishQualifiedCodeCues)
	englishQualifiedCodeCue = regexp.MustCompile(`(?i)` + englishQualifiedCodeCues)
	credentialCodeCue       = regexp.MustCompile(`(?i)\b(?:(?:verification|security|login|sign[ -]in|authentication|one[- ]time) (?:code|pin|password)|otp|passcode|2fa)\b`)
	ordinaryCodePrefix      = regexp.MustCompile(`(?i)\b(?:promo|coupon|discount|voucher|referral|gift|zip|postal|area|tracking|order|invoice|product|project)\s+$`)

	compactCode = strings.NewReplacer("-", "", " ", "", "\u00a0", "", "\u202f", "", "\u2009", "", "\n", "")
	codeToken   = regexp.MustCompile(`[0-9]+(?:[ \x{00a0}\x{202f}\x{2009}][0-9]+)+|[0-9](?:\n+[0-9]){3,}|[A-Z0-9]+(?:-[A-Z0-9]+)*`)
	codeGap     = regexp.MustCompile(`(?i)^[\s\x{00a0}]*(?:(?:is|est|ist|es|è|:|-|your|the|verification|security|to verify|this|below|[#*_\x60"'“”‘’\[\](){}<>])[\s\x{00a0}]*)*$`)
	introGap    = regexp.MustCompile(`(?i)^[\s\x{00a0}]*(?:\([^()\n]{0,30}\)[\s\x{00a0}]*)?(?:\pL[\pL'’-]*[\s\x{00a0}]*(?:\([^()\n]{0,30}\)[\s\x{00a0}]*)?){0,6}(?:is[\s\x{00a0}]*:?|:|=)[\s\x{00a0}]*$`)
	reverseGap  = regexp.MustCompile(`(?i)^\s+is (?:your |the )?(?:[a-z]+ ){0,3}$`)
	signInAfter = regexp.MustCompile(`(?i)^\s+to (?:sign[ -]in|log in|verify your (?:account|identity|email))\b`)
	useBefore   = regexp.MustCompile(`(?i)\b(?:use|enter|type)\s+$`)
	link        = regexp.MustCompile(`https?://[^\s<>"']+`)
)

// Is fails closed for oversized fields. Cue distance is at most 60 runes.
// A bounded window includes each complete cue, independently of cue count.
func Is(c Content) bool {
	fields := []string{c.Subject, c.Snippet, c.Text, c.HTML, c.Title}
	fields = append(fields, c.Filenames...)
	for _, field := range fields {
		if len(field) >= MaxFieldBytes {
			return true
		}
	}
	// Keep block separators so a code cannot merge with a neighboring number.
	// Single-digit cells remain recognizable through codeToken's whitespace run.
	fields[3] = mime.StripHTML(c.HTML)
	for i, field := range fields {
		fields[i] = strings.Map(func(r rune) rune {
			switch r {
			case '\u200b', '\u200c', '\u200d', '\u2060', '\ufeff', '\u00ad':
				return -1
			}
			if r >= '０' && r <= '９' {
				return '0' + r - '０'
			}
			return r
		}, strings.ReplaceAll(field, "\r\n", "\n"))
	}
	cueFields := make([]bool, len(fields))
	codeFields := make([]bool, len(fields))
	useFields := make([]bool, len(fields))
	localizedFields := make([]bool, len(fields))
	cueCount := 0
	linkContext := false
	for i, field := range fields {
		// Match regexp's Unicode simple folding, including the long s.
		// These literal checks only rule out scans; regexps still decide matches.
		lower := strings.Map(func(r rune) rune {
			if r == 'ſ' {
				return 's'
			}
			return unicode.ToLower(r)
		}, field)
		localizedFields[i] = containsAny(lower, strings.Split(localizedCodeCues, "|")...)
		codeFields[i] = containsAny(lower, "code", "password", "number", "pin", "otp", "2fa") || localizedFields[i]
		useFields[i] = containsAny(lower, "use", "enter", "type")
		qualified := englishQualifiedCodeCue
		if localizedFields[i] {
			qualified = qualifiedCodeCue
		}
		cueFields[i] = (codeFields[i] || strings.Contains(lower, "2fa")) && trailingCue(field, qualified)
		if cueFields[i] {
			cueCount++
		}
		linkContext = linkContext || containsAny(lower, "sign", "log", "magic", "reset", "password", "verif", "confirm", "activate") && linkCue.MatchString(field)
	}
	for i, field := range fields {
		separateCue := cueCount > 1 || cueCount == 1 && !cueFields[i]
		if !separateCue && !codeFields[i] && !useFields[i] {
			continue
		}
		cue := englishCodeCue
		if localizedFields[i] {
			cue = codeCue
		}
		for _, loc := range codeToken.FindAllStringIndex(field, -1) {
			start, end := loc[0], loc[1]
			if start > 0 && asciiAlnum(field[start-1]) || end < len(field) && asciiAlnum(field[end]) {
				continue
			}
			value := compactCode.Replace(field[start:end])
			digits := 0
			for _, r := range value {
				if r >= '0' && r <= '9' {
					digits++
				}
			}
			if len(value) < 4 || len(value) > 10 || digits == 0 || (digits != len(value) && len(value) < 5) {
				continue
			}
			left, right := start, end
			for n := 0; n < 120 && left > 0; n++ {
				_, n := utf8.DecodeLastRuneInString(field[:left])
				left -= n
			}
			for n := 0; n < 120 && right < len(field); n++ {
				_, n := utf8.DecodeRuneInString(field[right:])
				right += n
			}
			// Every code cue contains one of these letters, including localized cues.
			if !separateCue && !useFields[i] && !strings.ContainsAny(field[left:right], "cCpPoOfFкК验驗認인") {
				continue
			}
			if len(value) == 4 && value >= "1900" && value <= "2099" {
				if codeFields[i] && nearCue(field[left:start], true, cue) {
					return true
				}
			} else if digits != len(value) && nearCue(field[left:start], true, credentialCodeCue) || (digits == len(value) || len(value) >= 6 && digits >= 3) && (separateCue || codeFields[i] && (nearCue(field[left:start], true, cue) || nearCue(field[end:right], false, cue)) || useFields[i] && useBefore.MatchString(field[left:start]) && signInAfter.MatchString(field[end:right])) {
				return true
			}
		}
	}
	// Raw HTML retains href targets; unescaping exposes query separators.
	fields = append(fields, c.HTML)
	for _, field := range fields {
		field = html.UnescapeString(field)
		for _, loc := range link.FindAllStringIndex(field, -1) {
			if credentialLink(strings.TrimRight(field[loc[0]:loc[1]], ").,;:!?]}”’"), linkContext) {
				return true
			}
		}
	}
	return false
}

func containsAny(s string, words ...string) bool {
	for _, word := range words {
		if strings.Contains(s, word) {
			return true
		}
	}
	return false
}

func nearCue(window string, before bool, cue *regexp.Regexp) bool {
	for _, loc := range cue.FindAllStringIndex(window, -1) {
		if strings.EqualFold(window[loc[0]:loc[1]], "code") && ordinaryCodePrefix.MatchString(window[:loc[0]]) {
			continue
		}
		gap, breaks := window[:loc[0]], 0
		if before {
			gap = strings.Replace(window[loc[1]:], ".\n", "\n", 1) // the cue line may end a sentence
			breaks = 4
		}
		if utf8.RuneCountInString(gap) <= 60 && strings.Count(gap, "\n") <= breaks && !strings.ContainsAny(gap, ".!?") && (codeGap.MatchString(gap) || before && introGap.MatchString(gap) || !before && reverseGap.MatchString(gap)) {
			return true
		}
	}
	return false
}

// Only qualified cues can describe a code in a separate field.
func trailingCue(field string, cue *regexp.Regexp) bool {
	for _, loc := range cue.FindAllStringIndex(field, -1) {
		if codeGap.MatchString(field[loc[1]:]) {
			return true
		}
	}
	return false
}

func asciiAlnum(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func tokenValue(value string) bool {
	letters, digits := 0, 0
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
			letters++
		case r >= '0' && r <= '9':
			digits++
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return len(value) >= 16 && letters > 0 && digits > 0
}

func credentialLink(raw string, cue bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	excluded := false
	for _, word := range []string{"unsubscribe", "preferences"} {
		excluded = excluded || strings.Contains(strings.ToLower(u.Path), word)
	}
	hasToken, credentialKey := false, false
	values := u.Query()
	if fragment, err := url.ParseQuery(u.Fragment); err == nil {
		for key, vals := range fragment {
			values[key] = append(values[key], vals...)
		}
	}
	for key, vals := range values {
		for _, value := range vals {
			// Fragment values can include an escaped prefix before the token.
			for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == '&' || r == '=' }) {
				if tokenValue(part) {
					hasToken = true
					key = strings.ToLower(key)
					credentialKey = credentialKey || strings.Contains(key, "token")
					switch key {
					case "otp", "code", "oobcode", "magic", "reset", "verify":
						credentialKey = true
					}
				}
			}
		}
	}
	for part := range strings.SplitSeq(u.Path, "/") {
		hasToken = hasToken || tokenValue(part)
	}
	hasToken = hasToken || tokenValue(u.Fragment)
	return hasToken && (cue || credentialKey) && !excluded
}
