import { ENDPOINT_NAMES, endpointKey, type EndpointSummary } from '../people/duplicates';
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
import type { IdentityReviewState } from '../explore/models';
import {
  validatePersonMergeRequired,
  type PersonMergeSuccess,
  type ValidatedPersonMergeRequired,
} from './person-merge';
export const IDENTITY_REVIEW_PAGE_LIMIT = 10;
export type IdentityMatchCandidate = GeneratedIdentityMatchCandidate;
export type PersonMergeRequiredError = ValidatedPersonMergeRequired;
type ReviewCommit = (patch: {
  identityState?: IdentityReviewState;
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
  identityState = $state<IdentityReviewState>('candidate');
  rows = $state<IdentityMatchCandidate[]>([]);
  offset = $state(0);
  private moreRows = $state(false);
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
  private readonly onSettled: (result: IdentityDecisionResult) => void | Promise<void>;
  private settlement: Promise<IdentityDecisionResult | undefined> | undefined;
  pending = $state<{ candidate: IdentityMatchCandidate; decision: 'accept' | 'reject'; context: DirectoryReviewContextSnapshot; notes: string }>();
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
  constructor(client: APIClient, commit: ReviewCommit = () => undefined, onSettled: (result: IdentityDecisionResult) => void | Promise<void> = () => undefined) {
    this.client = client;
    this.onSettled = onSettled;
    this.commit = commit;
  }
  get hasPreviousPage(): boolean {
    return this.offset > 0;
  }
  get hasNextPage(): boolean {
    return this.moreRows;
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
  reviewContextSnapshot(): DirectoryReviewContextSnapshot {
    return {
      generation: this.reviewContextGeneration,
      identityState: this.identityState,
      offset: this.offset,
    };
  }
  isReviewContextCurrent(context: DirectoryReviewContextSnapshot): boolean {
    return (
      !this.disposed &&
      context.generation === this.reviewContextGeneration &&
      context.identityState === this.identityState &&
      context.offset === this.offset
    );
  }
  applyURLState(state: ReviewURLState, historyRestoration = false): void {
    const unchanged = this.initialized && this.identityState === state.identityState;
    if (this.disposed || (unchanged && !historyRestoration)) return;
    this.initialized = true;
    if (historyRestoration) this.invalidateSummaries();
    this.resetIdentityContext(state.identityState);
    void this.loadIdentityPage(0, state.identityState);
  }
  async setIdentityState(identityState: IdentityReviewState): Promise<void> {
    if (this.identityState === identityState && this.initialized) return;
    const generation = this.reviewContextGeneration;
    const result = this.pending ? await this.settleDecision() : undefined;
    if (generation !== this.reviewContextGeneration) return;
    this.commit({ identityState });
    this.resetIdentityContext(identityState);
    if (result && !result.ok && result.kind !== 'merge_required') this.decisionError = result.message;
    void this.loadIdentityPage(0, identityState);
  }
  async loadIdentityPage(
    targetOffset = this.offset,
    state: IdentityReviewState = this.identityState,
  ): Promise<boolean> {
    if (this.disposed) return false;
    const contextChanged = this.identityState !== state;
    if (contextChanged) {
      this.resetIdentityContext(state);
      targetOffset = 0;
    }
    this.initialized = true;
    this.identityState = state;
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
        { state: state === 'accepted' || state === 'rejected' ? 'accepted,rejected' : 'candidate,conflict', limit: IDENTITY_REVIEW_PAGE_LIMIT + 1, offset: targetOffset },
        {
          ...this.client,
          signal: abort.signal,
        },
      );
      if (!this.ownsPage(abort, generation)) return false;
      if (response.data) {
        if (targetOffset > 0 && !response.data.candidates?.length) return await this.loadIdentityPage(0, state);
        this.moreRows = (response.data.candidates?.length ?? 0) > IDENTITY_REVIEW_PAGE_LIMIT;
        this.rows = (response.data.candidates ?? []).slice(0, IDENTITY_REVIEW_PAGE_LIMIT);
        [this.offset, this.lastMerge] = [response.data.offset, response.data.offset === this.offset ? this.lastMerge : null];
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
    await this.loadPageAfterSettlement(this.offset + this.rows.length, this.rows.filter(row => this.hidden.has(row.id)).length);
  }
  async loadPreviousPage(): Promise<void> {
    if (this.loading || !this.hasPreviousPage) return;
    await this.loadPageAfterSettlement(Math.max(0, this.offset - IDENTITY_REVIEW_PAGE_LIMIT));
  }
  async retryPage(targetOffset = this.retryOffset ?? this.offset): Promise<void> {
    await this.loadPageAfterSettlement(targetOffset);
  }
  /** A page move made before a decision settles yields to any newer review context. */
  private async loadPageAfterSettlement(targetOffset: number, hiddenRows = 0): Promise<void> {
    const context = this.reviewContextSnapshot();
    const result = await this.settleDecision();
    if (this.isReviewContextCurrent(context)) await this.loadIdentityPage(targetOffset - (result?.ok ? hiddenRows : 0));
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
    success: PersonMergeSuccess,
  ): Promise<void> {
    if (this.disposed) return;
    this.lastMerge = { candidateID, ...success };
    this.invalidateSummaries();
    if (this.mergeRequired?.candidateID === candidateID) this.mergeRequired = null;
    const name = success.survivor.display_name?.trim() || `Person ${success.survivor.id}`;
    this.status = `Profiles merged into ${name}. Review the refreshed pair to finish. Identity cache ${success.result.cache_state}.`;
    await this.loadIdentityPage(this.offset, this.identityState);
  }
  get submitting(): boolean { return this.loading || this.pendingDecisions.size > 0 || this.mergeRequired !== null; }
  async queueDecision(candidate: IdentityMatchCandidate, decision: 'accept' | 'reject'): Promise<void> {
    if (this.pending || this.settlement) await this.settleDecision();
    if (this.disposed || this.submitting) return;
    this.hidden.add(candidate.id);
    this.pending = { candidate: { ...candidate }, decision, context: this.reviewContextSnapshot(), notes: this.getDecisionDraft(candidate.id) };
  }
  undoDecision(): void {
    if (this.pending) this.hidden.delete(this.pending.candidate.id);
    this.pending = undefined;
  }
  settleDecision(): Promise<IdentityDecisionResult | undefined> {
    if (this.settlement) return this.settlement;
    const pending = this.pending;
    if (!pending) return Promise.resolve(undefined);
    this.pending = undefined;
    const { candidate, decision, context, notes } = pending;
    this.settlement = this.decideIdentity(candidate.id, decision, notes, context, candidate.review_token).finally(() => { this.settlement = undefined; });
    return this.settlement;
  }
  async invalidate(active: boolean): Promise<void> {
    this.invalidateSummaries();
    this.resetIdentityContext(this.identityState);
    this.initialized = false;
    if (active) await this.settleDecision().then(() => this.loadIdentityPage(0));
  }

  retrySummary(key: string): void { this.summaries.delete(key); this.requestSummaries(); }
  /** Summaries describe whole clusters, so any identity change outdates them all. */
  private invalidateSummaries(): void { this.summaries.clear(); this.lookupAbort.abort(); }
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
          this.summaries.set(key, { id, name: ENDPOINT_NAMES[kind] ?? 'Unknown', address: candidate.normalized_value ?? '', lastAt: null });
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
        this.summaries.set(key, response.data ? { id, name: name || `Person ${id}`, address: 'Saved person', lastAt: null } : 'failed');
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
      const identifiers = person.identifiers?.filter(identifier => identifier.participant_id === id);
      const primary = identifiers?.find(identifier => identifier.is_primary) ?? identifiers?.[0];
      const member = person.cluster?.members?.find(member => member.participant_id === id);
      this.summaries.set(key, {
        id,
        name: primary?.participant_display_name || member?.display_name || (person.id === id ? person.display_label : '') || primary?.display_value || primary?.value || `Person ${id}`,
        address: primary?.value || member?.email || member?.phone || '',
        lastAt: person.last_at ?? null,
        count: person.activity_count ?? 0,
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
    if (!displayedToken && !this.isReviewContextCurrent(context)) {
      return { ok: false, kind: 'error', status: 0, message: 'The review context changed.' };
    }
    if (this.disposed || this.submitting) {
      return { ok: false, kind: 'error', status: 0, message: 'A decision is already pending.' };
    }
    if (notes !== undefined) this.setDecisionDraft(candidateID, notes);
    this.decisionError = null;
    this.status = null;
    const abort = new AbortController();
    this.decisionRequests.set(candidateID, abort);
    this.pendingDecisions.add(candidateID);
    const trimmedNotes = this.getDecisionDraft(candidateID).trim();
    const reviewToken = displayedToken ?? this.rows.find((row) => row.id === candidateID)?.review_token;
    let result: IdentityDecisionResult | undefined;
    try {
      if (!reviewToken) return result = { ok: false, kind: 'error', status: 0, message: 'Refresh the review before deciding.' };
      const body = trimmedNotes ? { review_token: reviewToken, notes: trimmedNotes } : { review_token: reviewToken };
      const response =
        decision === 'accept'
          ? await generatedAcceptIdentityMatchCandidate({ id: candidateID }, body, {
              ...this.client,
              signal: abort.signal,
              keepalive: true,
            })
          : await generatedRejectIdentityMatchCandidate({ id: candidateID }, body, {
              ...this.client,
              signal: abort.signal,
              keepalive: true,
            });
      if (!this.ownsDecision(candidateID, abort)) {
        return result = { ok: false, kind: 'error', status: 0, message: 'Decision was superseded.' };
      }
      if (response.data ? decision === 'accept' : response.error?.error === 'identity_match_review_stale') this.invalidateSummaries();
      if (response.data) {
        const decidedCandidate = response.data.candidate;
        this.decisionDrafts.delete(candidateID);
        if (this.ownsDecisionContext(context)) {
          this.rows = this.rows.map(row => row.id === candidateID ? decidedCandidate : row);
          this.status = `Identity match ${decidedCandidate.state}.${response.data.cache_state === 'stale' ? ' Identity cache refresh failed.' : ''}`;
        }
        result = { ok: true, candidate: decidedCandidate, cacheState: response.data.cache_state };
        await Promise.resolve().then(() => this.onSettled(result!)).catch(() => undefined);
        await this.loadIdentityPage(this.offset, this.identityState);
        return result;
      }
      const mergeConflict =
        decision === 'accept' && response.response.status === 409 ? validatePersonMergeRequired(response.error) : null;
      if (mergeConflict) {
        this.mergeRequired = { candidateID, conflict: mergeConflict };
        return result = { ok: false, kind: 'merge_required', conflict: mergeConflict };
      }
      if (response.response.status === 409 && response.error?.error === 'identity_match_review_stale') {
        await this.loadIdentityPage(this.offset, this.identityState);
        const message = 'The match changed. Review the refreshed evidence before deciding.';
        return result = { ok: false, kind: 'stale', status: 409, message };
      }
      const message = failureMessage(response.error, response.response.status);
      return result = { ok: false, kind: 'error', status: response.response.status, message };
    } catch (cause: unknown) {
      if (!this.ownsDecision(candidateID, abort)) {
        return result = { ok: false, kind: 'error', status: 0, message: 'Decision was superseded.' };
      }
      const message = failureMessage(cause, 0);
      return result = { ok: false, kind: 'error', status: 0, message };
    } finally {
      if (this.ownsDecision(candidateID, abort)) {
        this.decisionRequests.delete(candidateID);
        this.pendingDecisions.delete(candidateID);
        this.hidden.delete(candidateID);
        if (result) {
          if (!result.ok && result.kind !== 'merge_required') this.decisionError = result.message;
          if (!result.ok) await this.onSettled(result);
        }
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
    this[this.rows.length > 0 ? 'pageError' : 'error'] = message;
    this.requestSummaries();
  }
  private resetIdentityContext(identityState: IdentityReviewState): void {
    ++this.reviewContextGeneration;
    this.pageAbort?.abort();
    this.pageAbort = undefined;
    ++this.pageGeneration;
    this.identityState = identityState;
    this.rows = [];
    this.moreRows = false;
    this.lookupQueue = [];
    this.offset = 0;
    this.loading = false;
    this.error = null;
    this.pageError = null;
    this.decisionError = null;
    this.status = this.lastMerge = null;
    this.retryOffset = undefined;
  }
}
function failureMessage(error: unknown, status: number): string {
  if (typeof error === 'object' && error !== null && 'message' in error && typeof error.message === 'string') {
    return error.message;
  }
  if (error instanceof Error && error.message) return error.message;
  return status > 0 ? `Request failed (${status}).` : 'Request failed.';
}
