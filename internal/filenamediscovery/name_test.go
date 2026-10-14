package filenamediscovery

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilenameQueryLimits(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{"", false}, {"... / \\", false}, {strings.Repeat("a", 64), true}, {strings.Repeat("a", 65), false},
		{strings.Repeat("ß", 32), true}, {strings.Repeat("ß", 33), false},
		{strings.Repeat("a", 63) + " " + strings.Repeat("b", 64), true},
		{strings.Repeat("a", 64) + " " + strings.Repeat("b", 64), false},
		{"a" + strings.Repeat(" ", 126) + "b", true}, {"a" + strings.Repeat(" ", 127) + "b", false},
		{strings.Repeat("a ", 7) + "a", true}, {strings.Repeat("a ", 8) + "a", false},
	} {
		_, err := ParseQuery(tc.raw)
		if tc.valid {
			require.NoError(err)
		} else {
			require.Error(err)
		}
	}
	q, err := ParseQuery("b a b")
	require.NoError(err)
	assert.Equal([]string{"b", "a", "b"}, q.Tokens)
}

func TestFilenameUnicodePolicy(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	for _, tc := range []struct {
		raw, normalized string
		tokens          []string
	}{
		{"CAFÉ.PDF", "café.pdf", []string{"café", "pdf"}},
		{"CAFE\u0301.PDF", "café.pdf", []string{"café", "pdf"}},
		{"q\u0301.pdf", "q\u0301.pdf", []string{"q\u0301", "pdf"}},
		{"\u0301q.pdf", "\u0301q.pdf", []string{"q", "pdf"}},
		{"किताब.pdf", "किताब.pdf", []string{"किताब", "pdf"}},
		{"Straße.PDF", "strasse.pdf", []string{"strasse", "pdf"}},
		{"folder\\Report/Final😀PDF", "folder\\report/final😀pdf", []string{"folder", "report", "final", "pdf"}},
	} {
		n, ok := ProjectName(tc.raw)
		require.True(ok)
		assert.Equal(tc.normalized, n.Normalized)
		assert.Equal(tc.tokens, n.Tokens)
	}
	q, err := ParseQuery("q.pdf")
	require.NoError(err)
	n, ok := ProjectName("q\u0301.pdf")
	require.True(ok)
	rank, matched := Match(q, n)
	assert.True(matched)
	assert.Equal(Prefix, rank)
	q, err = ParseQuery("café")
	require.NoError(err)
	n, ok = ProjectName("cafe\u0301.pdf")
	require.True(ok)
	rank, matched = Match(q, n)
	assert.True(matched)
	assert.Equal(TokensExact, rank)
}

func TestFilenameMatchRanks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	for _, tc := range []struct {
		query, name string
		rank        Rank
		valid       bool
	}{
		{"report.pdf", "REPORT.PDF", FullExact, true}, {"report pdf", "report.pdf", TokensExact, true},
		{"report", "reports.pdf", Prefix, true}, {"reprot", "report.pdf", Typo, true},
		{"xeport", "report.pdf", Typo, true}, {"repor", "report.pdf", Prefix, true},
		{"repot", "report.pdf", Typo, true}, {"reportx", "report.pdf", Typo, true},
		{"abcd", "abce.pdf", 0, false}, {"alpha beta", "alhpa beat.pdf", 0, false},
		{"alpha gamma", "alhpa gamam.pdf", 0, false},
		{"alpha beta", "alhpa beta.pdf", Typo, true}, {"alpha beta", "alpha betamax.pdf", Prefix, true},
		{"alpha alpha", "alpha.pdf", TokensExact, true}, {"bcde", "abcde.pdf", 0, false},
		{"alpha", "axhpa.pdf", 0, false},
	} {
		q, err := ParseQuery(tc.query)
		require.NoError(err)
		n, ok := ProjectName(tc.name)
		require.True(ok)
		rank, matched := Match(q, n)
		assert.Equal(tc.valid, matched, "%q/%q", tc.query, tc.name)
		if matched {
			assert.Equal(tc.rank, rank)
		}
	}
	q, err := ParseQuery("report")
	require.NoError(err)
	var ranks []Rank
	for _, raw := range []string{"report", "report.pdf", "reports.pdf", "reprot.pdf"} {
		n, ok := ProjectName(raw)
		require.True(ok)
		r, matched := Match(q, n)
		require.True(matched)
		ranks = append(ranks, r)
	}
	assert.Equal([]Rank{FullExact, TokensExact, Prefix, Typo}, ranks)
}

func TestFilenameNumberProtection(t *testing.T) {
	for _, tc := range []struct {
		query, name string
		valid       bool
	}{
		{"2026", "2026.pdf", true}, {"2026", "20260.pdf", false},
		{"invoice2", "invoice20.pdf", true}, {"invoice2", "invocie2.pdf", true},
		{"invoice2", "invoice3.pdf", false}, {"invoice٢", "invoice٣.pdf", false},
		{"invoice²", "invoice³.pdf", false}, {"invoice①", "invoice②.pdf", false},
		{"invoice2", "invoice.pdf", false}, {"invoice2a", "invoicea2.pdf", false},
		{"٢٣٤٥٦", "٢٣٤٥٦٧.pdf", false}, {"invoice٢", "invocie٢.pdf", true},
		{"2026a", "2026.pdf", false}, {"a2026", "2026.pdf", false}, {"2026a", "2026ab.pdf", true},
	} {
		q, err := ParseQuery(tc.query)
		require.NoError(t, err)
		n, ok := ProjectName(tc.name)
		require.True(t, ok)
		_, matched := Match(q, n)
		assert.Equal(t, tc.valid, matched, "%q/%q", tc.query, tc.name)
	}
}

func TestFilenameProjectionLimits(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{strings.Repeat("界", 64), true}, {strings.Repeat("界", 65), false},
		{"a" + strings.Repeat(" ", 510) + "b", true}, {"a" + strings.Repeat(" ", 511) + "b", false},
		{strings.Repeat("a ", 63) + "a", true}, {strings.Repeat("a ", 64) + "a", false},
		{"---", true}, {"", true},
	} {
		_, ok := ProjectName(tc.raw)
		assert.Equal(t, tc.valid, ok)
	}
	n, ok := ProjectName("Beta.alpha.BETA")
	require.True(t, ok)
	assert.Equal(t, []string{"beta", "alpha"}, n.Tokens)
}

func TestFilenameDeletionCandidates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	assert.Equal([]string{"aaa", "aab"}, DeleteKeys("aaab"))
	assert.Equal([]string{"ab", "a界", "界b"}, DeleteKeys("a界b"))
	assert.Equal([]string{""}, DeleteKeys("界"))
	assert.Empty(DeleteKeys(""))
	assert.Empty(DeleteKeys("123456789012"))
	assert.Empty(DeleteKeys("١٢٣٤٥"))
	assert.Equal([]string{"abc2", "abd2", "acd2", "bcd2"}, DeleteKeys("abcd2"))
	assert.Equal([]string{"ab٢c", "ab٢d", "a٢cd", "b٢cd"}, DeleteKeys("ab٢cd"))
	q, err := ParseQuery("abcde")
	require.NoError(err)
	n, ok := ProjectName("abdec.pdf")
	require.True(ok)
	assert.Contains(DeleteKeys(q.Tokens[0]), "abde")
	assert.Contains(DeleteKeys(n.Tokens[0]), "abde")
	_, matched := Match(q, n)
	assert.False(matched, "deletion-key collision is only a candidate")
}
