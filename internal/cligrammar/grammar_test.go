package cligrammar

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGrammarKnownPaths(t *testing.T) {
	for _, tc := range []struct{ old, canonical string }{
		{"draft-reply", "draft reply"},
		{"add-account", "source gmail add"},
		{"add-slack", "source slack add"},
		{"list-folders", "source imap folders"},
		{"add-synctech-sms-drive", "source synctech-sms add"},
		{"migrate", "database migrate"},
		{"delete-deduped", "deletion purge-deduped"},
		{"show-message", "show"},
		{"schema-version", "database version"},
		{"sync-full", "sync full"},
	} {
		t.Run(tc.old, func(t *testing.T) {
			assert := assert.New(t)
			suffix := []string{"--json=false", "--", "draft-reply", "source slack add"}
			old := append(strings.Fields(tc.old), suffix...)
			canonical := append(strings.Fields(tc.canonical), suffix...)
			assert.Equal(canonical, Canonical(old))
			assert.Equal(old, Legacy(canonical))
			assert.Equal(append(strings.Fields(tc.old), suffix...), old)
			assert.Equal(append(strings.Fields(tc.canonical), suffix...), canonical)
		})
	}
}

func TestGrammarLeavesDataAndUnknownPathsAlone(t *testing.T) {
	assert := assert.New(t)
	for _, args := range [][]string{
		nil, {}, {"unknown", "draft-reply"}, {"--", "draft-reply"},
		{"draft", "--json", "reply"}, {"source", "slack", "unknown"},
		{"search", "draft-reply"}, {"draft-replying"},
		{"import", "--type=whatsapp", "whatsapp"},
		{"embeddings", "build", "--yes"},
	} {
		assert.Equal(args, Canonical(args))
		assert.Equal(args, Legacy(args))
	}
	assert.Equal([]string{"embeddings", "build", "--yes"}, Canonical([]string{"build-embeddings", "--yes"}))
}

func TestGrammarMovesAreUnambiguous(t *testing.T) {
	assert := assert.New(t)
	old, canonical := map[string]bool{}, map[string]bool{}
	for _, move := range Moves() {
		assert.False(old[move.Legacy], "duplicate old path: %s", move.Legacy)
		assert.False(canonical[move.Canonical], "duplicate canonical path: %s", move.Canonical)
		old[move.Legacy], canonical[move.Canonical] = true, true
	}
	assert.NotEmpty(old)
}

func FuzzGrammarPaths(f *testing.F) {
	f.Add(uint8(0), "--json=false")
	f.Add(uint8(42), "draft-reply")
	f.Add(uint8(255), "source\x00slack add")
	f.Fuzz(func(t *testing.T, index uint8, value string) {
		assert := assert.New(t)
		moves := Moves()
		move := moves[int(index)%len(moves)]
		old := []string{move.Legacy, "--", value}
		before := slices.Clone(old)
		canonical := Canonical(old)
		assert.Equal([]string{"--", value}, canonical[len(strings.Fields(move.Canonical)):])
		assert.Equal(before, old)
		assert.Equal(old, Legacy(canonical))
		assert.Equal(canonical, Canonical(canonical))
		assert.Equal(old, Legacy(Legacy(canonical)))
		unknown := []string{"unknown", value, move.Legacy}
		assert.Equal(unknown, Canonical(unknown))
		assert.Equal(unknown, Legacy(unknown))
	})
}
