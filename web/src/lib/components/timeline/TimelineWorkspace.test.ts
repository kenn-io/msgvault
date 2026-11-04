import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, onTestFinished, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { parseExploreURLState } from '../../explore/state.svelte';
import { TimelineController } from '../../timeline/controller.svelte';
import { entries, golden } from '../../timeline/fixtures';
import TimelineWorkspace from './TimelineWorkspace.svelte';

function setup(fetchFn: typeof fetch = vi.fn(async () => Response.json(golden()))) {
  const controller = new TimelineController(createAPIClient(fetchFn));
  onTestFinished(() => controller.destroy());
  const navigation = { onOpenMessage: vi.fn(), onOpenConversation: vi.fn(), onOpenPerson: vi.fn(), onOpenMeeting: vi.fn() };
  const view = render(TimelineWorkspace, { controller, ...navigation });
  return { controller, fetchFn, ...navigation, ...view };
}

describe('TimelineWorkspace', () => {
  it('renders every Day section through the generated client', async () => {
    const { fetchFn } = setup();
    await screen.findByText('Authored daily note');
    for (const name of ['Open loops', 'Upcoming', 'Meetings', 'Conversations', 'Calls', 'Notes']) {
      expect(screen.getByRole('heading', { name })).toBeTruthy();
    }
    expect(screen.getByText('Upcoming activity comes from the archive.')).toBeTruthy();
    const request = (vi.mocked(fetchFn).mock.calls[0]![0] as Request);
    expect(new URL(request.url).pathname).toBe('/api/v1/timeline');
    expect(new URL(request.url).searchParams.get('format')).toBe('json');
  });

  it('shows only a warning for hidden text, even if the payload contains a title and people', async () => {
    const value = golden();
    value.upcoming = [{ ...entries[0]!, hidden_text: true }];
    value.days = [];
    value.open_loops = undefined;
    setup(vi.fn(async () => Response.json(value)));
    const warning = await screen.findByText('Hidden text is present. Open the original to review it.');
    const row = warning.closest('article')!;
    expect(row.textContent).not.toContain('Planning meeting');
    expect(row.textContent).not.toContain('Alex Example');
    expect(row.querySelector('a')).toBeNull();
  });

  it('renders snippets as escaped plain text', async () => {
    setup(vi.fn(async () => Response.json({ ...golden(), history: [entries[3]!] })));
    await screen.findByText('<img src=x onerror=alert(1)>');
    expect(document.querySelector('article img')).toBeNull();
  });

  it('reuses Directory search and allows choosing an ambiguity candidate', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input as Request;
      requests.push(request);
      const url = new URL(request.url);
      if (url.pathname === '/api/v1/people/directory') return Response.json({ people: [{
        id: 7, display_name: 'Alex Example', vcard_uid: 'example-7', revision: 1,
        carddav_bindings: [], categories: [], organizations: [], contact_state: 'active',
      }] });
      if (url.searchParams.get('with') === 'Alex') return Response.json({
        error: 'ambiguous_reference', candidates: [{
          person_id: 7, person_uid: 'example-7', participant_ids: [17], display_name: 'Alex Choice', address: 'alex@example.test',
        }],
      }, { status: 409 });
      return Response.json(golden());
    });
    const { controller } = setup(fetchFn);
    await fireEvent.click(screen.getByRole('tab', { name: 'Person' }));
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Find a person' }), { target: { value: 'Alex' } });
    await waitFor(() => expect((screen.getByRole('button', { name: 'Search people' }) as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(screen.getByRole('button', { name: 'Search people' }));
    await screen.findByText('Alex Example');
    expect(requests.some((r) => new URL(r.url).pathname === '/api/v1/people/directory' && new URL(r.url).searchParams.get('q') === 'Alex')).toBe(true);
    await fireEvent.click(screen.getByRole('button', { name: 'Show person timeline' }));
    const candidate = await screen.findByRole('button', { name: /Alex Choice.*alex@example.test/ });
    await fireEvent.click(candidate);
    await waitFor(() => expect(controller.result).not.toBeNull());
    expect(new URL(requests.at(-1)!.url).searchParams.get('with')).toBe('person:7');
  });

  it.each(entries)('uses the exact $kind anchor for Around', async (entry) => {
    const requests: Request[] = [];
    setup(vi.fn(async (input) => {
      requests.push(input as Request);
      return Response.json({ ...golden(), days: [], upcoming: [], open_loops: undefined, history: [entry] });
    }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Around' }));
    await waitFor(() => expect(requests).toHaveLength(2));
    expect(new URL(requests[1]!.url).searchParams.get('around')).toBe(entry.anchor_ref);
    expect(screen.getByRole('tab', { name: 'Around' }).getAttribute('aria-selected')).toBe('true');
  });

  it.each([{ before: 0, after: 0 }, { before: 2, after: 3 }])('sends numeric neighbor inputs $before/$after without replacing zero', async ({ before, after }) => {
    const requests: Request[] = [];
    setup(vi.fn(async (input) => {
      requests.push(input as Request);
      return Response.json(golden());
    }));
    await screen.findByText('Authored daily note');
    await fireEvent.click(screen.getByRole('tab', { name: 'Around' }));
    const beforeInput = screen.getByRole('spinbutton', { name: 'Before' }) as HTMLInputElement;
    const afterInput = screen.getByRole('spinbutton', { name: 'After' }) as HTMLInputElement;
    expect(beforeInput.value).toBe('5');
    expect(afterInput.value).toBe('5');
    await fireEvent.input(screen.getByRole('textbox', { name: 'Anchor' }), { target: { value: 'msg:14' } });
    await fireEvent.input(beforeInput, { target: { value: String(before) } });
    await fireEvent.input(afterInput, { target: { value: String(after) } });
    await fireEvent.click(screen.getByRole('button', { name: 'Show around' }));
    await waitFor(() => expect(requests).toHaveLength(2));
    const query = new URL(requests[1]!.url).searchParams;
    expect(query.get('around')).toBe('msg:14');
    expect(query.get('before')).toBe(String(before));
    expect(query.get('after')).toBe(String(after));
  });

  it('combines abnormal freshness and gaps in one line and omits an empty line', async () => {
    const value = golden();
    value.freshness.sources = [{ source_id: 1, source: 'email:example', status: 'stale', last_run: 'failed',
      scheduling: 'scheduled', warnings: [], sync_command: 'msgvault sync' }];
    value.gaps = [{ section: 'calls', code: 'owner-only', retryable: false }];
    const { controller } = setup(vi.fn<typeof fetch>()
      .mockResolvedValueOnce(Response.json(value)).mockResolvedValueOnce(Response.json(golden())));
    const line = await screen.findByLabelText('Timeline freshness and gaps');
    expect(line.textContent).toContain('stale');
    expect(line.textContent).toContain('msgvault sync');
    expect(line.textContent).toContain('calls: owner-only');
    expect(screen.getAllByLabelText('Timeline freshness and gaps')).toHaveLength(1);
    await controller.load({ mode: 'day' });
    await waitFor(() => expect(screen.queryByLabelText('Timeline freshness and gaps')).toBeNull());
  });

  it('routes entries to the existing reader, thread, person and archived meeting paths', async () => {
    const { onOpenMessage, onOpenConversation, onOpenPerson, onOpenMeeting } = setup(vi.fn(async () =>
      Response.json({ ...golden(), days: [], upcoming: [], open_loops: undefined, history: [entries[1]!, entries[2]!, entries[3]!] })));
    const messageRow = await screen.findByRole('article', { name: 'message msg:14' });
    const read = within(messageRow).getByRole('link', { name: 'Open message' });
    expect(read.getAttribute('href')).toBe('/messages/14');
    await fireEvent.click(read);
    expect(onOpenMessage).toHaveBeenCalledWith(14);
    const thread = screen.getByRole('link', { name: 'Open conversation' });
    expect(parseExploreURLState(new URL(thread.getAttribute('href')!, document.baseURI).search).selectedRow).toBe('conversation:3');
    await fireEvent.click(thread);
    expect(onOpenConversation).toHaveBeenCalledWith(3, 13);
    const row = read.closest('article')!;
    await fireEvent.click(within(row).getByRole('link', { name: 'Alex Example' }));
    expect(onOpenPerson).toHaveBeenCalledWith(7);
    const meeting = screen.getByRole('link', { name: 'Open meeting' });
    expect(meeting.getAttribute('href')).toBe('/messages/12');
    await fireEvent.click(meeting);
    expect(onOpenMeeting).toHaveBeenCalledWith(12);
  });

  it('opens the original message when a calendar event has no supported detail action', async () => {
    const calendar = { ...entries[0]!, open: [entries[0]!.open[0]!] };
    const { onOpenMessage } = setup(vi.fn(async () =>
      Response.json({ ...golden(), days: [], upcoming: [], open_loops: undefined, history: [calendar] })));
    const row = await screen.findByRole('article', { name: `meeting ${calendar.ref}` });
    const original = within(row).getByRole('link', { name: 'Open original message' });
    expect(original.getAttribute('href')).toBe('/messages/1');
    await fireEvent.click(original);
    expect(onOpenMessage).toHaveBeenCalledWith(1);
  });
});
