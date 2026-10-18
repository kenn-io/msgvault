import { describe, expect, it } from 'vitest';

import legacyPipes from '../../../../testdata/savedview_legacy_pipes.json';
import {
  defaultExploreURLState,
  ExploreState,
  parseExploreURLState,
  serializeExploreURLState
} from '../explore/state.svelte';
import type { ExploreURLState } from '../explore/models';
import { canonicalSavedViewState, exploreStateFromSavedView, savedViewSummary, type CanonicalState } from './canonical';

const everything: ExploreURLState = {
  ...defaultExploreURLState,
  workspace: 'everything',
  query: ' invoice ',
  searchMode: 'full_text',
  filters: [{ dimension: 'source', values: ['7'] }],
  groupingChain: ['year'],
  presentation: 'table',
  sort: [{ field: 'occurred_at', direction: 'desc' }],
  columns: ['kind', 'title'],
  activeRow: 'message:9',
  selectedRow: 'message:9'
};

function reopened(state: ExploreURLState): ExploreURLState {
  const opened = exploreStateFromSavedView(canonicalSavedViewState(state));
  return parseExploreURLState(serializeExploreURLState({ ...defaultExploreURLState, ...opened }));
}

function openedAndReloaded(saved: CanonicalState) {
  window.history.replaceState(null, '', '/');
  const state = new ExploreState(window);
  try {
    state.replaceCommittedNavigation(exploreStateFromSavedView(saved));
    return {
      opened: state.current.filters,
      reloaded: parseExploreURLState(window.location.search).filters
    };
  } finally {
    state.destroy();
  }
}

describe('saved-view canonical state', () => {
  it.each([
    ['empty direction', '1', '1', ''],
    ['equivalent source spellings', '+1', '01', 'any'],
    ['equivalent identity spelling', '1', '+1', 'sender'],
    ['largest safe integer', '9007199254740991', '+09007199254740991', 'sender'],
    ['first unsafe integer', '+09007199254740992', '9007199254740992', 'any'],
    ['integer above 2^53', '9007199254740993', '+09007199254740993', 'recipient'],
    ['maximum int64', '9223372036854775807', '9223372036854775807', 'recipient']
  ])('preserves %s on open and URL-only reload', (_name, source, identity, direction) => {
    const saved: CanonicalState = { filters: [
      { field: 'source', operator: 'in', values: [source] },
      { field: 'identity', operator: 'in', values: [identity, 'person@example.com', direction] }
    ] };
    const original = structuredClone(saved);
    const expected = [
      { dimension: 'source', values: [source] },
      { dimension: 'identity', values: [identity, 'person@example.com', direction] }
    ];
    expect(openedAndReloaded(saved)).toEqual({ opened: expected, reloaded: expected });
    expect(saved).toEqual(original);
  });

  it('preserves equivalent decimal spellings across generated positive int64 IDs', () => {
    // Fixed seed keeps failures reproducible while exercising the full int64 range.
    let seed = 7n;
    for (let index = 0; index < 64; index++) {
      seed = BigInt.asUintN(63, seed * 6364136223846793005n + 1442695040888963407n);
      const id = (seed || 1n).toString();
      for (const direction of ['', 'any', 'sender', 'recipient']) {
        const source = `${index % 2 ? '+' : ''}${'0'.repeat(index % 4)}${id}`;
        const identity = `${index % 2 ? '' : '+'}${'0'.repeat((index + 1) % 4)}${id}`;
        const identifier = ['person@example.com', ' spaced identity ', '人+%&#\n@example.com'][index % 3]!;
        const saved: CanonicalState = { filters: [
          { field: 'message_type', operator: 'in', values: ['email'] },
          { field: 'source', operator: 'in', values: [source] },
          { field: 'identity', operator: 'in', values: [identity, identifier, direction] }
        ] };
        const expected = [
          { dimension: 'message_type', values: ['email'] },
          { dimension: 'source', values: [source] },
          { dimension: 'identity', values: [identity, identifier, direction] }
        ];
        const restored = openedAndReloaded(saved);
        expect(restored).toEqual({ opened: expected, reloaded: expected });
        expect(reopened({ ...defaultExploreURLState, filters: restored.reloaded }).filters).toEqual(expected);
      }
    }
  });

  it.each([
    '', ' 1', '1 ', '1\n', '1\r', '1\u2028', '1\u2029',
    '1e0', '0x1', '0b1', '0o1', '1.0', '++1', '1x', '１', '١'
  ])('drops only identity for malformed ID %j in either position', (invalid) => {
    for (const [source, identity] of [[invalid, '1'], ['1', invalid]]) {
      const saved: CanonicalState = { filters: [
        { field: 'source', operator: 'in', values: [source] },
        { field: 'identity', operator: 'in', values: [identity, 'person@example.com', 'any'] },
        { field: 'message_type', operator: 'in', values: ['email'] }
      ] };
      const expected = [
        { dimension: 'source', values: [source] },
        { dimension: 'message_type', values: ['email'] }
      ];
      expect(openedAndReloaded(saved)).toEqual({ opened: expected, reloaded: expected });
    }
  });

  it.each(['0', '+0', '-1', '9223372036854775808'])(
    'drops identity when both matching IDs are outside positive int64: %s',
    (invalid) => {
      const saved: CanonicalState = { filters: [
        { field: 'source', operator: 'in', values: [invalid] },
        { field: 'identity', operator: 'in', values: [invalid, 'person@example.com', 'any'] },
        { field: 'message_type', operator: 'in', values: ['email'] }
      ] };
      const expected = [
        { dimension: 'source', values: [invalid] },
        { dimension: 'message_type', values: ['email'] }
      ];
      expect(openedAndReloaded(saved)).toEqual({ opened: expected, reloaded: expected });
    }
  );

  it.each([
    { name: 'missing source', sources: [], identities: [['1', 'person@example.com', 'any']] },
    { name: 'duplicate sources', sources: [['1'], ['01']], identities: [['1', 'person@example.com', 'any']] },
    { name: 'multiple source values', sources: [['1', '2']], identities: [['1', 'person@example.com', 'any']] },
    { name: 'empty source values', sources: [[]], identities: [['1', 'person@example.com', 'any']] },
    { name: 'duplicate identities', sources: [['1']], identities: [['1', 'person@example.com', 'any'], ['1', 'other@example.com', 'sender']] },
    { name: 'short identity tuple', sources: [['1']], identities: [['1', 'person@example.com']] },
    { name: 'long identity tuple', sources: [['1']], identities: [['1', 'person@example.com', 'any', 'extra']] },
    { name: 'empty identifier', sources: [['1']], identities: [['1', '', 'any']] },
    { name: 'invalid direction', sources: [['1']], identities: [['1', 'person@example.com', 'sideways']] },
    { name: 'unequal adjacent IDs above 2^53', sources: [['9007199254740992']], identities: [['9007199254740993', 'person@example.com', 'any']] }
  ])('drops only identity for $name', ({ sources, identities }) => {
    const saved: CanonicalState = { filters: [
      ...sources.map((values) => ({ field: 'source' as const, operator: 'in' as const, values })),
      ...identities.map((values) => ({ field: 'identity' as const, operator: 'in' as const, values })),
      { field: 'message_type', operator: 'in', values: ['email'] }
    ] };
    const expected = [
      ...sources.map((values) => ({ dimension: 'source', values })),
      { dimension: 'message_type', values: ['email'] }
    ];
    expect(openedAndReloaded(saved)).toEqual({ opened: expected, reloaded: expected });
  });

  it.each(legacyPipes)('opens schema $schema_version $search_mode query $query', ({ schema_version, search_mode, query, want }) => {
    expect(exploreStateFromSavedView({ query, search_mode: search_mode as ExploreURLState['searchMode'] | undefined }, schema_version).query).toBe(want);
  });

  it('round-trips an Everything view', () => {
    expect(canonicalSavedViewState(everything)).toEqual({
      query: 'invoice', search_mode: 'full_text',
      filters: [{ field: 'source', operator: 'in', values: ['7'] }],
      grouping: ['year'], presentation: 'table',
      sort: [{ field: 'occurred_at', direction: 'desc' }],
      columns: ['kind', 'title']
    });
    const restored = reopened(everything);
    expect(restored.workspace).toBe('everything');
    expect(restored.query).toBe('invoice');
    expect(restored.filters).toEqual(everything.filters);
    expect(restored.groupingChain).toEqual(['year']);
    expect(restored.sort).toEqual(everything.sort);
    expect(restored.activeRow).toBeNull();
  });

  it('reopens a Files view in Files', () => {
    const files = parseExploreURLState(serializeExploreURLState({
      ...everything, workspace: 'files', groupingChain: []
    }));
    expect(canonicalSavedViewState(files).presentation).toBe('files');
    const opened = exploreStateFromSavedView(canonicalSavedViewState(files));
    expect(opened.workspace).toBe('files');
    expect(reopened(files).workspace).toBe('files');
  });

  it.each(['files', 'table'] as const)(
    'opens a %s view with the default filename, type, and file sort',
    (presentation) => {
      expect(exploreStateFromSavedView({ presentation })).toMatchObject({
        fileFilenameQuery: '', fileMIMEFamilies: [],
        fileSort: { field: 'occurred_at', direction: 'desc' }
      });
    }
  );

  it('opens a filter-only view without a stored query in full text', () => {
    expect(canonicalSavedViewState({ ...everything, query: ' ', searchMode: 'semantic' }))
      .not.toHaveProperty('query');
    expect(exploreStateFromSavedView({ presentation: 'timeline' })).toMatchObject({
      workspace: 'everything', query: '', searchMode: 'full_text', filters: [], groupingChain: [],
      presentation: 'timeline', sort: [{ field: 'occurred_at', direction: 'desc' }]
    });
  });

  it('maps the legacy source_id and participant_id fields', () => {
    const opened = exploreStateFromSavedView({
      filters: [
        { field: 'source_id', operator: 'eq', values: ['1'] },
        { field: 'participant_id', operator: 'in', values: ['42'] }
      ]
    });
    expect(opened.filters).toEqual([
      { dimension: 'source', values: ['1'] },
      { dimension: 'participant', values: ['42'] }
    ]);
  });

  it('summarizes the query, filters, grouping, and layout in words', () => {
    expect(savedViewSummary(canonicalSavedViewState(everything))).toEqual([
      'Full text: “invoice”', 'Source: 7', 'Grouped by Year', 'Table'
    ]);
    expect(savedViewSummary({
      query: 'budget', search_mode: 'hybrid',
      filters: [{ field: 'participant_id', operator: 'in', values: ['42', '43'] }],
      grouping: ['source', 'month'], presentation: 'files'
    })).toEqual(['Hybrid: “budget”', 'Person: 42, 43', 'Grouped by Source, then Month', 'Files']);
    expect(savedViewSummary({})).toEqual(['Table']);
  });
});
