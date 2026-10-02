package config

import (
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
)

// EditInlineAccount conditionally updates one authenticated principal, merges
// explicit chat filters, and enables future sync. An empty filter restores all chats. Unrelated config and comments remain
// intact, and the existing config transaction rejects concurrent edits.
func EditInlineAccount(path, ifMatch string, account InlineAccount) (ConfigFile, error) {
	if err := account.Validate(); err != nil {
		return ConfigFile{}, err
	}
	return editConfigWithMatch(path, ifMatch, nil, true, func(content []byte) ([]byte, error) {
		var current struct {
			Inline InlineConfig `toml:"inline"`
		}
		if _, err := toml.Decode(string(content), &current); err != nil {
			return nil, fmt.Errorf("decode config for Inline account update: %w", err)
		}
		cfg := Config{Inline: current.Inline}
		if err := cfg.validateInlineAccounts(); err != nil {
			return nil, err
		}
		if err := cfg.UpsertInlineAccount(account); err != nil {
			return nil, err
		}
		merged := cfg.GetInlineAccount(account.Identifier)
		content, err := applyTargetedEdits(content, []Edit{{Key: "inline.enabled", Value: true}})
		if err != nil {
			return nil, err
		}
		return replaceInlineAccountLines(content, *merged)
	}, defaultConfigFileOps(), true)
}

func replaceInlineAccountLines(content []byte, account InlineAccount) ([]byte, error) {
	lines := splitTOMLLines(string(content))
	eol := preferredEOL(lines)
	structural := tomlStructuralLines(lines)
	start, end := -1, -1
	for index, line := range lines {
		if !structural[index] {
			continue
		}
		path, array, ok := parseTOMLTable(line.body)
		if !ok || !array || !equalPath(path, []string{"inline", "accounts"}) {
			continue
		}
		blockEnd := len(lines)
		for next := index + 1; next < len(lines); next++ {
			if structural[next] {
				if _, _, table := parseTOMLTable(lines[next].body); table {
					blockEnd = next
					break
				}
			}
		}
		var block struct {
			Inline struct {
				Accounts []InlineAccount `toml:"accounts"`
			} `toml:"inline"`
		}
		if _, err := toml.Decode(string(joinTOMLLines(lines[index:blockEnd])), &block); err != nil {
			return nil, fmt.Errorf("decode Inline account table: %w", err)
		}
		if len(block.Inline.Accounts) != 1 || block.Inline.Accounts[0].Identifier == "" {
			return nil, fmt.Errorf("%w: Inline account table requires one identifier", ErrAmbiguousConfigTarget)
		}
		if block.Inline.Accounts[0].Identifier == account.Identifier {
			if start >= 0 {
				return nil, fmt.Errorf("%w: duplicate Inline principal", ErrAmbiguousConfigTarget)
			}
			start, end = index, blockEnd
		}
	}
	if start < 0 {
		// Refuse an inline-array representation instead of appending a duplicate
		// TOML accounts key that would discard operator-owned content.
		var current struct {
			Inline InlineConfig `toml:"inline"`
		}
		if _, err := toml.Decode(string(content), &current); err != nil {
			return nil, fmt.Errorf("decode config for Inline account insertion: %w", err)
		}
		for _, existing := range current.Inline.Accounts {
			if existing.Identifier == account.Identifier {
				return nil, fmt.Errorf("%w: Inline account must use [[inline.accounts]]", ErrAmbiguousConfigTarget)
			}
		}
		if len(current.Inline.Accounts) > 0 {
			hasArray := false
			for index, line := range lines {
				if !structural[index] {
					continue
				}
				path, array, ok := parseTOMLTable(line.body)
				hasArray = hasArray || ok && array && equalPath(path, []string{"inline", "accounts"})
			}
			if !hasArray {
				return nil, fmt.Errorf("%w: Inline accounts must use [[inline.accounts]]", ErrAmbiguousConfigTarget)
			}
		}
		if len(lines) > 0 {
			if lines[len(lines)-1].eol == "" {
				lines[len(lines)-1].eol = eol
			}
			if strings.TrimSpace(lines[len(lines)-1].body) != "" {
				lines = append(lines, tomlLine{eol: eol})
			}
		}
		start, end = len(lines), len(lines)
	}
	block := append([]tomlLine(nil), lines[start:end]...)
	if len(block) == 0 {
		block = []tomlLine{{body: "[[inline.accounts]]", eol: eol}}
	}
	chatIDs := account.ChatIDs
	if chatIDs == nil {
		chatIDs = []string{}
	}
	for _, assignment := range []providerAssignment{
		{key: "identifier", value: account.Identifier},
		{key: "transport", value: account.EffectiveTransport()},
		{key: "chat_ids", value: chatIDs},
		{key: "endpoint", value: account.Endpoint},
		{key: "cli_path", value: account.CLIPath},
	} {
		var err error
		block, err = editProviderBlockAssignment(block, assignment.key, assignment.value)
		if err != nil {
			return nil, err
		}
	}
	result := append([]tomlLine(nil), lines[:start]...)
	result = append(result, block...)
	result = append(result, lines[end:]...)
	return joinTOMLLines(result), nil
}
