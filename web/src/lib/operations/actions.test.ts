import { describe, expect, it } from 'vitest';
import { createAPIClient } from '../api/client';
import { runOperationAction } from './actions';

describe('runOperationAction', () => {
  it.each(['partial', 'failed'])('reports contacts sync %s', async result => {
    const client = createAPIClient(async () => Response.json({ status: result, binding_conflicts: result === 'partial' ? 2 : 0, connections: [
      { connection: 'work', status: result, error_message: 'private arbitrary upstream detail' },
      ...(result === 'partial' ? [{ connection: 'personal', status: 'succeeded' }] : [])
    ] }));
    const action = await runOperationAction('carddav_sync', client);
    expect(action.outcome).toBe('failed');
    expect(action.message).toContain(`CardDAV sync ${result}. Connections needing attention: work.`);
    if (result === 'partial') expect(action.message).toContain('2 CardDAV binding conflicts. Check CardDAV settings for the review command.');
  });
  it('reports binding conflicts after a successful contacts sync', async () => {
    const client = createAPIClient(async () => Response.json({ status: 'succeeded', binding_conflicts: 2 }));
    expect(await runOperationAction('carddav_sync', client)).toEqual({ outcome: 'succeeded', message: '2 CardDAV binding conflicts. Check CardDAV settings for the review command.' });
  });
});
