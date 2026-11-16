import { describe, expect, it } from 'vitest';
import { createAPIClient } from '../api/client';
import { runOperationAction } from './actions';

describe('runOperationAction', () => {
  it.each([
    {
      result: { status: 'partial', connections: [
        { connection: 'work', status: 'partial', error_message: 'private arbitrary upstream detail' },
        { connection: 'personal', status: 'succeeded' }
      ] },
      message: 'CardDAV sync partial. Connections needing attention: work.'
    },
    {
      result: { status: 'failed', connections: [
        { connection: 'work', status: 'failed', error_message: 'private arbitrary upstream detail' }
      ] },
      message: 'CardDAV sync failed. Connections needing attention: work.'
    }
  ])('reports contacts sync $result.status', async ({ result, message }) => {
    const client = createAPIClient(async () => Response.json(result));
    const action = await runOperationAction('carddav_sync', client);
    expect(action).toEqual({ outcome: 'failed', message });
  });
});
