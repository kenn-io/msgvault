import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import PersonBriefCard from './PersonBriefCard.svelte';

// Nothing on this list may reach the DOM: the API sends no archive excerpt,
// and the card withholds the evidence content key, the internal source URL,
// and the packet fingerprints the response does carry.
const forbidden = {
  evidenceKey: `sha256:${'f'.repeat(64)}`,
  sourceURL: 'https://forbidden.example.test/message/1',
  packetSHA: 'forbidden-packet-sha',
  claimKey: 'forbidden-claim-key'
};

function requestOf(input: RequestInfo | URL): Request {
  return input instanceof Request ? input : new Request(input);
}

function structured() {
  return {
    last_meaningful_interaction: { evidence_id: 'e1', summary: 'You compared notes on the role change.' },
    highlights: [{
      text: 'They were preparing for a role change.',
      speaker: 'person',
      evidence_ids: ['e1'],
      observed_at: '2026-08-29',
      confidence_basis_points: 800
    }],
    follow_ups: [{
      question: 'How did the transition go?',
      why: 'They were mid-move when you last spoke.',
      highlight_index: 0,
      evidence_ids: ['e1']
    }],
    appreciations: [{ text: 'You appreciated how candid they were.', evidence_ids: ['e2'] }],
    uncertainties: [{ text: 'The move may have changed.', kind: 'stale', evidence_ids: ['e2'] }],
    possible_attributes: [{ claim_key: forbidden.claimKey }]
  };
}

const sentenceTexts = [
  'Last time you talked (Aug 29, chat): you compared notes on the role change.',
  'They were preparing for a role change.',
  'You may want to ask how the transition went.',
  'You said you appreciated how candid they were.',
  'Check before assuming: the move may have changed.'
];

// The daemon names each sentence's own citations by ordinal. The appreciation
// carries none, which is the fallback the card still has to render.
function sentences() {
  return [
    { kind: 'last_interaction', index: 0, text: sentenceTexts[0], evidence_ordinals: [0] },
    { kind: 'highlight', index: 0, text: sentenceTexts[1], evidence_ordinals: [0] },
    { kind: 'follow_up', index: 0, text: sentenceTexts[2], evidence_ordinals: [0] },
    { kind: 'appreciation', index: 0, text: sentenceTexts[3], evidence_ordinals: [] },
    { kind: 'uncertainty', index: 0, text: sentenceTexts[4], evidence_ordinals: [1] }
  ];
}

function brief(overrides: Record<string, unknown> = {}) {
  return {
    version: 2,
    status: 'current',
    generated_at: '2026-08-29T18:42:10Z',
    rendered_text: sentenceTexts.join(' '),
    sentences: sentences(),
    structured: structured(),
    evidence: [
      {
        ordinal: 0, evidence_id: 11, evidence_key: forbidden.evidenceKey, source_ref: 'message:1',
        source_url: forbidden.sourceURL, directness: 'direct-other',
        event_time: '2026-08-29T17:00:00Z', evidence_supported: true
      },
      {
        ordinal: 1, evidence_id: 12, evidence_key: forbidden.evidenceKey, source_ref: 'conversation:4',
        source_url: forbidden.sourceURL, directness: 'direct-self',
        event_time: '2026-08-20T09:00:00Z', evidence_supported: false
      }
    ],
    boundary: { packet_sha256: forbidden.packetSHA },
    dropped_item_count: 1,
    program_id: 'msgvault-person-brief',
    program_version: 'v1',
    provider: 'openai_chat',
    model: 'gpt-test',
    rejected_at: null,
    rejected_reason: '',
    superseded_at: null,
    ...overrides
  };
}

function enrollment(enrolled: boolean) {
  return {
    person_id: 7,
    enrolled,
    enabled_at: enrolled ? '2026-08-01T00:00:00Z' : null,
    actor: 'api'
  };
}

function enrolledArchive(overrides: { brief?: Record<string, unknown> | null } = {}) {
  return vi.fn<typeof fetch>(async (input) => {
    const request = requestOf(input);
    const url = new URL(request.url);
    if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(true));
    if (url.pathname === '/api/v1/people/7/brief') {
      if (overrides.brief === null) {
        return Response.json({ error: 'person_brief_not_found', message: 'Person brief not found' }, { status: 404 });
      }
      return Response.json(brief(overrides.brief ?? {}));
    }
    if (url.pathname === '/api/v1/people/7/brief/versions') {
      return Response.json({
        versions: [
          brief(),
          brief({ version: 1, status: 'superseded', generated_at: '2026-08-01T00:00:00Z', superseded_at: '2026-08-29T18:42:10Z' })
        ]
      });
    }
    throw new Error(`Unexpected ${request.method} ${url.pathname}`);
  });
}

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((settle) => { resolve = settle; });
  return { promise, resolve };
}

describe('PersonBriefCard', () => {
  it('renders the paragraph, the version line, and no evidence excerpt or private identifier', async () => {
    render(PersonBriefCard, { client: createAPIClient(enrolledArchive()), personID: 7 });

    expect(await screen.findByRole('heading', { name: 'Last time we talked' })).toBeDefined();
    for (const text of sentenceTexts) {
      expect(await screen.findByRole('button', { name: text })).toBeDefined();
    }
    expect(screen.getByText(/Version 2/)).toBeDefined();
    expect(document.querySelector('time[datetime="2026-08-29T18:42:10Z"]')).not.toBeNull();
    expect(screen.getByText(/1 item was dropped/)).toBeDefined();
    expect(document.body.innerHTML).not.toMatch(/forbidden/i);
    expect(document.body.innerHTML).not.toMatch(/sha256|packet_sha256|evidence_key|source_url/i);
  });

  it('expands one sentence at a time to its structured item and its own citations', async () => {
    render(PersonBriefCard, { client: createAPIClient(enrolledArchive()), personID: 7 });

    const highlight = await screen.findByRole('button', { name: sentenceTexts[1] });
    expect(highlight.getAttribute('aria-expanded')).toBe('false');
    await fireEvent.click(highlight);

    await waitFor(() => expect(highlight.getAttribute('aria-expanded')).toBe('true'));
    const panel = document.getElementById(highlight.getAttribute('aria-controls') ?? '');
    expect(panel).not.toBeNull();
    expect(panel?.textContent).toContain('Said by this person');
    expect(panel?.textContent).toContain('Observed 2026-08-29');
    expect(panel?.textContent).toContain('Archive items this sentence cites');
    expect(panel?.textContent).toContain('message:1');
    expect(panel?.textContent).not.toContain('conversation:4');
    expect(panel?.textContent).not.toContain('Cited by the brief as a whole');
    expect(document.querySelector('time[datetime="2026-08-29T17:00:00Z"]')).not.toBeNull();
    // Directness is relative to the person, so a direct-other item is their
    // own transcribed meeting utterance, never something the owner said.
    expect(panel?.textContent).toContain('This person said it (meeting transcript)');
    expect(panel?.textContent).not.toContain('You said it');

    const uncertainty = screen.getByRole('button', { name: sentenceTexts[4] });
    await fireEvent.click(uncertainty);
    await waitFor(() => expect(uncertainty.getAttribute('aria-expanded')).toBe('true'));
    const uncertaintyPanel = document.getElementById(uncertainty.getAttribute('aria-controls') ?? '');
    expect(uncertaintyPanel?.textContent).toContain('conversation:4');
    expect(uncertaintyPanel?.textContent).not.toContain('message:1');
    expect(uncertaintyPanel?.textContent).toContain('Supporting source is no longer available');
    expect(uncertaintyPanel?.textContent).toContain('This person wrote it');

    const followUp = screen.getByRole('button', { name: sentenceTexts[2] });
    await fireEvent.click(followUp);
    await waitFor(() => expect(followUp.getAttribute('aria-expanded')).toBe('true'));
    expect(highlight.getAttribute('aria-expanded')).toBe('false');
    expect(screen.getByText('They were mid-move when you last spoke.')).toBeDefined();

    await fireEvent.click(followUp);
    await waitFor(() => expect(followUp.getAttribute('aria-expanded')).toBe('false'));
    expect(document.getElementById(followUp.getAttribute('aria-controls') ?? '')).toBeNull();
  });

  it('labels evidence directness relative to the person, not the owner', async () => {
    const archive = enrolledArchive({
      brief: {
        evidence: [
          {
            ordinal: 0, evidence_id: 11, evidence_key: forbidden.evidenceKey, source_ref: 'message:1',
            source_url: forbidden.sourceURL, directness: 'indirect',
            event_time: '2026-08-29T17:00:00Z', evidence_supported: true
          },
          {
            ordinal: 1, evidence_id: 12, evidence_key: forbidden.evidenceKey, source_ref: 'conversation:4',
            source_url: forbidden.sourceURL, directness: 'hearsay',
            event_time: '2026-08-20T09:00:00Z', evidence_supported: true
          }
        ]
      }
    });
    render(PersonBriefCard, { client: createAPIClient(archive), personID: 7 });

    const appreciation = await screen.findByRole('button', { name: sentenceTexts[3] });
    await fireEvent.click(appreciation);
    await waitFor(() => expect(appreciation.getAttribute('aria-expanded')).toBe('true'));

    const panel = document.getElementById(appreciation.getAttribute('aria-controls') ?? '');
    expect(panel?.textContent).toContain('Indirect');
    expect(panel?.textContent).toContain('Attribution unstated');
    expect(panel?.textContent).not.toContain('You said it');
    expect(panel?.textContent).not.toContain('They said it');
  });

  it('falls back to the whole brief citations for a sentence with none of its own', async () => {
    render(PersonBriefCard, { client: createAPIClient(enrolledArchive()), personID: 7 });

    const appreciation = await screen.findByRole('button', { name: sentenceTexts[3] });
    await fireEvent.click(appreciation);

    await waitFor(() => expect(appreciation.getAttribute('aria-expanded')).toBe('true'));
    const panel = document.getElementById(appreciation.getAttribute('aria-controls') ?? '');
    expect(panel?.textContent).toContain('Archive items this brief cites');
    expect(panel?.textContent).toContain('Cited by the brief as a whole');
    expect(panel?.textContent).toContain('message:1');
    expect(panel?.textContent).toContain('conversation:4');
  });

  it('keeps the paragraph readable when a version carries no sentence map', async () => {
    render(PersonBriefCard, {
      client: createAPIClient(enrolledArchive({ brief: { sentences: [] } })), personID: 7
    });

    expect(await screen.findByText(sentenceTexts.join(' '))).toBeDefined();
    expect(screen.getByText('Sentence expansion is unavailable for this brief version.')).toBeDefined();
    expect(screen.queryByRole('button', { name: sentenceTexts[1] })).toBeNull();
  });

  it('shows the enrol control and no brief for a person who is not enrolled', async () => {
    const paths: string[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const url = new URL(requestOf(input).url);
      paths.push(url.pathname);
      return Response.json(enrollment(false));
    });
    render(PersonBriefCard, { client: createAPIClient(fetchFn), personID: 7 });

    const toggle = await screen.findByRole('switch', { name: 'Enroll this person in briefs' }) as HTMLInputElement;
    expect(toggle.checked).toBe(false);
    expect(screen.getByRole('checkbox', { name: 'Also track this person' })).toBeDefined();
    expect(screen.getByText('This person is not enrolled, so no brief is generated or shown.')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Generate a brief now' })).toBeNull();
    expect(paths).toEqual(['/api/v1/people/7/brief-enrollment']);
  });

  it('enrols with the track option and announces the change once', async () => {
    const bodies: unknown[] = [];
    const onAnnounce = vi.fn();
    let enrolled = false;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (request.method === 'PUT') {
        bodies.push(await request.clone().json());
        enrolled = true;
        return Response.json(enrollment(true));
      }
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(enrolled));
      return Response.json({ error: 'person_brief_not_found' }, { status: 404 });
    });
    render(PersonBriefCard, { client: createAPIClient(fetchFn), personID: 7, onAnnounce });

    await fireEvent.click(await screen.findByRole('checkbox', { name: 'Also track this person' }));
    const toggle = await screen.findByRole('switch', { name: 'Enroll this person in briefs' });
    toggle.focus();
    await fireEvent.click(toggle);

    await waitFor(() => expect(onAnnounce).toHaveBeenCalledOnce());
    expect(bodies).toEqual([{ enrolled: true, track: true }]);
    expect(onAnnounce).toHaveBeenCalledWith('Brief enrollment enabled.');
    expect(await screen.findByText('No brief yet.')).toBeDefined();
    expect(await screen.findByRole('button', { name: 'Generate a brief now' })).toBeDefined();
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole('switch')));
  });

  it('resets "also track" after unenrolling so a later enrollment does not retrack them', async () => {
    const bodies: Array<{ enrolled: boolean; track: boolean }> = [];
    let enrolled = false;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (request.method === 'PUT') {
        const body = (await request.clone().json()) as { enrolled: boolean; track: boolean };
        bodies.push(body);
        enrolled = body.enrolled;
        return Response.json(enrollment(enrolled));
      }
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(enrolled));
      return Response.json({ error: 'person_brief_not_found' }, { status: 404 });
    });
    render(PersonBriefCard, { client: createAPIClient(fetchFn), personID: 7 });

    // The Toggle stays mounted across every enrolment change, so one
    // reference tracks it through the whole flow. A settled mutation always
    // ends by refocusing this same control (the card's only enabled input
    // while unenrolled or brief-missing), so blurring after each click and
    // waiting for focus to return is a reliable way to wait out the full
    // change, including the "also track" reset that follows it.
    const toggle = (await screen.findByRole('switch', {
      name: 'Enroll this person in briefs'
    })) as HTMLInputElement;
    async function clickAndSettle(): Promise<void> {
      await fireEvent.click(toggle);
      toggle.blur();
      await waitFor(() => expect(document.activeElement).toBe(toggle));
    }

    // Check "also track", then enrol: the enrolment PUT carries track: true.
    await fireEvent.click(await screen.findByRole('checkbox', { name: 'Also track this person' }));
    await clickAndSettle();
    expect(bodies).toEqual([{ enrolled: true, track: true }]);

    // Unenrol: the checkbox reappears once the person is unenrolled again.
    await clickAndSettle();
    expect(bodies[1].enrolled).toBe(false);

    // The remounted checkbox settles unchecked: a stale "also track" from the
    // first enrolment must not carry over now that the person is unenrolled.
    const checkbox = (await screen.findByRole('checkbox', {
      name: 'Also track this person'
    })) as HTMLInputElement;
    expect(checkbox.checked).toBe(false);

    // Enrolling again without re-checking the box posts track: false.
    await clickAndSettle();
    expect(bodies[2]).toEqual({ enrolled: true, track: false });
  });

  it('reports the untracked conflict as the card option rather than the daemon command', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (request.method === 'PUT') {
        return Response.json({
          error: 'person_brief_not_tracked',
          message: 'Track the person with `msgvault person track` before enrolling it in briefs'
        }, { status: 409 });
      }
      return Response.json(enrollment(false));
    });
    render(PersonBriefCard, { client: createAPIClient(fetchFn), personID: 7 });

    const toggle = await screen.findByRole('switch', { name: 'Enroll this person in briefs' });
    toggle.focus();
    await fireEvent.click(toggle);

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Also track this person');
    expect(document.body.textContent).not.toContain('msgvault person track');
    // The refused change must not leave the switch showing what was clicked.
    await waitFor(() => expect((screen.getByRole('switch') as HTMLInputElement).checked).toBe(false));
    expect(document.activeElement).toBe(screen.getByRole('switch'));
  });

  it('rejects the current version with a typed reason and shows no brief until one is regenerated', async () => {
    const bodies: unknown[] = [];
    let rejected = false;
    const onAnnounce = vi.fn();
    const rejectedVersion = {
      status: 'rejected', rejected_at: '2026-08-30T09:00:00Z', rejected_reason: 'Wrong person'
    };
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (request.method === 'POST' && url.pathname === '/api/v1/people/7/brief/reject') {
        bodies.push(await request.clone().json());
        rejected = true;
        return Response.json(brief(rejectedVersion));
      }
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(true));
      if (url.pathname === '/api/v1/people/7/brief/versions') {
        return Response.json({ versions: [brief(rejected ? rejectedVersion : {})] });
      }
      if (url.pathname === '/api/v1/people/7/brief') {
        // After the rejection the daemon has no current version to return.
        if (rejected) return Response.json({ error: 'person_brief_not_found' }, { status: 404 });
        return Response.json(brief());
      }
      throw new Error(`Unexpected ${request.method} ${url.pathname}`);
    });
    render(PersonBriefCard, { client: createAPIClient(fetchFn), personID: 7, onAnnounce });

    const reason = await screen.findByRole('textbox', { name: 'Why this brief is wrong (optional)' });
    await fireEvent.input(reason, { target: { value: 'Wrong person' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Reject this brief version' }));

    await waitFor(() => expect(onAnnounce).toHaveBeenCalledWith('Brief version 2 rejected.'));
    expect(bodies).toEqual([{ reason: 'Wrong person' }]);
    // The rejected paragraph is gone: the card offers a fresh generation, not
    // a reject button for a version that is no longer current.
    expect(await screen.findByText('No brief yet.')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Generate a brief now' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Reject this brief version' })).toBeNull();
    expect(screen.queryByText(sentenceTexts[0])).toBeNull();
    expect(screen.queryByText(/Version 2/)).toBeNull();

    // The rejected version survives only in the history.
    await fireEvent.click(screen.getByRole('button', { name: 'Show brief version history' }));
    const history = await screen.findByRole('list', { name: 'Brief version history' });
    expect(within(history).getByText('Version 2 · Rejected')).toBeDefined();
    expect(within(history).getByText('Reason: Wrong person')).toBeDefined();
  });

  it('shows a spinner while regenerating and then the stored version', async () => {
    const run = deferredResponse();
    let generated = false;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (request.method === 'POST') return run.promise;
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(true));
      if (url.pathname === '/api/v1/people/7/brief') return Response.json(brief(generated ? { version: 3 } : {}));
      throw new Error(`Unexpected ${request.method} ${url.pathname}`);
    });
    render(PersonBriefCard, { client: createAPIClient(fetchFn), personID: 7 });

    const regenerate = await screen.findByRole('button', { name: 'Regenerate this brief' });
    await fireEvent.click(regenerate);

    expect(await screen.findByText('Generating a brief. This spends provider budget.')).toBeDefined();
    expect((screen.getByRole('button', { name: 'Regenerate this brief' }) as HTMLButtonElement).disabled).toBe(true);
    generated = true;
    run.resolve(Response.json({ run_id: 'run-1', attempt_id: 'attempt-1', brief_version: 3, brief_failure_class: '' }));

    expect(await screen.findByText('Brief version 3 generated.')).toBeDefined();
    await waitFor(() => expect(screen.getByText(/Version 3/)).toBeDefined());
    expect(screen.queryByText('Generating a brief. This spends provider budget.')).toBeNull();
  });

  it('names the bounded failure class when an attempt stores no version', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (request.method === 'POST') {
        return Response.json({ run_id: 'run-1', attempt_id: 'attempt-1', brief_version: 0, brief_failure_class: 'budget' });
      }
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(true));
      return Response.json({ error: 'person_brief_not_found' }, { status: 404 });
    });
    render(PersonBriefCard, { client: createAPIClient(fetchFn), personID: 7 });

    await fireEvent.click(await screen.findByRole('button', { name: 'Generate a brief now' }));

    expect(await screen.findByText('No new brief version. Reason: Provider budget exhausted.')).toBeDefined();
    expect(screen.getByText('No brief yet.')).toBeDefined();
  });

  it('loads the version history on request and hides it again', async () => {
    const fetchFn = enrolledArchive();
    render(PersonBriefCard, { client: createAPIClient(fetchFn), personID: 7 });

    const show = await screen.findByRole('button', { name: 'Show brief version history' });
    expect(show.getAttribute('aria-expanded')).toBe('false');
    await fireEvent.click(show);

    const history = await screen.findByRole('list', { name: 'Brief version history' });
    expect(history.textContent).toContain('Version 2');
    expect(history.textContent).toContain('Current');
    expect(history.textContent).toContain('Version 1');
    expect(history.textContent).toContain('Superseded');
    expect(fetchFn.mock.calls.filter(([input]) => new URL(requestOf(input).url).pathname.endsWith('/brief/versions'))).toHaveLength(1);

    await fireEvent.click(await screen.findByRole('button', { name: 'Hide brief version history' }));
    expect(screen.queryByRole('list', { name: 'Brief version history' })).toBeNull();
  });

  it('offers a bounded retry when the brief read fails and never renders the daemon message', async () => {
    let reads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const url = new URL(requestOf(input).url);
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(true));
      reads += 1;
      if (reads === 1) return Response.json({ error: 'internal', message: forbidden.packetSHA }, { status: 500 });
      return Response.json(brief());
    });
    render(PersonBriefCard, { client: createAPIClient(fetchFn), personID: 7 });

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('Unable to load the brief.');
    expect(document.body.textContent).not.toContain(forbidden.packetSHA);

    await fireEvent.click(screen.getByRole('button', { name: 'Retry the brief' }));

    expect(await screen.findByRole('button', { name: sentenceTexts[1] })).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('clears an expanded sentence and stale state when the person changes', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      const id = Number(url.pathname.split('/')[4]);
      if (url.pathname.endsWith('/brief-enrollment')) {
        return Response.json({ ...enrollment(id === 7), person_id: id });
      }
      if (url.pathname.endsWith('/brief')) return Response.json(brief());
      throw new Error(`Unexpected ${request.method} ${url.pathname}`);
    });
    const view = render(PersonBriefCard, { client: createAPIClient(fetchFn), personID: 7 });

    await fireEvent.click(await screen.findByRole('button', { name: sentenceTexts[1] }));
    await waitFor(() => expect(screen.getByRole('button', { name: sentenceTexts[1] }).getAttribute('aria-expanded')).toBe('true'));

    await view.rerender({ client: createAPIClient(fetchFn), personID: 9 });

    expect(await screen.findByText('This person is not enrolled, so no brief is generated or shown.')).toBeDefined();
    expect(screen.queryByRole('button', { name: sentenceTexts[1] })).toBeNull();
    expect(document.body.textContent).not.toContain('Said by this person');
  });
});
