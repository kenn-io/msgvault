import { describe, expect, it } from 'vitest';
import { createAPIClient } from '../api/client';
import { runOperationAction } from './actions';

describe('runOperationAction', () => {
  it.each(['partial', 'conflict'])('reports contacts sync %s', async result => {
    const client = createAPIClient(async () => Response.json(result === 'conflict' ? { error: 'operation_conflict', message: 'synthetic conflict' } : { status: result, connections: [
      { connection: 'work', status: result, error_message: 'private arbitrary upstream detail' }, { connection: 'personal', status: 'succeeded' }
    ] }, { status: result === 'conflict' ? 409 : 200 }));
    const action = await runOperationAction('carddav_sync', client);
    expect(action).toEqual(result === 'conflict'
      ? { outcome: 'conflict', message: 'The operation could not start because current server state changed.' }
      : { outcome: 'failed', message: 'CardDAV sync partial. Connections needing attention: work.' });
  });
});
