import type { APIClient } from '../api/client';
import { getMessage } from '../api/generated/api/api';
import type { MessageDetail } from '../api/generated/models';

export class ArchiveMessageLoader {
  detail = $state<MessageDetail>();
  missing = $state(false);
  error = $state('');
  private controller: AbortController | undefined;

  async load(client: APIClient, id?: number): Promise<MessageDetail | undefined> {
    this.cancel();
    this.detail = undefined;
    this.missing = false;
    this.error = '';
    if (id === undefined) return;
    const controller = new AbortController();
    this.controller = controller;
    try {
      const { data, error, response } = await getMessage({ id }, { ...client, signal: controller.signal });
      if (controller.signal.aborted) return;
      if (response.status === 404 || (data && (data.id !== id || !Number.isSafeInteger(data.conversation_id) || data.conversation_id! < 1))) {
        this.missing = true;
        this.error = error?.message || 'Message not found.';
      } else if (!data) this.error = error?.message || 'Could not load this message.';
      else {
        this.detail = data;
        return data;
      }
    } catch {
      if (!controller.signal.aborted) this.error = 'Could not load this message.';
    }
  }

  get signal(): AbortSignal | undefined {
    return this.controller?.signal;
  }

  cancel(): void {
    this.controller?.abort();
  }
}
