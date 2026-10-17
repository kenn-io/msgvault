import { beforeEach, expect, it } from 'vitest';
import { createAPIClient } from '../api/client';
import { reportSearchOutcome } from './actions';

beforeEach(() => sessionStorage.clear());

it('keeps one outcome through a retry, identical submission, and reload', async () => {
 const events: unknown[] = [];
 const client = createAPIClient(async (request) => {
  events.push(await (request as Request).json());
  return Response.json({ status: 'queued' }, { status: 202 });
 });
 await reportSearchOutcome(client, '  missing   item ', 'full_text', true);
 await reportSearchOutcome(client, 'missing item', 'full_text', true);
 await reportSearchOutcome(client, 'missing item', 'semantic', true);
 await reportSearchOutcome(client, 'another', 'full_text', false);
 expect(events).toEqual([
  { event: 'search_empty', properties: { surface: 'web', search_type: 'text' } },
  { event: 'search_empty', properties: { surface: 'web', search_type: 'semantic' } },
 ]);
 expect(JSON.stringify(sessionStorage)).not.toContain('missing');
});
