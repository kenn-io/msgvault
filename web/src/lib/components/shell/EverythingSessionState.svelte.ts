import type { InspectorSelection } from '../inspector/Inspector.svelte';
import { VisibleLexicalCountCache, type SearchCoverageValue } from '../../search/modes';

/**
 * Session-scoped state for the Everything workspace that must survive a
 * workspace round-trip (navigating away from 'everything' and back).
 *
 * AppShell renders EverythingWorkspace behind an `{#if}` chain alongside the
 * other workspaces, so Svelte destroys and recreates it on every such
 * switch. Before EverythingWorkspace was split out of AppShell, this state
 * lived at AppShell's persistent top level and never lost continuity; this
 * container restores that by living in AppShell (instantiated once, like the
 * loader/controller) and being passed down as a prop. EverythingWorkspace
 * reads and writes through it while keeping its own `$effect`s, in-flight
 * `AbortController`s, and timer handles local to each mount.
 */
export class EverythingSessionState {
  /** Latest semantic coverage snapshot for the active poll key. */
  coverage = $state<SearchCoverageValue>();
  /** Exponential-backoff attempt count for the current coverage poll key. */
  coveragePollAttempts = 0;
  /** `workspace|mode|filtersFingerprint` identifying the active coverage poll. */
  coveragePollKey = '';
  /** Row keys the visible table/timeline currently has on screen, for exact lexical counts. */
  visibleLexicalRowKeys = $state<string[]>([]);
  /** LRU cache of exact lexical match counts, keyed by query/revision/rows. */
  readonly lexicalCountCache = new VisibleLexicalCountCache(128);
  /** Maps a (lexicalRevision, predicateFingerprint) pair to its last-seen canonical query hash. */
  readonly canonicalQueryHashes = new Map<string, string>();
  /** Loaded detail for the currently inspected group row, if any. */
  inspectorGroupDetail = $state<InspectorSelection>();
  /** Generation counter guarding the inspector-detail fetch against stale/aborted responses. */
  inspectorDetailGeneration = 0;
  /** `predicateFingerprint|group:dimension:key` that `inspectorGroupDetail` was loaded under. */
  inspectorDetailFingerprint = '';
}
