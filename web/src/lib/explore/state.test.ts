import { describe, expect, it } from 'vitest';

import type {
  ExplorePredicate,
  ExploreSelection,
  ExploreURLState,
  SearchProvenance
} from './models';
import {
  ExploreSelectionState,
  ExploreState,
  defaultExploreURLState,
  parseExploreURLState,
  serializeExploreURLState
} from './state.svelte';
import { createAllMatchingSelection, predicateFingerprint } from './selection';
import { SEARCH_MODE_PREFERENCE_KEY } from '../search/modes';

describe('Explore URL state', () => {
  it('round-trips every primary management workspace through the URL', () => {
    for (const workspace of ['saved_views', 'sources', 'deletions'] as const) {
      const parsed = parseExploreURLState(serializeExploreURLState({
        ...defaultExploreURLState,
        workspace
      }));
      expect(parsed.workspace).toBe(workspace);
    }
  });

  it('round-trips every durable field in the versioned envelope', () => {
    const state: ExploreURLState = {
      schemaVersion: 2,
      workspace: 'files',
      query: 'from:alice quarterly plan',
      searchMode: 'hybrid',
      filters: [
        { dimension: 'message_type', values: ['email'] },
        { dimension: 'after', values: ['2025-01-01'] }
      ],
      groupingChain: ['participant', 'year'],
      presentation: 'table',
      sort: [{ field: 'occurred_at', direction: 'desc' }],
      fileSort: { field: 'filename', direction: 'asc' },
      fileFilenameQuery: 'invoice',
      fileMIMEFamilies: ['pdf', 'image'],
	  identityQuery: 'Shared Name',
	  identitySort: { field: 'display_label', direction: 'asc' },
	  analysisTarget: 'person:42',
	  selectedIdentifier: 'email:alice@example.com',
      relationshipFacet: 'domains',
      relationshipTarget: 'domain:example.com',
      relationshipShowAll: true,
      relationshipFiles: true,
      columns: ['kind', 'people', 'title', 'excerpt', 'time', 'attachments', 'size'],
      columnWidths: { people: 240, title: 360 },
      selectedRow: 'message:42',
      inspectorPinned: true,
      inspectorWidth: 456,
      conversationAnchor: 'message:37',
      scrollAnchor: { key: 'message:31', offset: 12 },
      activeRow: 'message:33'
    };

    expect(parseExploreURLState(serializeExploreURLState(state))).toEqual(state);
  });

  it('normalizes legacy unpinned inspector states to pinned', () => {
    const parsed = parseExploreURLState(serializeExploreURLState({
      ...defaultExploreURLState,
      inspectorPinned: false
    }));
    expect(parsed.inspectorPinned).toBe(true);
  });

  it('restores legacy People and Domains targets as relationships without treating display labels as identity', () => {
    for (const [workspace, target] of [['people', 'person:42'], ['domains', 'domain:example.com']] as const) {
      const restored = parseExploreURLState(serializeExploreURLState({
        ...defaultExploreURLState,
        workspace,
        identityQuery: 'Shared Name',
        analysisTarget: target,
        selectedIdentifier: workspace === 'people' ? 'phone:+15550100001' : null
      } as unknown as ExploreURLState));

      expect(restored.workspace).toBe('relationships');
      expect(restored.relationshipFacet).toBe(workspace);
      expect(restored.relationshipTarget).toBe(workspace === 'people' ? 'cluster:42' : target);
      expect(restored.identityQuery).toBe('Shared Name');
      expect(restored.analysisTarget).toBe(target);
      expect(restored.selectedIdentifier).toBe(workspace === 'people' ? 'phone:+15550100001' : null);
    }
  });

  it('migrates supported version-one URLs before Files controls are edited', () => {
    const legacy = new URLSearchParams({
      explore: JSON.stringify({ ...defaultExploreURLState, schemaVersion: 1, workspace: 'files' })
    });
    const restored = parseExploreURLState(`?${legacy}`);
    expect(restored.schemaVersion).toBe(2);
    expect(serializeExploreURLState({ ...restored, fileFilenameQuery: 'invoice', fileMIMEFamilies: ['pdf'] }))
      .toContain('explore=');
    expect(parseExploreURLState(serializeExploreURLState({
      ...restored, fileFilenameQuery: 'invoice', fileMIMEFamilies: ['pdf']
    }))).toMatchObject({ schemaVersion: 2, fileFilenameQuery: 'invoice', fileMIMEFamilies: ['pdf'] });
  });

  it('preserves unknown future fields while dropping session selection', () => {
    const future = {
      ...defaultExploreURLState,
      schemaVersion: 2,
      futureLayout: { density: 'compact' },
      selection: { mode: 'explicit', rowKeys: ['message:42'] }
    } as unknown as ExploreURLState;

    const restored = parseExploreURLState(serializeExploreURLState(future));

    expect(restored).toMatchObject({ schemaVersion: 2, futureLayout: { density: 'compact' } });
    expect(restored).not.toHaveProperty('selection');
  });

  it('preserves unrelated URL parameters while replacing the exploration envelope', () => {
    const serialized = serializeExploreURLState(
      { ...defaultExploreURLState, query: 'quarterly' },
      '?feature=preview&source=desktop&explore=obsolete'
    );
    const parameters = new URLSearchParams(serialized);

    expect(parameters.get('feature')).toBe('preview');
    expect(parameters.get('source')).toBe('desktop');
    expect(parseExploreURLState(serialized).query).toBe('quarterly');
  });

  it('uses independent defaults for absent and malformed state', () => {
    const absent = parseExploreURLState('');
    const malformed = parseExploreURLState('?explore=%7Bbad-json');
    absent.columns.push('size');
    absent.columnWidths.people = 100;

    expect(malformed).toEqual(defaultExploreURLState);
    expect(absent.columns).not.toBe(defaultExploreURLState.columns);
    expect(absent.columnWidths).not.toBe(defaultExploreURLState.columnWidths);
  });

  it('rejects malformed file-specific filters without changing canonical search', () => {
    const restored = parseExploreURLState(serializeExploreURLState({
      ...defaultExploreURLState,
      query: 'canonical terms',
      fileFilenameQuery: 42,
      fileMIMEFamilies: ['pdf', 'bogus']
    } as unknown as ExploreURLState));

    expect(restored.query).toBe('canonical terms');
    expect(restored.fileFilenameQuery).toBe('');
    expect(restored.fileMIMEFamilies).toEqual([]);
  });

  it('drops malformed namespaced attachment authority instead of treating it as an entry key', () => {
    const restored = parseExploreURLState(serializeExploreURLState({
      ...defaultExploreURLState,
      selectedRow: 'attachment:0'
    }));

    expect(restored.selectedRow).toBeNull();
  });
});

describe('relationships workspace state', () => {
  it('normalizes legacy people/domains workspaces to relationships facets', () => {
    const fromPeople = parseExploreURLState(serializeExploreURLState({
      ...defaultExploreURLState,
      workspace: 'people',
      analysisTarget: 'person:12'
    } as unknown as ExploreURLState));
    expect(fromPeople.workspace).toBe('relationships');
    expect(fromPeople.relationshipFacet).toBe('people');
    expect(fromPeople.relationshipTarget).toBe('cluster:12');

    const fromDomains = parseExploreURLState(serializeExploreURLState({
      ...defaultExploreURLState,
      workspace: 'domains',
      analysisTarget: 'domain:example.com'
    } as unknown as ExploreURLState));
    expect(fromDomains.workspace).toBe('relationships');
    expect(fromDomains.relationshipFacet).toBe('domains');
    expect(fromDomains.relationshipTarget).toBe('domain:example.com');

    const withoutTarget = parseExploreURLState(serializeExploreURLState({
      ...defaultExploreURLState,
      workspace: 'people',
      analysisTarget: null
    } as unknown as ExploreURLState));
    expect(withoutTarget.workspace).toBe('relationships');
    expect(withoutTarget.relationshipFacet).toBe('people');
    expect(withoutTarget.relationshipTarget).toBeNull();
  });

  it('defaults workspace to relationships', () => {
    expect(defaultExploreURLState.workspace).toBe('relationships');
    const restored = parseExploreURLState(`?explore=${encodeURIComponent('{}')}`);
    expect(restored.workspace).toBe('relationships');
    expect(restored.relationshipFacet).toBe('people');
    expect(restored.relationshipTarget).toBeNull();
    expect(restored.relationshipShowAll).toBe(false);
    expect(restored.relationshipFiles).toBe(false);
  });

  it('round-trips relationship fields through serialize/parse', () => {
    const state: ExploreURLState = {
      ...defaultExploreURLState,
      workspace: 'relationships',
      relationshipFacet: 'domains',
      relationshipTarget: 'domain:example.com',
      relationshipShowAll: true,
      relationshipFiles: true
    };

    expect(parseExploreURLState(serializeExploreURLState(state))).toMatchObject({
      workspace: 'relationships',
      relationshipFacet: 'domains',
      relationshipTarget: 'domain:example.com',
      relationshipShowAll: true,
      relationshipFiles: true
    });
  });

  it('clears the carried search query when entering the Relationships workspace', async () => {
    window.history.replaceState(null, '', '/');
    const state = new ExploreState(window);
    state.commitWorkspace('everything');
    state.commitSearch('quarterly plan', 'full_text');

    state.commitWorkspace('relationships');

    expect(state.current.workspace).toBe('relationships');
    expect(state.current.query).toBe('');
    expect(state.predicate()).not.toHaveProperty('query');
    expect(state.predicate()).not.toHaveProperty('search_mode');
    expect(parseExploreURLState(window.location.search).query).toBe('');

    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
    expect(state.current.workspace).toBe('everything');
    expect(state.current.query).toBe('quarterly plan');
    state.destroy();
  });

  it('rejects invalid facet/target shapes', () => {
    const restored = parseExploreURLState(serializeExploreURLState({
      ...defaultExploreURLState,
      relationshipFacet: 'x',
      relationshipTarget: 'garbage'
    } as unknown as ExploreURLState));

    expect(restored.relationshipFacet).toBe('people');
    expect(restored.relationshipTarget).toBeNull();

    const validCluster = parseExploreURLState(serializeExploreURLState({
      ...defaultExploreURLState,
      relationshipTarget: 'cluster:12'
    } as unknown as ExploreURLState));
    expect(validCluster.relationshipTarget).toBe('cluster:12');

    const invalidCluster = parseExploreURLState(serializeExploreURLState({
      ...defaultExploreURLState,
      relationshipTarget: 'cluster:abc'
    } as unknown as ExploreURLState));
    expect(invalidCluster.relationshipTarget).toBeNull();

    const validDomain = parseExploreURLState(serializeExploreURLState({
      ...defaultExploreURLState,
      relationshipTarget: 'domain:example.com'
    } as unknown as ExploreURLState));
    expect(validDomain.relationshipTarget).toBe('domain:example.com');
  });
});

describe('ExploreState history ownership', () => {
  it('uses remembered mode only when the URL has no explicit mode and remembers user changes', () => {
    const values = new Map([[SEARCH_MODE_PREFERENCE_KEY, 'semantic']]);
    const storage = {
      getItem: (key: string) => values.get(key) ?? null,
      setItem: (key: string, value: string) => values.set(key, value)
    };
    window.history.replaceState(null, '', '/');
    const remembered = new ExploreState(window, storage);
    expect(remembered.current.searchMode).toBe('semantic');
    remembered.replaceSearchDraft('', 'hybrid');
    expect(values.get(SEARCH_MODE_PREFERENCE_KEY)).toBe('hybrid');
    remembered.destroy();

    window.history.replaceState(null, '', serializeExploreURLState({
      ...defaultExploreURLState, searchMode: 'full_text'
    }));
    const explicit = new ExploreState(window, storage);
    expect(explicit.current.searchMode).toBe('full_text');
    explicit.destroy();
  });

  it('advances restoration epochs only for initial URL ownership and popstate', () => {
    window.history.replaceState(null, '', '/');
    const state = new ExploreState(window);
    const initialEpoch = state.restorationEpoch;
    expect(state.peekRestorationEpoch()).toBe(initialEpoch);

    state.replaceTransient({ columns: ['kind', 'title'] });
    expect(state.peekRestorationEpoch()).toBe(initialEpoch);
    state.replaceTransient({ query: 'draft' });
    expect(state.peekRestorationEpoch()).toBeUndefined();
    state.commitSearch('committed', 'full_text');
    expect(state.restorationEpoch).toBe(initialEpoch);

    window.dispatchEvent(new PopStateEvent('popstate'));
    expect(state.restorationEpoch).toBe(initialEpoch + 1);
    expect(state.peekRestorationEpoch()).toBe(initialEpoch + 1);
    state.acknowledgeRestoration(initialEpoch);
    expect(state.peekRestorationEpoch()).toBe(initialEpoch + 1);
    state.acknowledgeRestoration(initialEpoch + 1);
    expect(state.peekRestorationEpoch()).toBeUndefined();
    state.destroy();
  });

  it('can explicitly arm restoration for programmatic cross-workspace navigation', () => {
    window.history.replaceState(null, '', '/');
    const state = new ExploreState(window);
    state.acknowledgeRestoration(state.restorationEpoch);

    state.commitRestorableNavigation({ workspace: 'everything', selectedRow: 'message:901' });

    expect(state.current.workspace).toBe('everything');
    expect(state.current.selectedRow).toBe('message:901');
    expect(state.peekRestorationEpoch()).toBe(state.restorationEpoch);
    state.destroy();
  });

  it('omits the search pair from the server predicate until a query exists', () => {
    window.history.replaceState(null, '', '/');
    const state = new ExploreState(window);

    expect(state.predicate()).not.toHaveProperty('query');
    expect(state.predicate()).not.toHaveProperty('search_mode');
    state.replaceTransient({ query: 'roadmap' });
    expect(state.predicate()).toMatchObject({ query: 'roadmap', search_mode: 'full_text' });
    state.destroy();
  });

  it('keeps transient typing and resizing in the current entry while committing navigation state', () => {
    window.history.replaceState(null, '', '/?');
    const state = new ExploreState(window);

    state.replaceTransient({ query: 'quarter', columnWidths: { title: 420 } });
    state.commitSearch('quarter', 'semantic');
    state.commitNavigation({ groupingChain: ['source'], selectedRow: 'message:2' });

    expect(state.current.searchMode).toBe('semantic');
    expect(state.current.groupingChain).toEqual(['source']);
    expect(state.current.selectedRow).toBe('message:2');
    expect(parseExploreURLState(window.location.search)).toMatchObject({
      query: 'quarter',
      searchMode: 'semantic',
      groupingChain: ['source'],
      selectedRow: 'message:2'
    });
    state.destroy();
  });

  it('replaces committed navigation without creating another history entry', async () => {
    window.history.replaceState(null, '', '/?feature=preview');
    const state = new ExploreState(window);
    state.commitNavigation({ selectedRow: 'message:1', conversationAnchor: '1' });
    const preReplaceLength = window.history.length;

    const replace = (state as ExploreState & {
      replaceCommittedNavigation?: (patch: Partial<ExploreURLState>) => void
    }).replaceCommittedNavigation;
    if (!replace) {
      state.destroy();
      throw new Error('replaceCommittedNavigation is missing');
    }
    replace.call(state, { conversationAnchor: '2' });

    expect(window.history.length).toBe(preReplaceLength);
    expect(state.current.conversationAnchor).toBe('2');
    expect(parseExploreURLState(window.location.search).conversationAnchor).toBe('2');
    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
    expect(state.current.conversationAnchor).toBeNull();
    state.destroy();
  });

  it('commits a filename draft by replacement so the next push retains a byte-identical prior entry', async () => {
    window.history.replaceState(null, '', '/?feature=files');
    const state = new ExploreState(window);
    state.commitWorkspace('files');
    const beforeTypingLength = window.history.length;
    const replace = (state as ExploreState & {
      replaceCommittedDraft?: (patch: Partial<ExploreURLState>) => void
    }).replaceCommittedDraft;
    if (!replace) {
      state.destroy();
      throw new Error('replaceCommittedDraft is missing');
    }

    replace.call(state, {
      fileFilenameQuery: 'inv', activeRow: null, selectedRow: null, scrollAnchor: null
    });
    replace.call(state, {
      fileFilenameQuery: 'invoice', activeRow: null, selectedRow: null, scrollAnchor: null
    });
    const filteredURL = `${window.location.pathname}${window.location.search}${window.location.hash}`;
    expect(window.history.length).toBe(beforeTypingLength);
    state.commitNavigation({ selectedRow: 'file:9' });
    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));

    expect(`${window.location.pathname}${window.location.search}${window.location.hash}`).toBe(filteredURL);
    expect(state.current.fileFilenameQuery).toBe('invoice');
    expect(state.current.selectedRow).toBeNull();
    state.destroy();
  });

  it('preserves search prior-focus restoration across committed filename draft replacement', async () => {
    window.history.replaceState(null, '', '/?feature=files');
    const state = new ExploreState(window);
    state.commitWorkspace('files');
    state.replaceCommittedNavigation({ query: 'committed', searchMode: 'full_text' });
    state.replaceTransient({ activeRow: 'file:4' });
    state.replaceSearchDraft('draft', 'full_text');
    state.replaceCommittedDraft({ fileFilenameQuery: 'invoice' });
    state.commitSearch('draft', 'full_text');
    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));

    expect(state.current.fileFilenameQuery).toBe('invoice');
    expect(state.current.activeRow).toBe('file:4');
    expect(state.current.query).toBe('committed');
    state.destroy();
  });

  it('clears conversation authority when selection or search context changes', () => {
    window.history.replaceState(null, '', '/');
    const state = new ExploreState(window);
    state.commitNavigation({ selectedRow: 'message:1', conversationAnchor: '1' });
    state.commitNavigation({ selectedRow: 'message:2' });
    expect(state.current.conversationAnchor).toBeNull();
    state.commitNavigation({ conversationAnchor: '2' });
    state.commitNavigation({ selectedRow: null });
    expect(state.current.conversationAnchor).toBeNull();
    state.commitNavigation({ selectedRow: 'message:3', conversationAnchor: '3' });
    state.commitSearch('new context', 'full_text');
    expect(state.current.conversationAnchor).toBeNull();
    state.destroy();
  });

  it('keeps ordered nested groups distinct from filters and semantic search', () => {
    window.history.replaceState(null, '', '/');
    const state = new ExploreState(window);

    state.commitGrouping('participant');
    state.commitGrouping('year');
    state.commitSearch('pasta', 'semantic');
    state.commitNavigation({ filters: [{ dimension: 'domain', values: ['example.com'] }] });

    expect(state.current).toMatchObject({
      query: 'pasta',
      searchMode: 'semantic',
      groupingChain: ['participant', 'year'],
      filters: [{ dimension: 'domain', values: ['example.com'] }]
    });
    expect(state.predicate()).toMatchObject({
      query: 'pasta',
      search_mode: 'semantic',
      grouping: ['participant', 'year'],
      filters: [{ dimension: 'domain', values: ['example.com'] }]
    });
    expect(parseExploreURLState(window.location.search)).toMatchObject({
      groupingChain: ['participant', 'year'],
      filters: [{ dimension: 'domain', values: ['example.com'] }]
    });
    state.destroy();
  });

  it('appends domain to type and source to time in catalog order without duplicates', () => {
    window.history.replaceState(null, '', '/');
    const state = new ExploreState(window);

    state.commitGrouping('domain');
    state.commitGrouping('message_type');
    state.commitGrouping('domain');
    expect(state.current.groupingChain).toEqual(['domain', 'message_type']);

    state.commitGrouping('kind');
    expect(state.current.groupingChain).toEqual(['domain', 'message_type']);

    state.commitNavigation({ groupingChain: [] });
    state.commitGrouping('source');
    state.commitGrouping('month');
    expect(state.current.groupingChain).toEqual(['source', 'month']);
    state.destroy();
  });

  it('uses a history entry for ungroup so browser Back restores the nested chain', async () => {
    window.history.replaceState(null, '', '/');
    const state = new ExploreState(window);
    state.commitNavigation({ groupingChain: ['participant', 'year'] });

    state.commitUngroup();
    expect(state.current.groupingChain).toEqual(['participant']);

    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
    expect(state.current.groupingChain).toEqual(['participant', 'year']);
    state.destroy();
  });

  it('restores row, scroll, inspector, grouping, and mode on popstate', () => {
    const state = new ExploreState(window);
    const restored: ExploreURLState = {
      ...defaultExploreURLState,
      selectedRow: 'conversation:9',
      scrollAnchor: { key: 'conversation:7', offset: 8 },
      inspectorPinned: true,
      inspectorWidth: 512,
      groupingChain: ['source', 'month'],
      searchMode: 'hybrid',
      activeRow: 'conversation:8'
    };
    window.history.replaceState(null, '', serializeExploreURLState(restored));

    window.dispatchEvent(new PopStateEvent('popstate'));

    expect(state.current).toMatchObject(restored);
    state.destroy();
  });

  it('restores the previous committed search after transient typing is committed', async () => {
    window.history.replaceState(null, '', '/?feature=preview');
    const state = new ExploreState(window);
    state.commitSearch('alpha', 'full_text');
    state.replaceTransient({ query: 'beta draft' });
    state.commitSearch('beta', 'semantic');

    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));

    expect(state.current.query).toBe('alpha');
    expect(state.current.searchMode).toBe('full_text');
    expect(new URLSearchParams(window.location.search).get('feature')).toBe('preview');
    state.destroy();
  });

  it('merges the latest transient layout and focus into the prior committed history entry', async () => {
    window.history.replaceState(null, '', '/?feature=preview');
    const state = new ExploreState(window);
    state.commitSearch('alpha', 'full_text');
    state.replaceTransient({
      columns: ['kind', 'title', 'time'],
      columnWidths: { title: 432 },
      activeRow: 'message:4888',
      scrollAnchor: { key: 'message:4880', offset: 17 }
    });
    state.replaceSearchDraft('beta draft', 'semantic');
    expect(state.current.activeRow).toBeNull();
    expect(state.current.scrollAnchor).toBeNull();
    state.commitSearch('beta', 'semantic');

    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));

    expect(state.current).toMatchObject({
      query: 'alpha',
      searchMode: 'full_text',
      columns: ['kind', 'title', 'time'],
      columnWidths: { title: 432 },
      activeRow: 'message:4888',
      scrollAnchor: { key: 'message:4880', offset: 17 }
    });
    state.destroy();
  });

  it.each([
    ['opening a row', (state: ExploreState) => state.commitNavigation({ selectedRow: 'message:beta' })],
    ['drilling a group', (state: ExploreState) => state.commitNavigation({
      filters: [{ dimension: 'source', values: ['7'] }],
      groupingChain: [],
      selectedRow: 'group:source:7'
    })],
    ['switching workspace', (state: ExploreState) => state.commitWorkspace('settings')]
  ] as const)('applies a pending search-draft snapshot when %s is the next push', async (_name, push) => {
    window.history.replaceState(null, '', '/?feature=preview');
    const state = new ExploreState(window);
    state.commitSearch('alpha', 'full_text');
    state.replaceTransient({
      columns: ['kind', 'title', 'time'],
      columnWidths: { title: 432 },
      activeRow: 'message:4888',
      scrollAnchor: { key: 'message:4880', offset: 17 }
    });
    state.replaceSearchDraft('beta draft', 'semantic');

    push(state);
    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));

    expect(state.current).toMatchObject({
      workspace: 'relationships',
      query: 'alpha',
      searchMode: 'full_text',
      columns: ['kind', 'title', 'time'],
      columnWidths: { title: 432 },
      activeRow: 'message:4888',
      scrollAnchor: { key: 'message:4880', offset: 17 }
    });
    state.destroy();
  });

  it('consumes the draft snapshot once while retaining each later entry focus', async () => {
    window.history.replaceState(null, '', '/');
    const state = new ExploreState(window);
    state.commitSearch('alpha', 'full_text');
    state.replaceTransient({
      columns: ['kind', 'title', 'time'],
      activeRow: 'message:4888',
      scrollAnchor: { key: 'message:4880', offset: 17 }
    });
    state.replaceSearchDraft('beta draft', 'semantic');
    state.commitNavigation({ selectedRow: 'message:beta' });
    state.replaceTransient({
      activeRow: 'message:beta',
      scrollAnchor: { key: 'message:beta', offset: 9 }
    });
    state.commitWorkspace('settings');

    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
    expect(state.current).toMatchObject({
      query: 'beta draft',
      activeRow: 'message:beta',
      scrollAnchor: { key: 'message:beta', offset: 9 }
    });

    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
    expect(state.current).toMatchObject({
      query: 'alpha',
      activeRow: 'message:4888',
      scrollAnchor: { key: 'message:4880', offset: 17 }
    });
    state.destroy();
  });

  it('keeps the original snapshot through draft replacement and cancels it on popstate', async () => {
    window.history.replaceState(null, '', '/');
    const state = new ExploreState(window);
    state.commitSearch('alpha', 'full_text');
    state.replaceTransient({
      activeRow: 'message:alpha',
      scrollAnchor: { key: 'message:alpha', offset: 4 }
    });
    state.replaceSearchDraft('beta draft', 'semantic');
    state.replaceTransient({
      activeRow: 'message:beta',
      scrollAnchor: { key: 'message:beta', offset: 8 }
    });
    state.replaceSearchDraft('gamma draft', 'hybrid');
    state.commitNavigation({ selectedRow: 'message:gamma' });

    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
    expect(state.current).toMatchObject({
      query: 'alpha',
      activeRow: 'message:alpha',
      scrollAnchor: { key: 'message:alpha', offset: 4 }
    });

    const replacement = {
      ...defaultExploreURLState,
      query: 'replacement',
      activeRow: 'message:replacement',
      scrollAnchor: { key: 'message:replacement', offset: 12 }
    };
    window.history.replaceState(null, '', serializeExploreURLState(replacement));
    window.dispatchEvent(new PopStateEvent('popstate'));
    state.commitNavigation({ selectedRow: 'message:next' });
    window.history.back();
    await new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
    expect(state.current).toMatchObject(replacement);
    state.destroy();
  });
});

describe('all-matching result authority', () => {
  const baseResult = {
    rows: [],
    totalCount: 8,
    cacheRevision: 'cache-3',
    searchProvenance: {},
    candidatePoolSaturated: false
  };
  const authorityCases: Array<[
    ExplorePredicate,
    SearchProvenance,
    string | undefined,
    boolean
  ]> = [
    [{ filters: [], presentation: 'table' }, {}, undefined, true],
    [{ query: 'alpha', search_mode: 'full_text' }, { lexical_index_revision: 'fts-2' }, undefined, true],
    [{ query: 'alpha', search_mode: 'semantic' }, { vector_generation: 4 }, 'snapshot-2', true],
    [{ query: 'alpha', search_mode: 'hybrid' }, { lexical_index_revision: 'fts-2', vector_generation: 4 }, 'snapshot-2', true],
    [{ query: 'alpha', search_mode: 'full_text' }, {}, undefined, false],
    [{ query: 'alpha', search_mode: 'semantic' }, { lexical_index_revision: 'fts-2' }, 'snapshot-2', false],
    [{ query: 'alpha', search_mode: 'hybrid' }, { vector_generation: 4 }, 'snapshot-2', false]
  ];

  it.each(authorityCases)('validates mode-specific search provenance for %o', (predicate, provenance, snapshot, valid) => {
    const selection = createAllMatchingSelection(
      predicate,
      { ...baseResult, searchProvenance: provenance, ...(snapshot ? { candidateSnapshotId: snapshot } : {}) },
      7
    );

    expect(Boolean(selection)).toBe(valid);
    if (selection) {
      expect(selection.predicateFingerprint).toBe(predicateFingerprint(predicate));
      expect(selection.resultGeneration).toBe(7);
    }
  });

  it('does not mint exact all-matching authority from a capped candidate pool', () => {
    expect(createAllMatchingSelection(
      { query: 'alpha', search_mode: 'full_text' },
      { ...baseResult, candidatePoolSaturated: true, searchProvenance: { lexical_index_revision: 'fts-2' } },
      7
    )).toBeUndefined();
  });
});

describe('session-owned exploration selection', () => {
  it('supports explicit toggle and inclusive shift range by stable row key', () => {
    const selection = new ExploreSelectionState();
    const keys = ['message:1', 'message:2', 'message:3', 'message:4'];

    selection.toggle(keys[0]!, 0, keys);
    selection.toggle(keys[3]!, 3, keys, true);

    expect(selection.selectedKeys(keys)).toEqual(keys);
  });

  it('anchors a visible selection range to its first stable key after reorder', () => {
    const selection = new ExploreSelectionState();
    selection.selectVisible(['message:2', 'message:3']);

    selection.toggle('message:4', 2, ['message:1', 'message:2', 'message:4', 'message:3'], true);

    expect(selection.selectedKeys(['message:1', 'message:2', 'message:3', 'message:4']))
      .toEqual(['message:2', 'message:3', 'message:4']);
  });

  it('retains the complete all-matching predicate, exclusions, and revisions', () => {
    const allMatching: ExploreSelection = {
      mode: 'all_matching',
      predicate: {
        query: 'roadmap',
        search_mode: 'hybrid',
        filters: [{ dimension: 'source', values: ['7'] }],
        grouping: ['participant'],
        presentation: 'table',
        sort: [{ field: 'occurred_at', direction: 'desc' }]
      },
      exclusions: [],
      cacheRevision: 'cache-12',
      searchProvenance: { lexical_index_revision: 'fts-4', vector_generation: 8 },
      candidateSnapshotId: 'snapshot-3',
      predicateFingerprint: predicateFingerprint({
        query: 'roadmap',
        search_mode: 'hybrid',
        filters: [{ dimension: 'source', values: ['7'] }],
        grouping: ['participant'],
        presentation: 'table',
        sort: [{ field: 'occurred_at', direction: 'desc' }]
      }),
      resultGeneration: 3
    };
    const selection = new ExploreSelectionState();

    selection.selectAllMatching(allMatching);
    selection.toggle('message:9', 0, ['message:9']);

    expect(selection.snapshot()).toEqual({ ...allMatching, exclusions: ['message:9'] });
    expect(window.location.search).not.toContain('selection');
  });

  it('requires a server candidate snapshot for semantic all-matching selection', () => {
    const selection = new ExploreSelectionState();

    expect(() =>
      selection.selectAllMatching({
        mode: 'all_matching',
        predicate: { query: 'roadmap', search_mode: 'semantic' },
        exclusions: [],
        cacheRevision: 'cache-12',
        searchProvenance: { vector_generation: 8 },
        predicateFingerprint: predicateFingerprint({ query: 'roadmap', search_mode: 'semantic' }),
        resultGeneration: 3
      })
    ).toThrow('candidate snapshot');
  });
});
