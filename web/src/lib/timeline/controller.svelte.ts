import type { GetTimelineParams, TimelineCandidate, TimelineResult } from '../api/generated/models';
import type { APIClient } from '../api/client';
import { DirectoryController } from '../directory/controller.svelte';
import { readTimeline, TimelineRequestError } from './api';

export type TimelineTab = 'day' | 'person' | 'around';

export class TimelineController {
  tab = $state<TimelineTab>('day');
  date = $state('');
  personQuery = $state('');
  anchor = $state('');
  before = $state(5);
  after = $state(5);
  result = $state<TimelineResult | null>(null);
  candidates = $state<TimelineCandidate[]>([]);
  loading = $state(false);
  error = $state<string | null>(null);
  readonly directory: DirectoryController;
  private abort: AbortController | undefined;
  private generation = 0;
  private request: GetTimelineParams = { mode: 'day' };

  constructor(private readonly client: APIClient) {
    this.directory = new DirectoryController(client);
  }

  async selectTab(tab: TimelineTab): Promise<void> {
    this.cancel();
    this.tab = tab;
    this.result = null;
    this.candidates = [];
    this.error = null;
    if (tab === 'day') await this.day();
    if (tab === 'person') await this.searchPeople();
  }

  day(): Promise<void> {
    return this.load({ mode: 'day', ...(this.date ? { date: this.date } : {}) });
  }

  searchPeople(): Promise<void> {
    this.directory.query = this.personQuery;
    return this.directory.loadFirstPage();
  }

  person(reference = this.personQuery): Promise<void> {
    this.tab = 'person';
    return this.load({ mode: 'relationship', with: reference.trim() });
  }

  chooseCandidate(candidate: TimelineCandidate): Promise<void> {
    const reference = candidate.person_id > 0 ? `person:${candidate.person_id}` :
      candidate.participant_ids[0] ? `participant:${candidate.participant_ids[0]}` : candidate.address;
    return this.person(reference);
  }

  around(reference = this.anchor): Promise<void> {
    this.tab = 'around';
    this.anchor = reference;
    return this.load({ mode: 'anchor', around: reference, before: this.before, after: this.after });
  }

  async load(request: GetTimelineParams): Promise<void> {
    this.abort?.abort();
    const abort = new AbortController();
    this.abort = abort;
    const generation = ++this.generation;
    this.request = { ...request };
    this.loading = true;
    this.result = null;
    this.candidates = [];
    this.error = null;
    try {
      const result = await readTimeline(this.client, request, abort.signal);
      if (abort.signal.aborted || generation !== this.generation) return;
      this.result = result;
    } catch (cause: unknown) {
      if (abort.signal.aborted || generation !== this.generation) return;
      if (cause instanceof TimelineRequestError && cause.candidates.length > 0) this.candidates = cause.candidates;
      else this.error = cause instanceof Error ? cause.message : 'Could not load the timeline.';
    } finally {
      if (generation === this.generation) this.loading = false;
    }
  }

  nextPage(): Promise<void> {
    const cursor = this.result?.cursor;
    return cursor && !this.loading ? this.load({ ...this.request, cursor }) : Promise.resolve();
  }

  cancel(): void {
    ++this.generation;
    this.abort?.abort();
    this.directory.destroy();
    this.loading = false;
  }

  destroy(): void { this.cancel(); }
}
