import { describe, expect, it } from 'vitest';
import { createAPIClient } from '../api/client';
import { runOperationAction } from './actions';

describe('runOperationAction', () => {
  it.each(['partial', 'conflict'])('reports contacts sync %s', async result => {
    const client = createAPIClient(async () => Response.json({ status: result, connections: [
      { connection: 'work', status: result }, { connection: 'personal', status: 'succeeded' }
    ] }, { status: result === 'conflict' ? 409 : 200 }));
    expect(await runOperationAction('carddav_sync', client)).toEqual(result === 'conflict'
      ? { outcome: 'conflict', message: 'The operation could not start because current server state changed.' }
      : { outcome: 'failed', message: 'CardDAV sync partial. Connections needing attention: work.' });
  });
});
