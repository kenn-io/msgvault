import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../api/client';
import { PersonBriefController, briefFailureClassLabel } from './person-brief-controller.svelte';

function requestOf(input: RequestInfo | URL): Request {
  return input instanceof Request ? input : new Request(input);
}

// Fields the card must never project: the evidence content hash, the internal
// source URL, the packet-local citation IDs, and the provider fingerprints.
const forbidden = {
  evidenceKey: `sha256:${'f'.repeat(64)}`,
  sourceURL: 'https://forbidden.example.test/message/1',
  claimKey: 'forbidden-claim-key',
  packetSHA: 'forbidden-packet-sha'
};

function structured() {
  return {
    last_meaningful_interaction: { evidence_id: 'e1', summary: 'Talked about the role change.' },
    highlights: [
      {
        text: 'They were preparing for a role change.',
        speaker: 'person',
        evidence_ids: ['e1'],
        observed_at: '2026-08-29',
        confidence_basis_points: 800
      },
      {
        text: 'You offered to review the offer letter.',
        speaker: 'owner',
        evidence_ids: ['e2'],
        observed_at: null,
        confidence_basis_points: 700
      }
    ],
    follow_ups: [
      {
        question: 'How did the transition go?',
        why: 'They were mid-move when you last spoke.',
        highlight_index: 0,
        evidence_ids: ['e1']
      }
    ],
    appreciations: [{ text: 'You appreciated how candid they were.', evidence_ids: ['e2'] }],
    uncertainties: [
      { text: 'The move may have changed.', kind: 'stale', evidence_ids: ['e2'] }
    ],
    possible_attributes: [{ claim_key: forbidden.claimKey, target: { kind: 'attribute' } }]
  };
}

// evidence_ordinals is the join key the daemon computes: each sentence names
// the entries of the response's own evidence list that it cites.
function sentences() {
  return [
    { kind: 'last_interaction', index: 0, text: 'Last time you talked (Aug 29, chat): talked about the role change.', evidence_ordinals: [0] },
    { kind: 'highlight', index: 0, text: 'They were preparing for a role change.', evidence_ordinals: [0] },
    { kind: 'highlight', index: 1, text: 'You offered to review the offer letter.', evidence_ordinals: [1] },
    { kind: 'follow_up', index: 0, text: 'You may want to ask how the transition went.', evidence_ordinals: [0] },
    { kind: 'appreciation', index: 0, text: 'You said you appreciated how candid they were.', evidence_ordinals: [] },
    { kind: 'uncertainty', index: 0, text: 'Check before assuming: the move may have changed.', evidence_ordinals: [1] }
  ];
}

const citedEvidence = {
  first: { ordinal: 0, source_ref: 'message:1', directness: 'direct-other', event_time: '2026-08-29T17:00:00Z', supported: true },
  second: { ordinal: 1, source_ref: 'conversation:4', directness: 'direct-self', event_time: '2026-08-20T09:00:00Z', supported: false }
};

function brief(overrides: Record<string, unknown> = {}) {
  return {
    version: 2,
    status: 'current',
    generated_at: '2026-08-29T18:42:10Z',
    rendered_text: sentences().map((sentence) => sentence.text).join(' '),
    sentences: sentences(),
    structured: structured(),
    evidence: [
      {
        ordinal: 0,
        evidence_id: 11,
        evidence_key: forbidden.evidenceKey,
        source_ref: 'message:1',
        source_url: forbidden.sourceURL,
        directness: 'direct-other',
        event_time: '2026-08-29T17:00:00Z',
        evidence_supported: true
      },
      {
        ordinal: 1,
        evidence_id: 12,
        evidence_key: forbidden.evidenceKey,
        source_ref: 'conversation:4',
        source_url: forbidden.sourceURL,
        directness: 'direct-self',
        event_time: '2026-08-20T09:00:00Z',
        evidence_supported: false
      }
    ],
    boundary: { packet_sha256: forbidden.packetSHA, item_count: 31 },
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

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((settle) => {
    resolve = settle;
  });
  return { promise, resolve };
}

describe('PersonBriefController', () => {
  it('reads enrollment before the brief and projects sentences, items, and evidence without private fields', async () => {
    const paths: string[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      paths.push(`${request.method} ${url.pathname}`);
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(true));
      if (url.pathname === '/api/v1/people/7/brief') return Response.json(brief());
      throw new Error(`Unexpected ${request.method} ${url.pathname}`);
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));

    await controller.setPerson(7);

    expect(paths).toEqual([
      'GET /api/v1/people/7/brief-enrollment',
      'GET /api/v1/people/7/brief'
    ]);
    expect(controller.enrollment).toEqual({ person_id: 7, enrolled: true, enabled_at: '2026-08-01T00:00:00Z' });
    expect(controller.briefMissing).toBe(false);
    expect(controller.brief?.version).toBe(2);
    expect(controller.brief?.dropped_item_count).toBe(1);
    expect(controller.brief?.sentences.map((sentence) => sentence.key)).toEqual([
      'last_interaction:0', 'highlight:0', 'highlight:1', 'follow_up:0', 'appreciation:0', 'uncertainty:0'
    ]);
    expect(controller.brief?.sentences[1]).toEqual({
      key: 'highlight:0',
      kind: 'highlight',
      index: 0,
      text: 'They were preparing for a role change.',
      detail: 'They were preparing for a role change.',
      meta: ['Said by this person', 'Observed 2026-08-29'],
      evidence: [citedEvidence.first]
    });
    expect(controller.brief?.sentences[3]?.detail).toBe('How did the transition go?');
    expect(controller.brief?.sentences[3]?.meta).toEqual(['They were mid-move when you last spoke.']);
    expect(controller.brief?.sentences[5]?.meta).toEqual(['May be out of date']);
    expect(controller.brief?.sentences[5]?.evidence).toEqual([citedEvidence.second]);
    expect(controller.brief?.sentences[4]?.evidence).toEqual([]);
    expect(controller.brief?.evidence).toEqual([citedEvidence.first, citedEvidence.second]);
    expect(JSON.stringify(controller.brief)).not.toMatch(/forbidden|sha256|evidence_key|source_url|possible_attributes/i);
    controller.destroy();
  });

  it('does not read the brief for a person who is not enrolled', async () => {
    const paths: string[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      paths.push(url.pathname);
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(false));
      throw new Error(`Unexpected ${request.method} ${url.pathname}`);
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));

    await controller.setPerson(7);

    expect(paths).toEqual(['/api/v1/people/7/brief-enrollment']);
    expect(controller.enrollment?.enrolled).toBe(false);
    expect(controller.brief).toBeUndefined();
    expect(controller.briefMissing).toBe(false);
    expect(controller.briefError).toBeNull();
    controller.destroy();
  });

  it('treats person_brief_not_found as no brief rather than an error', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const url = new URL(requestOf(input).url);
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(true));
      return Response.json({ error: 'person_brief_not_found', message: 'Person brief not found' }, { status: 404 });
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));

    await controller.setPerson(7);

    expect(controller.briefMissing).toBe(true);
    expect(controller.brief).toBeUndefined();
    expect(controller.briefError).toBeNull();
    controller.destroy();
  });

  it('enrolls with the exact replacement body and loads the brief once enrolled', async () => {
    const bodies: unknown[] = [];
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
      if (url.pathname === '/api/v1/people/7/brief') return Response.json(brief());
      throw new Error(`Unexpected ${request.method} ${url.pathname}`);
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));
    await controller.setPerson(7);
    expect(controller.brief).toBeUndefined();

    const outcome = await controller.setEnrolled(true, true);

    expect(outcome.kind).toBe('confirmed');
    expect(bodies).toEqual([{ enrolled: true, track: true }]);
    expect(controller.enrollment?.enrolled).toBe(true);
    expect(controller.brief?.version).toBe(2);
    expect(controller.announcement).toBe('Brief enrollment enabled.');
    controller.destroy();
  });

  it('clears the brief when the owner unenrolls', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (request.method === 'PUT') return Response.json(enrollment(false));
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(true));
      if (url.pathname === '/api/v1/people/7/brief') return Response.json(brief());
      throw new Error(`Unexpected ${request.method} ${url.pathname}`);
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));
    await controller.setPerson(7);
    expect(controller.brief?.version).toBe(2);

    const outcome = await controller.setEnrolled(false, false);

    expect(outcome.kind).toBe('confirmed');
    expect(controller.enrollment?.enrolled).toBe(false);
    expect(controller.brief).toBeUndefined();
    expect(controller.announcement).toBe('Brief enrollment disabled.');
    controller.destroy();
  });

  it('ignores a brief response that arrives after unenrollment', async () => {
    const read = deferredResponse();
    let briefRequest: Request | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (request.method === 'PUT') return Response.json(enrollment(false));
      if (new URL(request.url).pathname.endsWith('/brief-enrollment')) return Response.json(enrollment(true));
      briefRequest = request;
      return read.promise;
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));
    const loading = controller.setPerson(7);
    await vi.waitFor(() => expect(briefRequest).toBeDefined());

    expect(await controller.setEnrolled(false, false)).toEqual({ kind: 'confirmed' });
    read.resolve(Response.json(brief()));
    await loading;

    expect(controller.enrollment?.enrolled).toBe(false);
    expect(controller.brief).toBeUndefined();
    expect(controller.briefError).toBeNull();
    expect(controller.briefLoading).toBe(false);
    expect(briefRequest?.signal.aborted).toBe(true);
    controller.destroy();
  });

  it.each([200, 500])('keeps enrollment pending until its brief reload settles (%s)', async (status) => {
    const read = deferredResponse();
    let briefReads = 0;
    let writes = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (request.method === 'PUT') {
        writes += 1;
        return Response.json(enrollment(true));
      }
      if (new URL(request.url).pathname.endsWith('/brief-enrollment')) return Response.json(enrollment(false));
      briefReads += 1;
      return read.promise;
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));
    await controller.setPerson(7);
    const enrolling = controller.setEnrolled(true, false);
    await vi.waitFor(() => expect(briefReads).toBe(1));

    const pending = controller.pending;
    const duplicate = await controller.setEnrolled(false, false);
    read.resolve(Response.json(status === 200 ? brief() : { error: 'internal' }, { status }));
    await enrolling;

    expect(pending).toBe('enrollment');
    expect(duplicate).toEqual({ kind: 'ignored' });
    expect(writes).toBe(1);
    expect(controller.pending).toBeNull();
    expect(controller.briefLoading).toBe(false);
    expect(controller.brief?.version).toBe(status === 200 ? 2 : undefined);
    expect(controller.briefError).toBe(status === 200 ? null : 'Unable to load the brief.');
    controller.destroy();
  });

  it('maps person_brief_not_tracked to the track option instead of the daemon CLI sentence', async () => {
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
    const controller = new PersonBriefController(createAPIClient(fetchFn));
    await controller.setPerson(7);

    const outcome = await controller.setEnrolled(true, false);

    expect(outcome.kind).toBe('error');
    expect(controller.enrollmentError).toBe(
      'This person must be tracked before brief enrollment. Select “Also track this person” and enroll again.'
    );
    expect(controller.enrollmentError).not.toMatch(/msgvault person track/);
    expect(controller.enrollment?.enrolled).toBe(false);
    controller.destroy();
  });

  it('rejects the current version with the typed reason and treats the brief as missing', async () => {
    const bodies: unknown[] = [];
    let briefReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (request.method === 'POST' && url.pathname === '/api/v1/people/7/brief/reject') {
        bodies.push(await request.clone().json());
        return Response.json(brief({ status: 'rejected', rejected_at: '2026-08-30T00:00:00Z', rejected_reason: 'Wrong person' }));
      }
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(true));
      if (url.pathname === '/api/v1/people/7/brief') {
        briefReads += 1;
        return Response.json(brief());
      }
      throw new Error(`Unexpected ${request.method} ${url.pathname}`);
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));
    await controller.setPerson(7);
    expect(controller.brief?.version).toBe(2);

    const outcome = await controller.reject('  Wrong person  ');

    expect(outcome.kind).toBe('confirmed');
    expect(bodies).toEqual([{ reason: 'Wrong person' }]);
    // The daemon's read would now answer person_brief_not_found: the rejected
    // version is not the current brief and must not be shown as one.
    expect(controller.brief).toBeUndefined();
    expect(controller.briefMissing).toBe(true);
    expect(controller.briefError).toBeNull();
    expect(controller.announcement).toBe('Brief version 2 rejected.');
    expect(briefReads).toBe(1);
    controller.destroy();
  });

  it('reports a generated version and reloads the brief', async () => {
    let generated = false;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (request.method === 'POST' && url.pathname === '/api/v1/people/7/brief/generate') {
        generated = true;
        return Response.json({ run_id: 'run-1', attempt_id: 'attempt-1', brief_version: 3, brief_failure_class: '' });
      }
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(true));
      if (url.pathname === '/api/v1/people/7/brief') return Response.json(brief(generated ? { version: 3 } : {}));
      throw new Error(`Unexpected ${request.method} ${url.pathname}`);
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));
    await controller.setPerson(7);

    const outcome = await controller.generate();

    expect(outcome.kind).toBe('confirmed');
    expect(controller.runOutcome).toBe('Brief version 3 generated.');
    expect(controller.brief?.version).toBe(3);
    expect(controller.actionError).toBeNull();
    controller.destroy();
  });

  it('reports a bounded failure class when an attempt stores no version', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (request.method === 'POST') {
        return Response.json({ run_id: 'run-1', attempt_id: 'attempt-1', brief_version: 0, brief_failure_class: 'budget' });
      }
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(true));
      return Response.json({ error: 'person_brief_not_found' }, { status: 404 });
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));
    await controller.setPerson(7);

    const outcome = await controller.generate();

    expect(outcome.kind).toBe('confirmed');
    expect(controller.runOutcome).toBe('No new brief version. Reason: Provider budget exhausted.');
    expect(controller.brief).toBeUndefined();
    controller.destroy();
  });

  it('maps person_brief_not_enrolled on generate without inventing a version', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (request.method === 'POST') {
        return Response.json({ error: 'person_brief_not_enrolled', message: 'Enroll the person in briefs before generating one' }, { status: 409 });
      }
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(true));
      return Response.json({ error: 'person_brief_not_found' }, { status: 404 });
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));
    await controller.setPerson(7);

    const outcome = await controller.generate();

    expect(outcome.kind).toBe('error');
    expect(controller.actionError).toBe('Enroll this person in briefs before generating one.');
    expect(controller.runOutcome).toBeNull();
    controller.destroy();
  });

  it.each([
    ['person_brief_lane_disabled', 'Brief generation is turned off in the daemon configuration.'],
    ['person_brief_policy_refused',
      'The people inference profile does not allow sensitive content, which every brief carries.'],
    ['person_brief_no_supported_lane',
      'The people inference profile does not allow conversation text, the only source a brief reads in this version.'],
    ['person_brief_busy',
      'Another worker is sweeping this person right now; nothing was generated. Retry once it finishes.']
  ])('maps %s on generate to readable copy without inventing a version', async (code, copy) => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (request.method === 'POST') {
        return Response.json({ error: code, message: 'refused' }, { status: 409 });
      }
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(true));
      return Response.json({ error: 'person_brief_not_found' }, { status: 404 });
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));
    await controller.setPerson(7);

    const outcome = await controller.generate();

    expect(outcome.kind).toBe('error');
    expect(controller.actionError).toBe(copy);
    expect(controller.runOutcome).toBeNull();
    expect(controller.brief).toBeUndefined();
    controller.destroy();
  });

  it('loads the bounded version history on demand and reloads it after a rejection', async () => {
    const queries: string[] = [];
    let rejected = false;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (request.method === 'POST') {
        rejected = true;
        return Response.json(brief({ status: 'rejected', rejected_at: '2026-08-30T00:00:00Z', rejected_reason: '' }));
      }
      if (url.pathname === '/api/v1/people/7/brief/versions') {
        queries.push(url.search);
        return Response.json({
          versions: [
            brief(rejected ? { status: 'rejected', rejected_at: '2026-08-30T00:00:00Z' } : {}),
            brief({ version: 1, status: 'superseded', generated_at: '2026-08-01T00:00:00Z', superseded_at: '2026-08-29T18:42:10Z' })
          ]
        });
      }
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(true));
      if (url.pathname === '/api/v1/people/7/brief') return Response.json(brief());
      throw new Error(`Unexpected ${request.method} ${url.pathname}`);
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));
    await controller.setPerson(7);
    expect(controller.versionsShown).toBe(false);

    await controller.showVersions();

    expect(queries).toEqual(['?limit=20']);
    expect(controller.versionsShown).toBe(true);
    expect(controller.versions).toEqual([
      { version: 2, status: 'current', generated_at: '2026-08-29T18:42:10Z', rejected_at: null, rejected_reason: '', superseded_at: null },
      { version: 1, status: 'superseded', generated_at: '2026-08-01T00:00:00Z', rejected_at: null, rejected_reason: '', superseded_at: '2026-08-29T18:42:10Z' }
    ]);
    expect(JSON.stringify(controller.versions)).not.toMatch(/forbidden|rendered_text|role change/i);

    await controller.reject('');

    expect(queries).toEqual(['?limit=20', '?limit=20']);
    expect(controller.versions[0]?.status).toBe('rejected');
    expect(controller.brief).toBeUndefined();
    expect(controller.briefMissing).toBe(true);
    controller.destroy();
  });

  it('surfaces bounded lane errors without leaking the daemon message and retries each lane', async () => {
    let enrollmentReads = 0;
    let briefReads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (url.pathname === '/api/v1/people/7/brief-enrollment') {
        enrollmentReads += 1;
        if (enrollmentReads === 1) {
          return Response.json({ error: 'internal', message: forbidden.packetSHA }, { status: 500 });
        }
        return Response.json(enrollment(true));
      }
      if (url.pathname === '/api/v1/people/7/brief') {
        briefReads += 1;
        if (briefReads === 1) return Response.json({ error: 'internal', message: forbidden.packetSHA }, { status: 500 });
        return Response.json(brief());
      }
      throw new Error(`Unexpected ${request.method} ${url.pathname}`);
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));

    await controller.setPerson(7);
    expect(controller.enrollmentError).toBe('Unable to load brief enrollment.');
    expect(controller.enrollment).toBeUndefined();
    expect(briefReads).toBe(0);

    await controller.retryEnrollment();
    expect(controller.enrollmentError).toBeNull();
    expect(controller.briefError).toBe('Unable to load the brief.');

    await controller.retryBrief();
    expect(controller.briefError).toBeNull();
    expect(controller.brief?.version).toBe(2);
    expect(JSON.stringify([controller.enrollmentError, controller.briefError])).not.toContain(forbidden.packetSHA);
    controller.destroy();
  });

  it('keeps the paragraph when a version reports no sentence map', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const url = new URL(requestOf(input).url);
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(true));
      return Response.json(brief({ sentences: [] }));
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));

    await controller.setPerson(7);

    expect(controller.brief?.sentences).toEqual([]);
    expect(controller.brief?.rendered_text).toContain('Last time you talked');
    controller.destroy();
  });

  it('drops a sentence detail that its own structure cannot supply', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const url = new URL(requestOf(input).url);
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(true));
      return Response.json(brief({
        sentences: [{ kind: 'highlight', index: 4, text: 'Sentence without a structured item.' }]
      }));
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));

    await controller.setPerson(7);

    expect(controller.brief?.sentences).toEqual([{
      key: 'highlight:4', kind: 'highlight', index: 4,
      text: 'Sentence without a structured item.', detail: null, meta: [], evidence: []
    }]);
    controller.destroy();
  });

  it('falls back to no per-sentence citations when the daemon supplies no ordinals', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const url = new URL(requestOf(input).url);
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(true));
      return Response.json(brief({
        sentences: sentences().map(({ evidence_ordinals: _dropped, ...rest }) => rest)
      }));
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));

    await controller.setPerson(7);

    expect(controller.brief?.sentences).toHaveLength(6);
    for (const sentence of controller.brief?.sentences ?? []) {
      expect(sentence.evidence).toEqual([]);
    }
    expect(controller.brief?.evidence).toHaveLength(2);
    controller.destroy();
  });

  it('refuses a brief whose sentence citations are inconsistent with its own evidence', async () => {
    const inconsistent = [
      { name: 'an ordinal naming no pointer', sentences: [{ kind: 'highlight', index: 0, text: 'One.', evidence_ordinals: [7] }] },
      { name: 'a negative ordinal', sentences: [{ kind: 'highlight', index: 0, text: 'One.', evidence_ordinals: [-1] }] },
      { name: 'a non-array ordinal list', sentences: [{ kind: 'highlight', index: 0, text: 'One.', evidence_ordinals: 0 }] },
      {
        name: 'duplicate sentence keys',
        sentences: [
          { kind: 'highlight', index: 0, text: 'One.', evidence_ordinals: [0] },
          { kind: 'highlight', index: 0, text: 'One again.', evidence_ordinals: [0] }
        ]
      }
    ];
    for (const variant of inconsistent) {
      const fetchFn = vi.fn<typeof fetch>(async (input) => {
        const url = new URL(requestOf(input).url);
        if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(true));
        return Response.json(brief({ sentences: variant.sentences }));
      });
      const controller = new PersonBriefController(createAPIClient(fetchFn));

      await controller.setPerson(7);

      expect(controller.brief, variant.name).toBeUndefined();
      expect(controller.briefError, variant.name).toBe('Unable to load the brief.');
      expect(controller.briefMissing, variant.name).toBe(false);
      controller.destroy();
    }
  });

  it('treats a 404 with another error code as a failed read, not a missing brief', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const url = new URL(requestOf(input).url);
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(true));
      return Response.json(
        { error: 'person_profile_not_found', message: 'Person profile not found' },
        { status: 404 }
      );
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));

    await controller.setPerson(7);

    expect(controller.briefMissing).toBe(false);
    expect(controller.brief).toBeUndefined();
    expect(controller.briefError).toBe('Unable to load the brief.');
    controller.destroy();
  });

  it('settles the history loading flag when the owner hides it mid-request', async () => {
    const pending = deferredResponse();
    let reads = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (url.pathname === '/api/v1/people/7/brief/versions') {
        reads += 1;
        if (reads === 1) return pending.promise;
        return Response.json({ versions: [brief()] });
      }
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(true));
      if (url.pathname === '/api/v1/people/7/brief') return Response.json(brief());
      throw new Error(`Unexpected ${request.method} ${url.pathname}`);
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));
    await controller.setPerson(7);

    const abandoned = controller.showVersions();
    expect(controller.versionsLoading).toBe(true);
    controller.hideVersions();
    expect(controller.versionsLoading).toBe(false);
    expect(controller.versionsShown).toBe(false);
    pending.resolve(Response.json({ versions: [] }));
    await abandoned;
    expect(controller.versionsLoading).toBe(false);

    await controller.showVersions();

    expect(reads).toBe(2);
    expect(controller.versionsShown).toBe(true);
    expect(controller.versions).toHaveLength(1);
    expect(controller.versionsLoading).toBe(false);
    controller.destroy();
  });

  it('settles every loading flag when the person is replaced mid-request', async () => {
    const pending = deferredResponse();
    let call = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      call += 1;
      if (call === 1) return pending.promise;
      const url = new URL(request.url);
      if (url.pathname === '/api/v1/people/9/brief-enrollment') return Response.json({ ...enrollment(false), person_id: 9 });
      throw new Error(`Unexpected ${request.method} ${url.pathname}`);
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));

    const stale = controller.setPerson(7);
    expect(controller.enrollmentLoading).toBe(true);

    await controller.setPerson(9);

    expect(controller.enrollmentLoading).toBe(false);
    expect(controller.briefLoading).toBe(false);
    expect(controller.versionsLoading).toBe(false);
    pending.resolve(Response.json(enrollment(true)));
    await stale;
    expect(controller.enrollmentLoading).toBe(false);
    controller.destroy();
  });

  it('aborts every lane and clears state on person replacement', async () => {
    const first = deferredResponse();
    const signals: AbortSignal[] = [];
    let call = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      signals.push(request.signal);
      call += 1;
      if (call === 1) return first.promise;
      const url = new URL(request.url);
      if (url.pathname === '/api/v1/people/9/brief-enrollment') return Response.json({ ...enrollment(false), person_id: 9 });
      throw new Error(`Unexpected ${request.method} ${url.pathname}`);
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));

    const stale = controller.setPerson(7);
    const current = controller.setPerson(9);
    expect(signals[0]?.aborted).toBe(true);
    await current;
    first.resolve(Response.json(enrollment(true)));
    await stale;

    expect(controller.personID).toBe(9);
    expect(controller.enrollment).toEqual({ person_id: 9, enrolled: false, enabled_at: null });
    expect(controller.brief).toBeUndefined();
    controller.destroy();
  });

  it('suppresses a duplicate mutation while one is pending', async () => {
    const mutation = deferredResponse();
    let puts = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      const url = new URL(request.url);
      if (request.method === 'PUT') {
        puts += 1;
        return mutation.promise;
      }
      if (url.pathname === '/api/v1/people/7/brief-enrollment') return Response.json(enrollment(false));
      throw new Error(`Unexpected ${request.method} ${url.pathname}`);
    });
    const controller = new PersonBriefController(createAPIClient(fetchFn));
    await controller.setPerson(7);

    const first = controller.setEnrolled(true, false);
    const duplicate = await controller.setEnrolled(true, false);

    expect(duplicate).toEqual({ kind: 'ignored' });
    expect(puts).toBe(1);
    mutation.resolve(Response.json(enrollment(true)));
    await first;
    controller.destroy();
  });

  it('names every failure class it renders and falls back for an unknown one', () => {
    expect(briefFailureClassLabel('policy')).toBe('Policy limit');
    expect(briefFailureClassLabel('budget')).toBe('Provider budget exhausted');
    expect(briefFailureClassLabel('lease_lost')).toBe('Worker lease lost');
    expect(briefFailureClassLabel('rate_limited')).toBe('Provider rate limited');
    expect(briefFailureClassLabel('timeout')).toBe('Provider timed out');
    expect(briefFailureClassLabel('provider_http')).toBe('Provider HTTP error');
    expect(briefFailureClassLabel('invalid_output')).toBe('Invalid provider output');
    expect(briefFailureClassLabel('archive_gap')).toBe('Archive gap');
    expect(briefFailureClassLabel('internal')).toBe('Internal error');
    expect(briefFailureClassLabel('')).toBe('Not reported');
    expect(briefFailureClassLabel('forbidden-unknown-class')).toBe('Not reported');
  });
});
