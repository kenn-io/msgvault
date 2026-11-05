import {
  resumeVisualAttachmentBuild,
  startVisualAttachmentBuild,
  syncCardDAV,
} from '../api/generated/api/api';
import type { SyncResult } from '../api/generated/models';
import type { GeneratedRequestOptions } from '../api/runtime';
import type { OperationAction, OperationActionOutcome } from './models';

/** Shared requests and results for the actions advertised by operation lanes. */
export async function runOperationAction(action: OperationAction, client: GeneratedRequestOptions): Promise<{
  outcome: Exclude<OperationActionOutcome, 'discarded'>;
  message?: string;
}> {
  const result = action === 'carddav_sync'
    ? await syncCardDAV({ full: false }, client)
    : action === 'visual_build'
      ? await startVisualAttachmentBuild({ consent: true }, client)
      : await resumeVisualAttachmentBuild(client);
  if (result.response.status === 409) {
    return { outcome: 'conflict', message: 'The operation could not start because current server state changed.' };
  }
  if (!result.response.ok) return { outcome: 'failed', message: 'Unable to start the operation.' };
  if (action === 'carddav_sync' && result.data && 'status' in result.data &&
    (result.data.status === 'partial' || result.data.status === 'failed')) {
    const names = ((result.data as SyncResult).connections ?? [])
      .filter(connection => connection.status !== 'succeeded')
      .map(connection => connection.connection);
    return { outcome: 'failed', message: `CardDAV sync ${result.data.status}. Connections needing attention: ${names.join(', ')}.` };
  }
  return { outcome: 'succeeded' };
}
