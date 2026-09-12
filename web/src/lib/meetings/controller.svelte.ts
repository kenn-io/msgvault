import type { ActionsPage, MeetingActionsRequest, MeetingExploreScope, MeetingScopeRequest, Metrics } from '../api/generated/models';
import { canonicalFingerprint } from '../explore/selection';
import { MeetingsAPIError, type MeetingsAPI } from './api';

/** The tag makes dropping Explore authority in favor of a direct scope impossible. */
export type MeetingPanelScope =
  | { kind: 'direct'; scope: MeetingScopeRequest }
  | { kind: 'explore'; explore: MeetingExploreScope };
export interface MeetingActionFilters {
  status?: MeetingActionsRequest['status'];
  assigneeEmail?: string;
}
export interface MeetingLoadError {
  message: string;
  recovery: 'reload' | 'narrow' | 'unavailable' | 'retry';
}

function normalizedScope(scope: MeetingPanelScope): MeetingPanelScope {
  if (scope.kind === 'explore') return scope;
  const normalized = { ...scope.scope };
  for (const key of ['message_ids', 'source_ids', 'participant_ids'] as const) {
    if (normalized[key]) normalized[key] = [...new Set(normalized[key])].sort((a, b) => a - b);
  }
  if (normalized.domains) normalized.domains = [...new Set(normalized.domains.map((domain) => domain.trim().toLowerCase()))].sort();
  return { kind: 'direct', scope: normalized };
}

export function meetingLoadError(cause: unknown): MeetingLoadError {
  if (cause instanceof MeetingsAPIError) {
    if (cause.code === 'meeting_scope_too_large' || cause.code === 'candidate_pool_saturated' || cause.code === 'search_candidate_pool_saturated') {
      return { recovery: 'narrow', message: 'This meeting scope is incomplete or too large. Choose narrower filters to see complete meeting activity.' };
    }
    if (cause.status === 409 || cause.code === 'candidate_snapshot_expired') {
      return { recovery: 'reload', message: `${cause.message} Reload meeting activity to use the current scope.` };
    }
    if (cause.status === 404 && cause.code === 'not_found') {
      return { recovery: 'unavailable', message: 'This daemon does not support meeting activity. Update the daemon to load it.' };
    }
    if (cause.code === 'cache_unavailable') {
      return { recovery: 'unavailable', message: cause.message };
    }
  }
  return { recovery: 'retry', message: cause instanceof Error ? cause.message : 'Meeting activity could not be loaded.' };
}

/** Owns action requests for both activity panels and standalone readers. */
export class MeetingActionsController {
  page = $state<ActionsPage>();
  loading = $state(false);
  error = $state<MeetingLoadError>();
  private request: MeetingActionsRequest | undefined;
  private generation = 0;
  private requestController: AbortController | undefined;

  constructor(private readonly api: Pick<MeetingsAPI, 'actions'>) {}

  async load(request: MeetingActionsRequest | undefined): Promise<void> {
    this.request = request;
    const generation = ++this.generation;
    this.requestController?.abort();
    const controller = new AbortController();
    this.requestController = controller;
    this.page = undefined;
    this.error = undefined;
    this.loading = request !== undefined;
    if (!request) return;
    try {
      const page = await this.api.actions(request, controller.signal);
      if (generation === this.generation && !controller.signal.aborted) this.page = page;
    } catch (cause: unknown) {
      if (generation === this.generation && !controller.signal.aborted) this.error = meetingLoadError(cause);
    } finally {
      if (generation === this.generation) this.loading = false;
    }
  }

  async loadMore(): Promise<void> {
    const cursor = this.page?.next_cursor;
    if (!cursor || !this.request || this.loading) return;
    const generation = this.generation;
    const signal = this.requestController?.signal;
    this.loading = true;
    this.error = undefined;
    try {
      const next = await this.api.actions({ ...this.request, cursor }, signal);
      if (generation !== this.generation || signal?.aborted) return;
      // Source edits can move an action across page boundaries. Keep its first
      // visible position, replacing the entire row with the latest evidence.
      const rows = new Map([...(this.page?.rows ?? []), ...next.rows]
        .map((row) => [`${row.meeting.message_id}:${row.action.locator}`, row] as const));
      this.page = { ...next, rows: [...rows.values()] };
    } catch (cause: unknown) {
      if (generation !== this.generation || signal?.aborted) return;
      this.error = meetingLoadError(cause);
    } finally {
      if (generation === this.generation) this.loading = false;
    }
  }

  destroy(): void {
    this.generation += 1;
    this.requestController?.abort();
  }
}

export class MeetingsController {
  metrics = $state<Metrics>();
  private readonly actionPages: MeetingActionsController;
  get actions(): ActionsPage | undefined { return this.actionPages.page; }
  get actionsLoading(): boolean { return this.actionPages.loading; }
  get actionsError(): MeetingLoadError | undefined { return this.actionPages.error; }
  metricsLoading = $state(false);
  metricsError = $state<MeetingLoadError>();
  filters = $state<MeetingActionFilters>({});
  private scope: MeetingPanelScope | undefined;
  private scopeFingerprint = '';
  private generation = 0;
  private requestController: AbortController | undefined;

  constructor(private readonly api: MeetingsAPI) {
    this.actionPages = new MeetingActionsController(api);
  }

  async setScope(scope: MeetingPanelScope, refreshKey = ''): Promise<void> {
    const normalized = normalizedScope(scope);
    const fingerprint = canonicalFingerprint({ scope: normalized, refreshKey });
    if (fingerprint === this.scopeFingerprint) return;
    this.scopeFingerprint = fingerprint;
    this.scope = normalized;
    await this.reload();
  }

  async setFilters(filters: MeetingActionFilters): Promise<void> {
    const normalized = { ...filters, assigneeEmail: filters.assigneeEmail?.trim().toLowerCase() || undefined };
    if (canonicalFingerprint(normalized) === canonicalFingerprint(this.filters)) return;
    this.filters = normalized;
    await this.reload();
  }

  async reload(): Promise<void> {
    const scope = this.scope;
    if (!scope) return;
    const generation = ++this.generation;
    this.requestController?.abort();
    const controller = new AbortController();
    this.requestController = controller;
    this.metrics = undefined;
    this.metricsError = undefined;
    this.metricsLoading = true;
    const request = scope.kind === 'direct' ? { scope: scope.scope } : { explore: scope.explore };
    const current = () => generation === this.generation && !controller.signal.aborted;
    await Promise.all([
      this.api.metrics(request, controller.signal).then((metrics) => {
        if (current()) this.metrics = metrics;
      }).catch((cause: unknown) => {
        if (current()) this.metricsError = meetingLoadError(cause);
      }).finally(() => { if (current()) this.metricsLoading = false; }),
      this.actionPages.load(this.actionRequest())
    ]);
  }

  async loadMore(): Promise<void> {
    if (!this.actionsError) await this.actionPages.loadMore();
  }

  private actionRequest(): MeetingActionsRequest {
    const scope = this.scope;
    return { ...(scope?.kind === 'direct' ? { scope: scope.scope } : { explore: scope?.explore }), limit: 50,
      ...(this.filters.status ? { status: this.filters.status } : {}),
      ...(this.filters.assigneeEmail ? { assignee_email: this.filters.assigneeEmail } : {}) };
  }

  destroy(): void {
    this.generation += 1;
    this.requestController?.abort();
    this.actionPages.destroy();
  }
}
