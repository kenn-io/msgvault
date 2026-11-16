import { describe, expect, it } from 'vitest';
import { createAPIClient } from '../api/client';
import { runOperationAction } from './actions';

describe('runOperationAction', () => {
  it.each([
    [409, 'conflict', 'The operation could not start because current server state changed.'],
    [502, 'failed', 'Unable to start the operation.']
  ])('reports a sync error with status %i', async (status, outcome, message) => {
    const client = createAPIClient(async () => Response.json({}, { status }));
    expect(await runOperationAction('carddav_sync', client)).toEqual({ outcome, message });
  });

  it.each([
    {
      result: { status: 'partial', binding_conflicts: 2, connections: [
        { connection: 'work', status: 'partial', error_message: 'private arbitrary upstream detail' },
        { connection: 'personal', status: 'succeeded' }
      ] },
      message: 'CardDAV sync partial. Connections needing attention: work.'
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
});
