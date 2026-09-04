import { SvelteMap, SvelteSet } from 'svelte/reactivity';

import type { APIClient } from '../api/client';
import type { components } from '../api/generated/schema';
import type {
  DirectoryReviewKind,
  IdentityReviewState,
  RelationshipReviewState
} from '../explore/models';
import {
  validatePersonMergeRequired,
  type PersonMergeSuccess,
  type ValidatedPersonMergeRequired
} from './person-merge';

export const IDENTITY_REVIEW_PAGE_LIMIT = 100;

export type IdentityMatchCandidate = components['schemas']['IdentityMatchCandidate'];
export type PersonMergeRequiredError = ValidatedPersonMergeRequired;
type ReviewCommit = (patch: {
  reviewKind?: DirectoryReviewKind;
  identityState?: IdentityReviewState;
  relationshipReviewState?: RelationshipReviewState;
}) => void;

export type IdentityDecisionResult =
  | { ok: true; candidate: IdentityMatchCandidate; cacheState: 'ready' | 'stale' }
  | { ok: false; kind: 'merge_required'; conflict: PersonMergeRequiredError }
  | { ok: false; kind: 'error'; status: number; message: string };

interface ReviewURLState {
  reviewKind: DirectoryReviewKind;
  identityState: IdentityReviewState;
}

export interface DirectoryReviewContextSnapshot extends ReviewURLState {
  generation: number;
  offset: number;
}

export type DirectoryReviewMergeCompletion = PersonMergeSuccess & { candidateID: number };

/**
 * Browser-independent owner for the Directory review queue. URL filters stay
 * with ExploreState; request generations, offsets, pending decisions and
 * recovery state deliberately remain ephemeral.
 */
export class DirectoryReviewController {
  reviewKind = $state<DirectoryReviewKind>('identity');
  identityState = $state<IdentityReviewState>('candidate');
  rows = $state<IdentityMatchCandidate[]>([]);
  offset = $state(0);
  loading = $state(false);
  error = $state<string | null>(null);
  pageError = $state<string | null>(null);
  decisionError = $state<string | null>(null);
  status = $state<string | null>(null);
  mergeRequired = $state<{ candidateID: number; conflict: PersonMergeRequiredError } | null>(null);
  lastMerge = $state<DirectoryReviewMergeCompletion | null>(null);
  readonly pendingDecisions = new SvelteSet<number>();

  private readonly client: APIClient;
  private readonly commit: ReviewCommit;
  private pageAbort: AbortController | undefined;
  private pageGeneration = 0;
  private reviewContextGeneration = $state(0);
  private retryOffset: number | undefined;
  private initialized = false;
  private disposed = false;
  private readonly decisionRequests = new Map<number, AbortController>();
  private readonly decisionDrafts = new SvelteMap<number, string>();

  constructor(client: APIClient, commit: ReviewCommit = () => undefined) {
    this.client = client;
    this.commit = commit;
  }

  get hasPreviousPage(): boolean { return this.offset > 0; }
  get hasNextPage(): boolean { return this.rows.length === IDENTITY_REVIEW_PAGE_LIMIT; }
  get apiClient(): APIClient { return this.client; }

  isDecisionPending(candidateID: number): boolean {
    return this.pendingDecisions.has(candidateID);
  }

  getDecisionDraft(candidateID: number): string {
    return this.decisionDrafts.get(candidateID) ?? '';
  }

  setDecisionDraft(candidateID: number, draft: string): void {
    if (this.disposed || this.pendingDecisions.has(candidateID)) return;
    if (draft === '') this.decisionDrafts.delete(candidateID);
    else this.decisionDrafts.set(candidateID, draft);
  }

  clearDecisionDraft(candidateID: number): void {
    if (this.disposed || this.pendingDecisions.has(candidateID)) return;
    this.decisionDrafts.delete(candidateID);
  }

  reviewContextSnapshot(): DirectoryReviewContextSnapshot {
    return {
      generation: this.reviewContextGeneration,
      reviewKind: this.reviewKind,
      identityState: this.identityState,
      offset: this.offset
    };
  }

  isReviewContextCurrent(context: DirectoryReviewContextSnapshot): boolean {
    return !this.disposed && context.generation === this.reviewContextGeneration &&
      context.reviewKind === this.reviewKind && context.identityState === this.identityState &&
      context.offset === this.offset;
  }

  applyURLState(state: ReviewURLState, historyRestoration = false): void {
    if (this.disposed) return;
    const changed = this.reviewKind !== state.reviewKind || this.identityState !== state.identityState;
    this.reviewKind = state.reviewKind;
    this.identityState = state.identityState;
    if (state.reviewKind !== 'identity') {
      if (!this.initialized || changed || historyRestoration) this.resetIdentityContext(state.identityState);
      this.initialized = true;
      return;
    }
    if (!this.initialized || changed || historyRestoration) {
      this.initialized = true;
      this.resetIdentityContext(state.identityState);
      void this.loadIdentityPage(0, state.identityState);
    }
  }

  setReviewKind(reviewKind: DirectoryReviewKind): void {
    if (this.reviewKind === reviewKind) return;
    this.reviewKind = reviewKind;
    this.commit({ reviewKind });
    this.initialized = true;
    this.resetIdentityContext(this.identityState);
    if (reviewKind === 'identity') void this.loadIdentityPage(0, this.identityState);
  }

  setIdentityState(identityState: IdentityReviewState): void {
    if (this.reviewKind === 'identity' && this.identityState === identityState && this.initialized) return;
    this.identityState = identityState;
    this.reviewKind = 'identity';
    this.commit({ reviewKind: 'identity', identityState });
    this.initialized = true;
    this.resetIdentityContext(identityState);
    void this.loadIdentityPage(0, identityState);
  }

  async loadIdentityPage(
    targetOffset = this.offset,
    state: IdentityReviewState = this.identityState
  ): Promise<boolean> {
    if (this.disposed) return false;
    const contextChanged = this.reviewKind !== 'identity' || this.identityState !== state;
    if (contextChanged) {
      this.reviewKind = 'identity';
      this.resetIdentityContext(state);
      targetOffset = 0;
    }
    this.initialized = true;
    this.identityState = state;
    this.reviewKind = 'identity';
    this.pageAbort?.abort();
    const abort = new AbortController();
    this.pageAbort = abort;
    const generation = ++this.pageGeneration;
    this.loading = true;
    this.error = null;
    this.pageError = null;
    this.retryOffset = undefined;
    try {
      const response = await this.client.GET('/api/v1/identity/match-candidates', {
        params: { query: { state, limit: IDENTITY_REVIEW_PAGE_LIMIT, offset: targetOffset } },
        signal: abort.signal
      });
      if (!this.ownsPage(abort, generation)) return false;
      if (response.data) {
        this.rows = response.data.candidates ?? [];
        this.offset = response.data.offset;
        return true;
      }
      this.retainPageFailure(targetOffset, failureMessage(response.error, response.response.status));
      return false;
    } catch (cause: unknown) {
      if (!this.ownsPage(abort, generation)) return false;
      this.retainPageFailure(targetOffset, failureMessage(cause, 0));
      return false;
    } finally {
      if (generation === this.pageGeneration) this.loading = false;
    }
  }

  async loadNextPage(): Promise<void> {
    if (this.loading || !this.hasNextPage) return;
    await this.loadIdentityPage(this.offset + IDENTITY_REVIEW_PAGE_LIMIT);
  }

  async loadPreviousPage(): Promise<void> {
    if (this.loading || !this.hasPreviousPage) return;
    await this.loadIdentityPage(Math.max(0, this.offset - IDENTITY_REVIEW_PAGE_LIMIT));
  }

  async retryPage(): Promise<void> {
    await this.loadIdentityPage(this.retryOffset ?? this.offset);
  }

  async acceptIdentity(
    candidateID: number,
    notes?: string,
    context = this.reviewContextSnapshot()
  ): Promise<IdentityDecisionResult> {
    return this.decideIdentity(candidateID, 'accept', notes, context);
  }

  async rejectIdentity(
    candidateID: number,
    notes?: string,
    context = this.reviewContextSnapshot()
  ): Promise<IdentityDecisionResult> {
    return this.decideIdentity(candidateID, 'reject', notes, context);
  }

  async completePersonMerge(
    candidateID: number,
    context: DirectoryReviewContextSnapshot,
    success: PersonMergeSuccess
  ): Promise<void> {
    if (this.disposed) return;
    this.lastMerge = { candidateID, ...success };
    this.decisionDrafts.delete(candidateID);
    if (!this.isReviewContextCurrent(context)) return;
    if (this.mergeRequired?.candidateID === candidateID) this.mergeRequired = null;
    const name = success.survivor.display_name?.trim() || `Person ${success.survivor.id}`;
    this.status = `People merged into ${name}. Identity cache ${success.result.cache_state}.`;
    await this.loadIdentityPage(context.offset, context.identityState);
  }

  destroy(): void {
    this.disposed = true;
    this.pageAbort?.abort();
    this.pageAbort = undefined;
    ++this.pageGeneration;
    for (const request of this.decisionRequests.values()) request.abort();
    this.decisionRequests.clear();
    this.pendingDecisions.clear();
    this.decisionDrafts.clear();
    this.loading = false;
  }

  private async decideIdentity(
    candidateID: number,
    decision: 'accept' | 'reject',
    notes: string | undefined,
    context: DirectoryReviewContextSnapshot
  ): Promise<IdentityDecisionResult> {
    if (!this.isReviewContextCurrent(context)) {
      return { ok: false, kind: 'error', status: 0, message: 'The review context changed.' };
    }
    if (this.disposed || this.pendingDecisions.has(candidateID)) {
      return { ok: false, kind: 'error', status: 0, message: 'A decision is already pending.' };
    }
    if (notes !== undefined) this.setDecisionDraft(candidateID, notes);
    this.decisionError = null;
    this.status = null;
    if (decision === 'accept' && this.mergeRequired?.candidateID === candidateID) this.mergeRequired = null;
    const abort = new AbortController();
    this.decisionRequests.set(candidateID, abort);
    this.pendingDecisions.add(candidateID);
    const trimmedNotes = this.getDecisionDraft(candidateID).trim();
    const body = trimmedNotes ? { notes: trimmedNotes } : {};
    try {
      const response = decision === 'accept'
        ? await this.client.POST('/api/v1/identity/match-candidates/{id}/accept', {
            params: { path: { id: candidateID } }, body, signal: abort.signal
          })
        : await this.client.POST('/api/v1/identity/match-candidates/{id}/reject', {
            params: { path: { id: candidateID } }, body, signal: abort.signal
          });
      if (!this.ownsDecision(candidateID, abort)) {
        return { ok: false, kind: 'error', status: 0, message: 'Decision was superseded.' };
      }
      if (response.data) {
        const decidedCandidate = response.data.candidate;
        this.decisionDrafts.delete(candidateID);
        if (!this.ownsDecisionContext(context)) {
          return { ok: true, candidate: decidedCandidate, cacheState: response.data.cache_state };
        }
        this.rows = replaceByID(this.rows, decidedCandidate);
        this.status = `Identity match ${decidedCandidate.state}.`;
        await this.loadIdentityPage(this.offset, this.identityState);
        return { ok: true, candidate: decidedCandidate, cacheState: response.data.cache_state };
      }
      const mergeConflict = decision === 'accept' && response.response.status === 409
        ? validatePersonMergeRequired(response.error)
        : null;
      if (mergeConflict) {
        if (this.ownsDecisionContext(context)) this.mergeRequired = { candidateID, conflict: mergeConflict };
        return { ok: false, kind: 'merge_required', conflict: mergeConflict };
      }
      const message = failureMessage(response.error, response.response.status);
      if (this.ownsDecisionContext(context)) this.decisionError = message;
      return { ok: false, kind: 'error', status: response.response.status, message };
    } catch (cause: unknown) {
      if (!this.ownsDecision(candidateID, abort)) {
        return { ok: false, kind: 'error', status: 0, message: 'Decision was superseded.' };
      }
      const message = failureMessage(cause, 0);
      if (this.ownsDecisionContext(context)) this.decisionError = message;
      return { ok: false, kind: 'error', status: 0, message };
    } finally {
      if (this.decisionRequests.get(candidateID) === abort) {
        this.decisionRequests.delete(candidateID);
        this.pendingDecisions.delete(candidateID);
      }
    }
  }

  private ownsPage(abort: AbortController, generation: number): boolean {
    return !this.disposed && !abort.signal.aborted && this.pageAbort === abort && this.pageGeneration === generation;
  }

  private ownsDecision(candidateID: number, abort: AbortController): boolean {
    return !this.disposed && !abort.signal.aborted && this.decisionRequests.get(candidateID) === abort;
  }

  private ownsDecisionContext(context: DirectoryReviewContextSnapshot): boolean {
    return this.isReviewContextCurrent(context);
  }

  private retainPageFailure(targetOffset: number, message: string): void {
    this.retryOffset = targetOffset;
    if (this.rows.length > 0) this.pageError = message;
    else this.error = message;
  }

  private resetIdentityContext(identityState: IdentityReviewState): void {
    ++this.reviewContextGeneration;
    this.pageAbort?.abort();
    this.pageAbort = undefined;
    ++this.pageGeneration;
    this.identityState = identityState;
    this.rows = [];
    this.offset = 0;
    this.loading = false;
    this.error = null;
    this.pageError = null;
    this.decisionError = null;
    this.status = null;
    this.mergeRequired = null;
    this.retryOffset = undefined;
  }
}

function replaceByID(rows: IdentityMatchCandidate[], replacement: IdentityMatchCandidate): IdentityMatchCandidate[] {
  return rows.some((row) => row.id === replacement.id)
    ? rows.map((row) => row.id === replacement.id ? replacement : row)
    : [...rows, replacement];
}

function failureMessage(error: unknown, status: number): string {
  if (typeof error === 'object' && error !== null && 'message' in error && typeof error.message === 'string') {
    return error.message;
  }
  if (error instanceof Error && error.message) return error.message;
  return status > 0 ? `Request failed (${status}).` : 'Request failed.';
}
