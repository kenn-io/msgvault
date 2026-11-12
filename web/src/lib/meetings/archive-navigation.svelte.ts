import { createMeetingsAPI } from './api';
import type { APIClient } from '../api/client';
import { ArchiveMessageLoader } from '../archive/message-loader.svelte';
import type { MessageDetail } from '../api/generated/models';
export { archiveMeetingSelection, parseArchiveMeetingSelection } from './archive-selection';

/** Loads archive authority before navigation; no Explore row metadata is inferred. */
export class ArchiveMeetingNavigation {
  detail = $state<MessageDetail>();
  loading = $state(false);
  error = $state('');
  private readonly loader = new ArchiveMessageLoader({
    mismatchedID: 'This archived meeting is no longer available.',
    invalidConversation: 'The archived meeting conversation changed or is unavailable. Reload meeting activity.',
    missing: 'The archived meeting could not be loaded.',
    failed: 'The archived meeting could not be loaded.',
  }, (data) => {
    if (data.message_type !== 'meeting_transcript') return 'The selected archive item is no longer a meeting transcript.';
  });
  private generation = 0;

  constructor(private readonly client: APIClient) {}

  async load(id: number, conversationID?: number): Promise<MessageDetail | undefined> {
    this.cancel();
    const generation = this.generation;
    this.detail = undefined;
    this.error = '';
    this.loading = true;
    try {
      const data = await this.loader.load(this.client, id);
      if (generation !== this.generation) return;
      if (!data) throw new Error(this.loader.error || 'The archived meeting could not be loaded.');
      if (conversationID !== undefined && data.conversation_id !== conversationID) {
        throw new Error('The archived meeting conversation changed or is unavailable. Reload meeting activity.');
      }
      // Legacy detail.deleted_at records source deletion. Meeting scope excludes local hides.
      const metrics = await createMeetingsAPI(this.client).metrics({ scope: { message_ids: [id] } }, this.loader.signal);
      if (generation !== this.generation) return;
      if (metrics.totals.meeting_count !== 1) throw new Error('This archived meeting is no longer available.');
      this.detail = data;
      return data;
    } catch (cause: unknown) {
      if (generation === this.generation) this.error = cause instanceof Error ? cause.message : 'The archived meeting could not be loaded.';
    } finally {
      if (generation === this.generation) this.loading = false;
    }
  }

  cancel(): void {
    this.generation += 1;
    this.loader.cancel();
    this.loading = false;
  }
}
