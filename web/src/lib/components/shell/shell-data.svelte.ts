import {
  deleteSavedView,
  listSavedViews,
  patchSavedView,
} from '../../api/generated/api/api';
import type { APIClient } from '../../api/client';
import { OperationsController } from '../../operations/controller.svelte';
import { JobsStore } from '../../jobs/jobs.svelte';
import type { SavedView, PatchSavedViewRequest } from '../../api/generated/models';
import type { ExploreURLState } from '../../explore/models';
import { supportsSavedViewSchema, exploreStateFromSavedView, type CanonicalState } from '../../saved-views/canonical';

import { DeletionsController } from '../../deletions/controller.svelte';

const REFRESH_MS = 30_000;
export function viewIncompatibility(view: SavedView): string {
  if (!supportsSavedViewSchema(view.schema_version)) {
    return `This view uses schema version ${view.schema_version}. Automatic migration is not supported; remove it and save the current view again.`;
  }
  return view.incompatibility_reason ?? '';
}

export function viewState(view: SavedView): Partial<ExploreURLState> {
  return exploreStateFromSavedView(view.canonical_state as CanonicalState, view.schema_version);
}

/** Saved views and live jobs shown in the shell. */
export class ShellData {
  #ordered = $state<SavedView[]>([]);
  #deleting = $state<number[]>([]);
  get views(): SavedView[] {
    return this.#ordered.filter(view => view.id !== this.pending?.id && !this.#deleting.includes(view.id));
  }
  pending = $state<SavedView>();
  error = $state<string>();
  readonly deletions: DeletionsController;
  get deletionsStaged(): boolean { return this.deletions.visible; }
  readonly jobs: JobsStore;

  #client: APIClient;
  #timer: ReturnType<typeof setInterval> | undefined;
  #controller: AbortController | undefined;

  constructor(client: APIClient, operations = new OperationsController(client)) {
    this.#client = client;
    this.deletions = new DeletionsController(client);
    this.jobs = new JobsStore(client, operations);
  }

  /** Jobs poll while visible; saved views refresh on entry, focus, and writes. */
  start(): void {
    this.#timer = setInterval(() => {
      if (typeof document === 'undefined' || document.visibilityState === 'visible') { void this.jobs.refresh(); }
    }, REFRESH_MS);
  }

  stop(): void {
    if (this.#timer !== undefined) clearInterval(this.#timer);
    this.#timer = undefined;
    this.#controller?.abort();
    this.deletions.destroy();
    this.jobs.dispose();
  }

  async refresh(): Promise<void> {
    this.#controller?.abort();
    const controller = (this.#controller = new AbortController());
    const client = { ...this.#client, signal: controller.signal };
    const deletions = this.refreshDeletions(true);
    const views = await this.#loadViews(client);
    if (controller.signal.aborted) return;
    this.error = views ? undefined : "Couldn't load saved views.";
    if (views) this.#ordered = views;
    await deletions;
  }

  refreshDeletions(retry = false): Promise<void> { return this.deletions.refresh(retry); }

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
    if (!trimmed || trimmed === view.name) return trimmed ? undefined : 'Enter a name.';
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
          return 'This view changed in another session. Cancel this edit and try again.';
        }
        return failure;
      }
      this.#controller?.abort();
      this.#ordered = this.#ordered.map(candidate => candidate.id === data.id ? data : candidate);
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
    if (!this.#ordered.some(candidate => candidate.id === view.id)) this.#ordered = [...this.#ordered, view];
    await this.refresh();
  }

  private async commitDelete(view: SavedView): Promise<string | undefined> {
    this.#deleting = [...this.#deleting, view.id];
    try {
      const { response } = await deleteSavedView(
        { id: view.id },
        { ...this.#client, keepalive: true, headers: { 'If-Match': `"saved-view-${view.id}-r${view.revision}"` } },
      );
      if (!response.ok) throw new Error(`DELETE ${response.status}`);
      this.#controller?.abort();
      this.#ordered = this.#ordered.filter(candidate => candidate.id !== view.id);
      await this.refresh();
    } catch {
      this.#deleting = this.#deleting.filter(id => id !== view.id);
      await this.restoreView(view);
      return `Couldn't delete ${view.name}.`;
    } finally {
      this.#deleting = this.#deleting.filter(id => id !== view.id);
    }
  }
}
