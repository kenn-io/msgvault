import type { MeetingPanelScope } from '../../meetings/controller.svelte';
import type { ReadingPaneSelection } from '../reader/ReadingPane.svelte';
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
  /** The mounted meeting overview retains its exact authority during a reload
   * of the same predicate, including restorable archive-reader navigation. */
  meetingOverview = $state<{ fingerprint: string; scope: MeetingPanelScope; refreshKey: number }>();
  /** Loaded group detail, including its own predicate/cache/search authority.
   * Kept together so a round-trip cannot substitute the outer list snapshot. */
  readingGroupDetail = $state<ReadingPaneSelection>();
  /** Generation counter guarding the group-detail fetch against stale/aborted responses. */
  readingDetailGeneration = 0;
  /** Predicate, outer authority and exact group identity used to load the detail. */
  readingDetailFingerprint = '';
}
