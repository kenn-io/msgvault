import { describe, expect, it } from 'vitest';
import { createAPIClient } from '../api/client';
import { runOperationAction } from './actions';

describe('runOperationAction', () => {
  it.each(['partial', 'failed'])('reports contacts sync %s', async result => {
    const client = createAPIClient(async () => Response.json({ status: result, connections: [
      { connection: 'work', status: result, error_message: 'private arbitrary upstream detail' }, { connection: 'personal', status: 'succeeded' }
    ] }));
    const action = await runOperationAction('carddav_sync', client);
    expect(action).toEqual({ outcome: 'failed', message: `CardDAV sync ${result}. Connections needing attention: work.` });
  });

  it('reports contacts sync conflict', async () => {
    const client = createAPIClient(async () => Response.json({ error: 'operation_conflict', message: 'synthetic conflict' }, { status: 409 }));
    const action = await runOperationAction('carddav_sync', client);
    expect(action).toEqual({ outcome: 'conflict', message: 'The operation could not start because current server state changed.' });
  });
});
