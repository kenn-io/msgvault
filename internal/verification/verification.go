// Package verification classifies verification content at read time.
package verification

import (
	"html"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/mime"
)

const Masked = "[verification content masked]"
const MaxFieldBytes = 1024 * 1024

type Content struct {
	Subject, Snippet, Title, Text, HTML string
	Filenames                           []string
}

var (
	codeCue = regexp.MustCompile(`(?i)bestätigungscode|sicherheitscode|code de vérification|code de sécurité|código de verificación|código de seguridad|codice di verifica|код подтверждения|код безопасности|验证码|驗證碼|認証コード|인증 코드|\b(code|passcode|otp|(?:one[- ]time|temporary) password|verification number|pin)\b`)
	linkCue = regexp.MustCompile(`(?i)\b(sign[ -]in|log in|login link|magic link|reset (your )?password|password reset|verif(y|ication)( your email| link)|confirm (your )?(email|account|subscription)|activate your account)\b`)

	compactCode    = strings.NewReplacer("-", "", " ", "", "\u00a0", "")
	codeToken      = regexp.MustCompile(`[0-9]+(?:[ \x{00a0}][0-9]+)+|[A-Z0-9]+(?:-[A-Z0-9]+)*`)
	directYearCode = regexp.MustCompile(`(?:` + codeCue.String() + `)\s*(?:is|:|-)?\s*[0-9]{4}$`)
	codeGap        = regexp.MustCompile(`(?i)^[\s\x{00a0}]*(?:(?:is|:|-|your|the|verification|security)[\s\x{00a0}]*)*$`)
	reverseGap     = regexp.MustCompile(`(?i)^\s+is (?:your |the )?(?:[a-z]+ ){0,3}$`)
	signInAfter    = regexp.MustCompile(`(?i)^\s+to (?:sign[ -]in|log in|verify your account)\b`)
	useBefore      = regexp.MustCompile(`(?i)\buse\s+$`)
	link           = regexp.MustCompile(`https?://[^\s<>"']+`)
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
	fields[3] = strings.Join(strings.Fields(mime.StripHTML(c.HTML)), " ")
	for i, field := range fields {
		fields[i] = strings.Map(func(r rune) rune {
			if r >= '０' && r <= '９' {
				return '0' + r - '０'
			}
			return r
		}, field)
	}
	cueFields := make([]bool, len(fields))
	cueCount := 0
	for i, field := range fields {
		cueFields[i] = trailingCue(field)
		if cueFields[i] {
			cueCount++
		}
	}
	linkContext := false
	for _, field := range fields {
		linkContext = linkContext || linkCue.MatchString(field)
	}
	for i, field := range fields {
		separateCue := cueCount > 1 || cueCount == 1 && !cueFields[i]
		for _, loc := range codeToken.FindAllStringIndex(field, -1) {
			start, end := loc[0], loc[1]
			if start > 0 && asciiAlnum(field[start-1]) || end < len(field) && asciiAlnum(field[end]) {
				continue
			}
			value := compactCode.Replace(field[start:end])
			if len(value) > 1 && strings.HasPrefix(field[start:end], value[:1]+"-") {
				value = value[1:]
			}
			digits := 0
			for _, r := range value {
				if r >= '0' && r <= '9' {
					digits++
				}
			}
			if len(value) < 4 || len(value) > 10 || (digits != len(value) && (len(value) < 6 || digits < 3)) {
				continue
			}
			left, right := start, end
			for range 120 {
				if left == 0 {
					break
				}
				_, n := utf8.DecodeLastRuneInString(field[:left])
				left -= n
			}
			for range 120 {
				if right == len(field) {
					break
				}
				_, n := utf8.DecodeRuneInString(field[right:])
				right += n
			}
			if len(value) == 4 && value >= "1900" && value <= "2099" {
				if directYearCode.MatchString(field[left:end]) {
					return true
				}
			} else if separateCue || nearCue(field[left:start], true) || nearCue(field[end:right], false) || useBefore.MatchString(field[left:start]) && signInAfter.MatchString(field[end:right]) {
				return true
			}
		}
	}
	// Raw HTML retains href targets; unescaping exposes query separators.
	fields = append(fields, c.HTML)
	for _, field := range fields {
		field = html.UnescapeString(field)
		for _, loc := range link.FindAllStringIndex(field, -1) {
			if credentialLink(field[loc[0]:loc[1]], linkContext) {
				return true
			}
		}
	}
	return false
}

func nearCue(window string, before bool) bool {
	for _, loc := range codeCue.FindAllStringIndex(window, -1) {
		gap, breaks := window[:loc[0]], 0
		if before {
			gap = strings.ReplaceAll(window[loc[1]:], "\r\n", "\n")
			gap = strings.Replace(gap, ".\n", "\n", 1) // the cue line may end a sentence
			breaks = 4
		}
		if utf8.RuneCountInString(gap) <= 60 && strings.Count(gap, "\n") <= breaks && !strings.ContainsAny(gap, ".!?") && (codeGap.MatchString(gap) || !before && reverseGap.MatchString(gap)) {
			return true
		}
	}
	return false
}

// A cue in a separate metadata field can describe a code-only representation.
func trailingCue(field string) bool {
	for _, loc := range codeCue.FindAllStringIndex(field, -1) {
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
	for _, word := range []string{"unsubscribe", "preferences", "track", "click"} {
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
					switch strings.ToLower(key) {
					case "token", "otp", "code", "oobcode", "magic", "reset", "verify":
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
