import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../api/client';
import { ArchiveMessageLoader } from './message-loader.svelte';

describe('ArchiveMessageLoader', () => {
  it('ignores cancelled details and clears missing state when retried', async () => {
    let finish!: (value: Response) => void;
    const fetchFn = vi.fn<typeof fetch>()
      .mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }))
      .mockResolvedValueOnce(Response.json({ error: 'not_found' }, { status: 404 }))
      .mockResolvedValueOnce(Response.json({ id: 10, conversation_id: 2 }));
    const client = createAPIClient(fetchFn);
    const loader = new ArchiveMessageLoader();
    const old = loader.load(client, 9);
    await loader.load(client, 10);
    expect((fetchFn.mock.calls[0][0] as Request).signal.aborted).toBe(true);
    expect(loader.missing).toBe(true);
    expect(loader.error).toBe('Message not found.');
    finish(Response.json({ id: 9, conversation_id: 2 }));
    await old;
    expect(loader.detail).toBeUndefined();
    await loader.load(client, 10);
    expect(loader.detail?.id).toBe(10);
    expect(loader.missing).toBe(false);
    expect(loader.error).toBe('');
    loader.cancel();
  });
});
