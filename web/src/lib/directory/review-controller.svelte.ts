import { ENDPOINT_NAMES, endpointKey, type EndpointSummary } from '../people/duplicates.svelte';
import {
  getParticipant as generatedGetParticipant,
  getPersonProfile as generatedGetPersonProfile,
  reviewAcceptIdentityMatchCandidate as generatedAcceptIdentityMatchCandidate,
  listIdentityMatchCandidates as generatedListIdentityMatchCandidates,
  reviewRejectIdentityMatchCandidate as generatedRejectIdentityMatchCandidate,
} from '../api/generated/api/api';
import { SvelteMap, SvelteSet } from 'svelte/reactivity';
import type { APIClient } from '../api/client';
import type { IdentityMatchCandidate as GeneratedIdentityMatchCandidate } from '../api/generated/models';
import type { DirectoryReviewKind, IdentityReviewState, RelationshipReviewState } from '../explore/models';
import {
  validatePersonMergeRequired,
  type PersonMergeSuccess,
  type ValidatedPersonMergeRequired,
} from './person-merge';
export const IDENTITY_REVIEW_PAGE_LIMIT = 10;
export type IdentityMatchCandidate = GeneratedIdentityMatchCandidate;
export type PersonMergeRequiredError = ValidatedPersonMergeRequired;
type ReviewCommit = (patch: {
  reviewKind?: DirectoryReviewKind;
  identityState?: IdentityReviewState;
  relationshipReviewState?: RelationshipReviewState;
}) => void;
export type IdentityDecisionResult =
  | {
      ok: true;
      candidate: IdentityMatchCandidate;
      cacheState: 'ready' | 'stale';
    }
  | {
      ok: false;
      kind: 'merge_required';
      conflict: PersonMergeRequiredError;
    }
  | {
      ok: false;
      kind: 'error' | 'stale';
      status: number;
      message: string;
    };
interface ReviewURLState {
  reviewKind: DirectoryReviewKind;
  identityState: IdentityReviewState;
}
export interface DirectoryReviewContextSnapshot extends ReviewURLState {
  generation: number;
  offset: number;
}
export type DirectoryReviewMergeCompletion = PersonMergeSuccess & {
  candidateID: number;
};
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
  mergeRequired = $state<{
    candidateID: number;
    conflict: PersonMergeRequiredError;
  } | null>(null);
  lastMerge = $state<DirectoryReviewMergeCompletion | null>(null);
  readonly summaries = new SvelteMap<string, EndpointSummary | 'failed'>();
  readonly hidden = new SvelteSet<number>();
  private lookupQueue: string[] = [];
  private lookupRunning = false;
  private lookupAbort = new AbortController();
  private lookupPauseMs = 120;
  private lookupTimer: ReturnType<typeof setTimeout> | undefined;
  staleDecision = $state<IdentityMatchCandidate>();
  private readonly onSettled: (result: IdentityDecisionResult) => void;
  pending = $state<{ candidate: IdentityMatchCandidate; decision: 'accept' | 'reject'; context: DirectoryReviewContextSnapshot }>();
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
  constructor(client: APIClient, commit: ReviewCommit = () => undefined, onSettled: (result: IdentityDecisionResult) => void = () => undefined) {
    this.client = client;
    this.onSettled = onSettled;
    this.commit = commit;
  }
  get hasPreviousPage(): boolean {
    return this.offset > 0;
  }
  get hasNextPage(): boolean {
    return this.rows.length === IDENTITY_REVIEW_PAGE_LIMIT;
  }
  get apiClient(): APIClient {
    return this.client;
  }
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
      offset: this.offset,
    };
  }
  isReviewContextCurrent(context: DirectoryReviewContextSnapshot): boolean {
    return (
      !this.disposed &&
      context.generation === this.reviewContextGeneration &&
      context.reviewKind === this.reviewKind &&
      context.identityState === this.identityState &&
      context.offset === this.offset
    );
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
    void this.settleDecision();
    this.identityState = identityState;
    this.reviewKind = 'identity';
    this.commit({ identityState });
    this.initialized = true;
    this.resetIdentityContext(identityState);
    void this.loadIdentityPage(0, identityState);
  }
  async loadIdentityPage(
    targetOffset = this.offset,
    state: IdentityReviewState = this.identityState,
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
      const response = await generatedListIdentityMatchCandidates(
        { state: state === 'accepted' || state === 'rejected' ? 'accepted,rejected' : 'candidate,conflict', limit: IDENTITY_REVIEW_PAGE_LIMIT, offset: targetOffset },
        {
          ...this.client,
          signal: abort.signal,
        },
      );
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
    void this.settleDecision();
    await this.loadIdentityPage(this.offset + IDENTITY_REVIEW_PAGE_LIMIT);
  }
  async loadPreviousPage(): Promise<void> {
    if (this.loading || !this.hasPreviousPage) return;
    void this.settleDecision();
    await this.loadIdentityPage(Math.max(0, this.offset - IDENTITY_REVIEW_PAGE_LIMIT));
  }
  async retryPage(): Promise<void> {
    void this.settleDecision();
    await this.loadIdentityPage(this.retryOffset ?? this.offset);
  }
  async acceptIdentity(
    candidateID: number,
    notes?: string,
    context = this.reviewContextSnapshot(),
    reviewToken?: string,
  ): Promise<IdentityDecisionResult> {
    return this.decideIdentity(candidateID, 'accept', notes, context, reviewToken);
  }
  async rejectIdentity(
    candidateID: number,
    notes?: string,
    context = this.reviewContextSnapshot(),
    reviewToken?: string,
  ): Promise<IdentityDecisionResult> {
    return this.decideIdentity(candidateID, 'reject', notes, context, reviewToken);
  }
  async completePersonMerge(
    candidateID: number,
    context: DirectoryReviewContextSnapshot,
    success: PersonMergeSuccess,
  ): Promise<void> {
    if (this.disposed) return;
    this.lastMerge = { candidateID, ...success };
    this.summaries.clear();
    if (!this.isReviewContextCurrent(context)) return;
    if (this.mergeRequired?.candidateID === candidateID) this.mergeRequired = null;
    const name = success.survivor.display_name?.trim() || `Person ${success.survivor.id}`;
    this.status = `Profiles merged into ${name}. Review the refreshed pair to finish. Identity cache ${success.result.cache_state}.`;
    await this.loadIdentityPage(context.offset, context.identityState);
  }
  queueDecision(candidate: IdentityMatchCandidate, decision: 'accept' | 'reject'): void {
    void this.settleDecision();
    this.hidden.add(candidate.id);
    this.pending = { candidate, decision, context: this.reviewContextSnapshot() };
  }
  undoDecision(): void {
    if (this.pending) this.hidden.delete(this.pending.candidate.id);
    this.pending = undefined;
  }
  async settleDecision(): Promise<IdentityDecisionResult | undefined> {
    const pending = this.pending;
    if (!pending) return;
    this.pending = undefined;
    const { candidate, decision, context } = pending;
    const result = await this.decideIdentity(candidate.id, decision, this.getDecisionDraft(candidate.id), context, candidate.review_token);
    this.hidden.delete(candidate.id);
    if (!result.ok && result.kind === 'merge_required') this.mergeRequired = { candidateID: candidate.id, conflict: result.conflict };
    if (!result.ok && result.kind !== 'merge_required') this.decisionError = result.message;
    this.onSettled(result);
    return result;
  }
  async retryCache(): Promise<void> {
    const candidate = this.staleDecision;
    if (!candidate) return;
    const result = await this.acceptIdentity(candidate.id, undefined, this.reviewContextSnapshot(), candidate.review_token);
    this.onSettled(result);
  }
  retrySummary(key: string): void { this.summaries.delete(key); this.requestSummaries(); }
  /** Queues the lookups for the cards on the current page. */
  requestSummaries(): void {
    this.lookupQueue = [];
    this.lookupAbort.abort();
    this.lookupAbort = new AbortController();
    for (const candidate of this.rows) {
      for (const side of ['left', 'right'] as const) {
        const key = endpointKey(candidate, side);
        if (this.summaries.has(key) || this.lookupQueue.includes(key)) continue;
        const kind = side === 'left' ? candidate.left_kind : candidate.right_kind;
        if (kind !== 'participant' && kind !== 'person') {
          const id = side === 'left' ? candidate.left_id : candidate.right_id;
          this.summaries.set(key, { id, name: ENDPOINT_NAMES[kind] ?? 'Unknown', address: candidate.normalized_value ?? '', lastAt: null, count: 0 });
          continue;
        }
        this.lookupQueue.push(key);
      }
    }
    this.pumpLookups();
  }

  private pumpLookups(): void {
    if (this.lookupRunning || this.lookupQueue.length === 0 || this.disposed) return;
    const key = this.lookupQueue.shift()!;
    this.lookupRunning = true;
    void this.lookup(key).finally(() => {
      this.lookupTimer = setTimeout(() => {
        this.lookupRunning = false;
        this.pumpLookups();
      }, this.lookupPauseMs);
    });
  }

  private async lookup(key: string, signal = this.lookupAbort.signal): Promise<void> {
    const [kind, raw] = key.split(':') as [string, string];
    const id = Number(raw);
    if (kind === 'person') {
      try {
        const response = await generatedGetPersonProfile({ id }, { ...this.client, signal });
        if (this.disposed || signal.aborted) return;
        const name = response.data?.display_name?.trim();
        this.summaries.set(key, response.data ? { id, name: name || `Person ${id}`, address: 'Saved person', lastAt: null, count: 0, personID: id } : 'failed');
      } catch {
        if (!this.disposed && !signal.aborted) this.summaries.set(key, 'failed');
      }
      return;
    }
    try {
      const response = await generatedGetParticipant({ id }, { ...this.client, signal });
      if (this.disposed || signal.aborted) return;
      const person = response.data;
      if (!person) {
        this.summaries.set(key, 'failed');
        return;
      }
      const primary = person.identifiers?.find((identifier) => identifier.is_primary) ?? person.identifiers?.[0];
      this.summaries.set(key, {
        id,
        name: person.display_label || primary?.display_value || primary?.value || `Person ${id}`,
        address: primary?.display_value ?? primary?.value ?? '',
        lastAt: person.last_at ?? null,
        count: person.activity_count ?? 0,
        personID: person.profile?.id,
      });
    } catch {
      if (!this.disposed && !signal.aborted) this.summaries.set(key, 'failed');
    }
  }

  destroy(): void {
    this.disposed = true;
    this.lookupAbort.abort();
    clearTimeout(this.lookupTimer);
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
    context: DirectoryReviewContextSnapshot,
    displayedToken?: string,
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
    const reviewToken = displayedToken ?? this.rows.find((row) => row.id === candidateID)?.review_token;
    if (!reviewToken) {
      this.decisionRequests.delete(candidateID);
      this.pendingDecisions.delete(candidateID);
      const message = 'Refresh the review before deciding.';
      this.decisionError = message;
      return { ok: false, kind: 'error', status: 0, message };
    }
    const body = trimmedNotes ? { review_token: reviewToken, notes: trimmedNotes } : { review_token: reviewToken };
    try {
      const response =
        decision === 'accept'
          ? await generatedAcceptIdentityMatchCandidate({ id: candidateID }, body, {
              ...this.client,
              signal: abort.signal,
            })
          : await generatedRejectIdentityMatchCandidate({ id: candidateID }, body, {
              ...this.client,
              signal: abort.signal,
            });
      if (!this.ownsDecision(candidateID, abort)) {
        return { ok: false, kind: 'error', status: 0, message: 'Decision was superseded.' };
      }
      if (response.data) {
        const decidedCandidate = response.data.candidate;
        this.summaries.clear();
        this.staleDecision = response.data.cache_state === 'stale' ? decidedCandidate : undefined;
        this.decisionDrafts.delete(candidateID);
        if (!this.ownsDecisionContext(context)) {
          return { ok: true, candidate: decidedCandidate, cacheState: response.data.cache_state };
        }
        this.rows = replaceByID(this.rows, decidedCandidate);
        this.status = `Identity match ${decidedCandidate.state}.`;
        await this.loadIdentityPage(this.offset, this.identityState);
        return { ok: true, candidate: decidedCandidate, cacheState: response.data.cache_state };
      }
      const mergeConflict =
        decision === 'accept' && response.response.status === 409 ? validatePersonMergeRequired(response.error) : null;
      if (mergeConflict) {
        if (this.ownsDecisionContext(context)) this.mergeRequired = { candidateID, conflict: mergeConflict };
        return { ok: false, kind: 'merge_required', conflict: mergeConflict };
      }
      if (response.response.status === 409 && response.error?.error === 'identity_match_review_stale') {
        if (this.ownsDecisionContext(context)) await this.loadIdentityPage(context.offset, context.identityState);
        const message = 'The match changed. Review the refreshed evidence before deciding.';
        if (this.ownsDecisionContext(context)) this.decisionError = message;
        return { ok: false, kind: 'stale', status: 409, message };
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
    this.lookupQueue = [];
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
    ? rows.map((row) => (row.id === replacement.id ? replacement : row))
    : [...rows, replacement];
}
function failureMessage(error: unknown, status: number): string {
  if (typeof error === 'object' && error !== null && 'message' in error && typeof error.message === 'string') {
    return error.message;
  }
  if (error instanceof Error && error.message) return error.message;
  return status > 0 ? `Request failed (${status}).` : 'Request failed.';
}
