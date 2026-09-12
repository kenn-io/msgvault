import { createMeetingsAPI } from './api';
import type { APIClient } from '../api/client';
import { getMessage } from '../api/generated/api/api';
import type { MessageDetail } from '../api/generated/models';
export { archiveMeetingSelection, parseArchiveMeetingSelection } from './archive-selection';

/** Loads archive authority before navigation; no Explore row metadata is inferred. */
export class ArchiveMeetingNavigation {
  detail = $state<MessageDetail>();
  loading = $state(false);
  error = $state('');
  private controller: AbortController | undefined;
  private generation = 0;

  constructor(private readonly client: APIClient) {}

  async load(id: number, conversationID?: number): Promise<MessageDetail | undefined> {
    this.cancel();
    const generation = this.generation;
    const controller = new AbortController();
    this.controller = controller;
    this.detail = undefined;
    this.error = '';
    this.loading = true;
    try {
      const { data, error } = await getMessage({ id }, { ...this.client, signal: controller.signal });
      if (generation !== this.generation || controller.signal.aborted) return;
      if (!data) {
        throw new Error(error && typeof error === 'object' && 'message' in error && typeof error.message === 'string'
          ? error.message : 'The archived meeting could not be loaded.');
      }
      if (data.id !== id) throw new Error('This archived meeting is no longer available.');
      if (data.message_type !== 'meeting_transcript') throw new Error('The selected archive item is no longer a meeting transcript.');
      if (!Number.isSafeInteger(data.conversation_id) || data.conversation_id! < 1 || (conversationID !== undefined && data.conversation_id !== conversationID)) {
        throw new Error('The archived meeting conversation changed or is unavailable. Reload meeting activity.');
      }
      // Legacy detail.deleted_at records source deletion. Meeting scope excludes local hides.
      const metrics = await createMeetingsAPI(this.client).metrics({ scope: { message_ids: [id] } }, controller.signal);
      if (generation !== this.generation || controller.signal.aborted) return;
      if (metrics.totals.meeting_count !== 1) throw new Error('This archived meeting is no longer available.');
      this.detail = data;
      return data;
    } catch (cause: unknown) {
      if (generation === this.generation && !controller.signal.aborted) this.error = cause instanceof Error ? cause.message : 'The archived meeting could not be loaded.';
    } finally {
      if (generation === this.generation) this.loading = false;
    }
  }

  cancel(): void {
    this.generation += 1;
    this.controller?.abort();
    this.loading = false;
  }
}
