import { describe, expect, it } from 'vitest';
import { createAPIClient } from '../api/client';
import { runOperationAction } from './actions';

describe('runOperationAction', () => {
  it.each([
    {
      result: { status: 'partial', binding_conflicts: 2, connections: [
        { connection: 'work', status: 'partial', error_message: 'private arbitrary upstream detail' },
        { connection: 'personal', status: 'succeeded' }
      ] },
      message: 'CardDAV sync partial. Connections needing attention: work. 2 CardDAV binding conflicts. Check CardDAV settings for the review command.'
    },
    {
      result: { status: 'failed', binding_conflicts: 0, connections: [
        { connection: 'work', status: 'failed', error_message: 'private arbitrary upstream detail' }
      ] },
      message: 'CardDAV sync failed. Connections needing attention: work.'
    }
  ])('reports contacts sync $result.status', async ({ result, message }) => {
    const client = createAPIClient(async () => Response.json(result));
    const action = await runOperationAction('carddav_sync', client);
    expect(action).toEqual({ outcome: 'failed', message });
  });
  it('reports binding conflicts after a successful contacts sync', async () => {
    const client = createAPIClient(async () => Response.json({ status: 'succeeded', binding_conflicts: 2 }));
    expect(await runOperationAction('carddav_sync', client)).toEqual({ outcome: 'succeeded', message: '2 CardDAV binding conflicts. Check CardDAV settings for the review command.' });
  });
});
