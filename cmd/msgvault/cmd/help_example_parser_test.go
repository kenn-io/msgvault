package cmd

import (
	"errors"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Lines may be ordinary help examples or commands inside shell fences.
// We only extract an actual executable token, never inline prose references.
func advertisedCommandLines(source string, prose ...bool) []string {
	var result []string
	lines := strings.Split(source, "\n")
	fenced := false
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if len(prose) > 0 && prose[0] && !fenced && strings.TrimLeft(lines[i], " \t") == lines[i] {
			continue
		}
		for strings.HasSuffix(line, "\\") && i+1 < len(lines) {
			i++
			line = strings.TrimSuffix(line, "\\") + " " + strings.TrimSpace(lines[i])
		}
		// A pipeline may supply a secret via stdin; inspect its msgvault segment.
		if !strings.HasPrefix(line, "msgvault ") {
			if index := strings.Index(line, "| msgvault "); index >= 0 {
				line = strings.TrimSpace(line[index+2:])
			} else {
				continue
			}
		}
		result = append(result, line)
	}
	return result
}

// Tokenize the documented shell notation without executing shell expansion.
// Angle-bracket placeholders are literal argv words; quoted operators stay data.
func splitAdvertisedWords(line string) ([]string, error) {
	var words []string
	var word strings.Builder
	var quote rune
	started, escape := false, false
	flush := func() {
		if started {
			words = append(words, word.String())
			word.Reset()
			started = false
		}
	}
	for index, r := range line {
		if escape {
			word.WriteRune(r)
			started = true
			escape = false
			continue
		}
		if quote == '\'' {
			if r == '\'' {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		if r == '\\' {
			escape = true
			continue
		}
		if quote == '"' {
			if r == '"' {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			started = true
			continue
		}
		if unicode.IsSpace(r) {
			flush()
			continue
		}
		if r == '<' && !started {
			tail := line[index+1:]
			end := strings.IndexByte(tail, '>')
			if end < 0 || strings.ContainsAny(tail[:end], " \t") {
				flush()
				break
			}
		}
		if (r == '#' && !started) || r == '|' || r == '>' && !strings.HasPrefix(word.String(), "<") {
			if r == '>' && word.Len() > 0 && strings.Trim(word.String(), "0123456789") == "" {
				word.Reset()
				started = false
			}
			flush()
			break
		}
		word.WriteRune(r)
		started = true
	}
	if quote != 0 || escape {
		return nil, errors.New("unfinished shell word")
	}
	flush()
	for _, w := range words {
		if strings.Contains(w, "$(") {
			return nil, errors.New("use literal example values instead of shell substitution")
		}
	}
	return words, nil
}

func TestAdvertisedShellNotation(t *testing.T) {
	assert := assert.New(t)
	for _, tc := range []struct {
		line string
		want []string
	}{
		{`msgvault search 'from:alice@example.com newer_than:30d' --json # notes`, []string{"msgvault", "search", "from:alice@example.com newer_than:30d", "--json"}},
		{`msgvault show <id> --json > output.json`, []string{"msgvault", "show", "<id>", "--json"}},
		{`msgvault search "a # and > and |"`, []string{"msgvault", "search", "a # and > and |"}},
	} {
		got, err := splitAdvertisedWords(tc.line)
		require.NoError(t, err)
		assert.Equal(tc.want, got)
	}
	assert.Equal([]string{"msgvault credentials set example-key"}, advertisedCommandLines(`printf '%s' "$KEY" | msgvault credentials set example-key`))
	assert.Equal([]string{"msgvault search words  --json"}, advertisedCommandLines("msgvault search words \\\n --json"))
}

func TestAdvertisedExtractionIgnoresProse(t *testing.T) {
	source := "msgvault archive is described here.\n  msgvault show 1\n```sh\nmsgvault search words\n```"
	assert.Equal(t, []string{"msgvault show 1", "msgvault search words"}, advertisedCommandLines(source, true))
	words, err := splitAdvertisedWords("msgvault source muesli hook < completion-event.json")
	require.NoError(t, err)
	assert.Equal(t, []string{"msgvault", "source", "muesli", "hook"}, words)
}
