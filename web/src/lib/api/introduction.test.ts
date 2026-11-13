import { describe, expect, expectTypeOf, it, vi } from 'vitest';

import { getIntroduction, listIntroductions } from './generated/api/api';
import type { IntroductionRecord } from './generated/models';

describe('generated introduction client', () => {
  it.each([undefined, 42])('round trips optional meeting context %s', async (meetingMessageID) => {
    expectTypeOf<IntroductionRecord['meeting_message_id']>().toEqualTypeOf<number | undefined>();
    const record: IntroductionRecord = {
      intro_id: 'intro-0123456789abcdef0123456789abcdef',
      source_id: 7,
      person_a_id: 11,
      person_b_id: 12,
      status: 'prepared',
      revision: 1,
      title: 'Email introduction',
      body: '',
      created_by: 'agent',
      created_at: '2026-11-13T10:00:00Z',
      updated_at: '2026-11-13T10:00:00Z',
      cancelled_at: null,
      members: [{ person_id: 11, participant_id: 21, user_id: '' }],
      blockers: [],
      ...(meetingMessageID === undefined ? {} : { meeting_message_id: meetingMessageID }),
    };
    const page = { items: [record], has_more: false, next_after_id: '' };
    const fetchFn = vi.fn<typeof fetch>(async (input, init) => {
      const url = new URL(String(input), 'https://archive.example.test');
      expect(init?.method).toBe('GET');
      if (url.pathname === '/api/v1/intros') {
        expect(url.searchParams.get('source_id')).toBe('7');
        return Response.json(page);
      }
      expect(url.pathname).toBe(`/api/v1/intros/${record.intro_id}`);
      return Response.json(record);
    });
    const detail = await getIntroduction({ id: record.intro_id }, { fetch: fetchFn });
    const listed = await listIntroductions({ source_id: 7 }, { fetch: fetchFn });

    expect(detail.response.status).toBe(200);
    expect(detail.data).toEqual(record);
    expect(listed.data).toEqual(page);
    expect(listed.data?.items[0]).toEqual(detail.data);
    expect(detail.data?.meeting_message_id).toBe(meetingMessageID);
    expect(Object.hasOwn(detail.data!, 'meeting_message_id')).toBe(meetingMessageID !== undefined);
    expect(JSON.parse(JSON.stringify(detail.data))).toEqual(record);
    expect(fetchFn).toHaveBeenCalledTimes(2);
  });
});
