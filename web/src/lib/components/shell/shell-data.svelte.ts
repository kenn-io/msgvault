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
      this.views = this.views.map(candidate => candidate.id === data.id ? data : candidate);
      await this.refresh();
      return undefined;
    } catch {
      return failure;
    }
  }

  /** Offers Undo for this view while committing the previous deletion. */
  async deleteView(view: SavedView): Promise<string | undefined> {
    const previous = this.pending;
    this.pending = view;
    this.views = this.views.filter(candidate => candidate.id !== view.id);
    if (previous) return this.commitDelete(previous);
  }

  async undoDelete(): Promise<void> {
    const view = this.pending;
    this.pending = undefined;
    if (view) await this.restoreView(view);
  }

  async expireDelete(): Promise<string | undefined> {
    const view = this.pending;
    this.pending = undefined;
    if (view) return this.commitDelete(view);
  }

  private async restoreView(view: SavedView): Promise<void> {
    if (!this.views.some(candidate => candidate.id === view.id)) this.views = [...this.views, view];
    await this.refresh();
  }

  private async commitDelete(view: SavedView): Promise<string | undefined> {
    try {
      const { response } = await deleteSavedView(
        { id: view.id },
        { ...this.#client, keepalive: true, headers: { 'If-Match': `"saved-view-${view.id}-r${view.revision}"` } },
      );
      if (!response.ok) {
        await this.restoreView(view);
        return `Couldn't delete ${view.name}.`;
      }
      this.#controller?.abort();
      this.views = this.views.filter(candidate => candidate.id !== view.id);
    } catch {
      await this.restoreView(view);
      return `Couldn't delete ${view.name}.`;
    }
  }
}
