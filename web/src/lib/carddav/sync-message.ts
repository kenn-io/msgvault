import { reconcileCardDAVParticipantBindings } from '../api/generated/api/api';
import type { GeneratedRequestOptions } from '../api/runtime';

export async function readCardDAVBindingConflicts(client: GeneratedRequestOptions): Promise<number> {
  try {
    const result = await reconcileCardDAVParticipantBindings({ dry_run: true, undo: false }, client);
    return result.response.ok ? result.data?.conflicts ?? 0 : 0;
  } catch {
    return 0;
  }
}
