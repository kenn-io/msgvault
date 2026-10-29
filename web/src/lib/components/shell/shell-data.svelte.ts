import {
  deleteSavedView,
  listSavedViews,
  patchSavedView,
} from '../../api/generated/api/api';
import type { APIClient } from '../../api/client';
import { JobsStore } from '../../jobs/jobs.svelte';
import type { SavedView, PatchSavedViewRequest } from '../../api/generated/models';
import type { ExploreURLState } from '../../explore/models';
import { CURRENT_SCHEMA_VERSION, exploreStateFromSavedView, type CanonicalState } from '../../saved-views/canonical';

const REFRESH_MS = 30_000;
export function viewIncompatibility(view: SavedView): string {
  if (view.schema_version < 1 || view.schema_version > CURRENT_SCHEMA_VERSION) {
    return `This view uses schema version ${view.schema_version}. Automatic migration is not supported; remove it and save the current view again.`;
  }
  return view.incompatibility_reason ?? '';
}

export function viewIsFiles(view: SavedView): boolean {
  return (view.canonical_state as CanonicalState | undefined)?.presentation === 'files';
}

export function viewState(view: SavedView): Partial<ExploreURLState> {
  return exploreStateFromSavedView(view.canonical_state as CanonicalState, view.schema_version);
}

/** Saved views and live jobs shown in the shell. */
export class ShellData {
  views = $state<SavedView[]>([]);
  pending = $state<SavedView>();
  error = $state<string>();
  readonly jobs: JobsStore;

  #client: APIClient;
  #timer: ReturnType<typeof setInterval> | undefined;
  #controller: AbortController | undefined;

  constructor(client: APIClient) {
    this.#client = client;
    this.jobs = new JobsStore(client);
  }

  /** Refreshes on a timer. The shell also refreshes on every page change, including the first. */
  start(): void {
    this.#timer = setInterval(() => {
      if (typeof document === 'undefined' || document.visibilityState === 'visible') void this.refresh();
    }, REFRESH_MS);
  }

  stop(): void {
    if (this.#timer !== undefined) clearInterval(this.#timer);
    this.#timer = undefined;
    this.#controller?.abort();
    this.jobs.dispose();
  }

  async refresh(): Promise<void> {
    this.#controller?.abort();
    const controller = (this.#controller = new AbortController());
    const client = { ...this.#client, signal: controller.signal };
    void this.jobs.refresh();
    const views = await this.#loadViews(client);
    if (controller.signal.aborted) return;
    this.error = views ? undefined : "Couldn't load saved views.";
    if (views) this.views = views.filter(view => view.id !== this.pending?.id);
  }

  async #loadViews(client: APIClient): Promise<SavedView[] | undefined> {
    try {
      const { data } = await listSavedViews(client);
      return data?.saved_views ?? undefined;
    } catch {
      return undefined;
    }
  }

  /** Renames one saved view, returning an error message on failure. */
  async renameView(view: SavedView, name: string): Promise<string | undefined> {
    const trimmed = name.trim();
    if (!trimmed || trimmed === view.name) return undefined;
    return this.#patch(view, { name: trimmed }, `Couldn't rename ${view.name}.`);
  }

  async describeView(view: SavedView, description: string): Promise<string | undefined> {
    const trimmed = description.trim();
    if (trimmed === (view.description ?? '')) return undefined;
    return this.#patch(view, { description: trimmed }, "Couldn't save the description.");
  }

  async #patch(view: SavedView, body: PatchSavedViewRequest, failure: string): Promise<string | undefined> {
    try {
      const { data, response } = await patchSavedView(
        { id: view.id }, body,
        { ...this.#client, headers: { 'If-Match': `"saved-view-${view.id}-r${view.revision}"` } },
      );
      if (!data) {
        if (response.status === 409) {
          void this.refresh();
          return 'This view changed in another session.';
        }
        return failure;
      }
      this.#controller?.abort();
      this.views = this.views.map(candidate => candidate.id === data.id ? data : candidate).sort(viewOrder);
      return undefined;
    } catch {
      return failure;
    }
  }

  /** Commits the previous deletion before offering Undo for this view. */
  async deleteView(view: SavedView): Promise<string | undefined> {
    const error = await this.expireDelete();
    this.pending = view;
    this.views = this.views.filter(candidate => candidate.id !== view.id);
    return error;
  }

  undoDelete(): void {
    if (this.pending) this.restoreView(this.pending);
    this.pending = undefined;
  }

  async expireDelete(): Promise<string | undefined> {
    const view = this.pending;
    this.pending = undefined;
    if (view) return this.commitDelete(view);
  }

  private restoreView(view: SavedView): void {
    if (this.views.some(candidate => candidate.id === view.id)) return;
    this.views = [...this.views, view].sort(viewOrder);
  }

  private async commitDelete(view: SavedView): Promise<string | undefined> {
    try {
      const { response } = await deleteSavedView(
        { id: view.id },
        { ...this.#client, keepalive: true, headers: { 'If-Match': `"saved-view-${view.id}-r${view.revision}"` } },
      );
      if (!response.ok) {
        this.restoreView(view);
        if (response.status === 409) void this.refresh();
        return `Couldn't delete ${view.name}.`;
      }
      this.#controller?.abort();
      this.views = this.views.filter(candidate => candidate.id !== view.id);
    } catch {
      this.restoreView(view);
      return `Couldn't delete ${view.name}.`;
    }
  }
}

function viewOrder(a: SavedView, b: SavedView): number {
  const left = a.name.toLowerCase();
  const right = b.name.toLowerCase();
  return left < right ? -1 : left > right ? 1 : a.id - b.id;
}
