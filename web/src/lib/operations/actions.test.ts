import { describe, expect, it } from 'vitest';
import { createAPIClient } from '../api/client';
import { runOperationAction } from './actions';

describe('runOperationAction', () => {
  it.each([200, 500])('reports available conflicts after a sync failure with preview %i', async (status) => {
    const client = createAPIClient(async (input) => {
      const request = new Request(input);
      if (new URL(request.url).pathname.endsWith('/sync')) return Response.json({}, { status: 502 });
      expect(new URL(request.url).pathname).toBe('/api/v1/carddav/participant-bindings');
      expect(await request.json()).toEqual({ dry_run: true, undo: false });
      return Response.json({ conflicts: 2 }, { status });
    });
    const result = await runOperationAction('carddav_sync', client);
    expect(result).toEqual({ outcome: 'failed', message: status === 200
      ? 'Unable to start the operation. 2 CardDAV binding conflicts. Check CardDAV settings for the review command.'
      : 'Unable to start the operation.' });
  });

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
