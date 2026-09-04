import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';

import { createAPIClient } from '../../api/client';
import { FactLedgerController } from '../../directory/fact-ledger-controller.svelte';
import FactLedger from './FactLedger.svelte';

afterEach(() => cleanup());

const revision = `sha256:${'a'.repeat(64)}`;

function requestOf(input: RequestInfo | URL): Request {
  return input instanceof Request ? input : new Request(input);
}

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((next) => { resolve = next; });
  return { promise, resolve };
}

function ledgerResponse(path: string): Response {
  if (path.endsWith('/fact-evidence')) return Response.json({ evidence: [] });
  if (path.endsWith('/fact-claims')) return Response.json({ claims: [{
    id: 1, claim_key: 'private-claim-key', generation_id: 2, evidence_ids: [],
    target: { kind: 'attribute', key: 'private-note', revision },
    submitted_value: 'private-sensitive-value', normalized_value: 'private-normalized-value',
    relation: 'equals', origin: 'observed', confidence: { reported_score: 80 },
    created_at: '2026-08-03T00:00:00Z', program_fingerprint: 'private', program_id: 'private',
    program_version: 'private', value_fingerprint: 'private'
  }] });
  if (path.endsWith('/fact-decisions')) return Response.json({ decisions: [] });
  if (path.endsWith('/fact-pins')) return Response.json({ pins: [] });
  throw new Error(`unexpected ${path}`);
}

function controller(): FactLedgerController {
  const value = new FactLedgerController(createAPIClient(vi.fn<typeof fetch>()));
  value.personID = 42;
  value.targets = [{ optionID: 'target-0', description: 'Private note', kind: 'attribute', valueType: 'string', cardinality: 'single', sensitive: true, canonical: `attribute:note:sha256:${'a'.repeat(64)}` }];
  value.evidence.rows = [{ rowToken: 'evidence-0', sourceClass: 'message', directness: 'direct', authority: 'first party', identityScore: 92,
    eventTime: '2026-08-01', recordedTime: '2026-08-02', supported: true, currentSupportLabel: 'Source reimported', createdAt: '2026-08-03',
    excerpt: 'allowed evidence excerpt', evidenceKey: 'forbidden-evidence-key' }];
  value.claims.rows = [{ rowToken: 'claim-0', submittedValue: 'allowed sensitive value', normalizedValue: 'allowed normalized value', relation: 'equals', origin: 'observed',
    validFrom: '2026-01-01', validUntil: null, reportedScore: 88, createdAt: '2026-08-03', targetCanonical: value.targets[0]!.canonical }];
  value.decisions.rows = [{ rowToken: 'decision-0', action: 'project', reason: 'highest score', score: { authority: 1, confidence: 2, corroboration: 3,
    directness: 4, freshness: 5, sourceClass: 6, total: 21 }, projectionKind: 'attribute', createdAt: '2026-08-03' }];
  value.pins.rows = [{ rowToken: 'pin-0', description: 'Private note', kind: 'attribute', pinned: true, targetCanonical: value.targets[0]!.canonical }];
  return value;
}

describe('FactLedger', () => {
  it('renders allow-listed fields and keeps excerpts and sensitive values row-locally absent until reveal', async () => {
    const value = controller();
    vi.spyOn(value, 'selectSection').mockImplementation(async (section) => { value.selectedSection = section; });
    const rendered = render(FactLedger, { controller: value });

    expect(screen.getByText('message')).toBeDefined();
    expect(screen.getByText(/Source reimported/)).toBeDefined();
    expect(rendered.container.innerHTML).not.toContain('allowed evidence excerpt');
    expect(rendered.container.innerHTML).not.toContain('forbidden-evidence-key');
    await fireEvent.click(screen.getByRole('button', { name: 'Reveal evidence excerpt' }));
    expect(screen.getByText('allowed evidence excerpt')).toBeDefined();

    await fireEvent.click(screen.getByRole('radio', { name: 'Claims' }));
    expect(rendered.container.innerHTML).not.toContain('allowed sensitive value');
    await fireEvent.click(screen.getByRole('button', { name: 'Reveal sensitive fact value' }));
    expect(screen.getByText('allowed sensitive value')).toBeDefined();
    expect(screen.getByText('Reported confidence 88')).toBeDefined();

    await fireEvent.click(screen.getByRole('radio', { name: 'Decisions' }));
    expect(screen.getByText(/Total 21/)).toBeDefined();
    expect(screen.getByText('Projection attribute')).toBeDefined();

    await fireEvent.click(screen.getByRole('radio', { name: 'Pins' }));
    expect(screen.getByText(/Pinned/)).toBeDefined();
    expect(screen.queryByRole('button', { name: /pin/i })).toBeNull();
  });

  it('renders target metadata without putting canonical keys into the DOM', () => {
    const value = controller();
    const rendered = render(FactLedger, { controller: value });

    expect(screen.getByText(/Private note/)).toBeDefined();
    expect(screen.getByText(/Sensitive/)).toBeDefined();
    expect(rendered.container.innerHTML).not.toContain('attribute:note:sha256');
  });

  it('fails closed while the target catalog is deferred, then permits only explicit sensitive reveal', async () => {
    const catalog = deferredResponse();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      return path.endsWith('/person-fact-targets') ? catalog.promise : ledgerResponse(path);
    });
    const value = new FactLedgerController(createAPIClient(fetchFn));
    value.applyContext(true, 42, false);
    await vi.waitFor(() => expect(value.claims.rows).toHaveLength(1));
    value.selectedSection = 'claims';
    const rendered = render(FactLedger, { controller: value });

    expect(rendered.container.innerHTML).not.toContain('private-sensitive-value');
    expect(rendered.container.innerHTML).not.toContain('private-normalized-value');
    expect(screen.getByRole('status', { name: 'Fact value hidden until target sensitivity is verified.' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Reveal sensitive fact value' })).toBeNull();

    catalog.resolve(Response.json({ fingerprint: 'private', version: 'private', targets: [{
      description: 'Private note', kind: 'attribute', key: 'private-note', revision,
      value_type: 'string', cardinality: 'single', sensitive: true, choices: null, fields: null,
      slug: 'private', universal_id: 'private'
    }] }));
    await vi.waitFor(() => expect(screen.getByRole('button', { name: 'Reveal sensitive fact value' })).toBeDefined());
    expect(rendered.container.innerHTML).not.toContain('private-sensitive-value');
  });

  it.each(['failed', 'malformed'] as const)('fails closed when the target catalog is %s', async (mode) => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(requestOf(input).url).pathname;
      if (!path.endsWith('/person-fact-targets')) return ledgerResponse(path);
      if (mode === 'failed') return Response.json({ message: 'private-sensitive-value' }, { status: 503 });
      return Response.json({ fingerprint: 'private', version: 'private', targets: [{
        description: 'Malformed private note', kind: 'attribute', key: 'private-note', revision,
        value_type: 'string', cardinality: 'single', choices: null, fields: null,
        slug: 'private', universal_id: 'private'
      }] });
    });
    const value = new FactLedgerController(createAPIClient(fetchFn));
    value.applyContext(true, 42, false);
    await vi.waitFor(() => expect(value.initialLoading).toBe(false));
    value.selectedSection = 'claims';
    const rendered = render(FactLedger, { controller: value });

    expect(rendered.container.innerHTML).not.toContain('private-sensitive-value');
    expect(rendered.container.innerHTML).not.toContain('private-normalized-value');
    expect(screen.getByRole('status', { name: 'Fact value hidden until target sensitivity is verified.' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Reveal sensitive fact value' })).toBeNull();
  });

  it.each([
    { name: 'replacement person', replace: (value: FactLedgerController) => value.applyContext(true, 43, false) },
    { name: 'same-person history restoration', replace: (value: FactLedgerController) => value.applyContext(true, 42, true) }
  ])('does not apply queued focus after $name invalidates its owner', async ({ replace }) => {
    const value = controller();
    value.active = true;
    const rendered = render(FactLedger, { controller: value });
    const outside = document.createElement('button');
    document.body.append(outside);
    value.historyTriggerToken = 'missing-row';
    value.closeEvidenceHistory();

    await tick();
    outside.focus();
    replace(value);
    await tick();

    expect(document.activeElement).toBe(outside);
    expect(document.activeElement?.isConnected).toBe(true);
    expect(rendered.container.querySelector('#fact-evidence-heading')).not.toBe(document.activeElement);
    outside.remove();
    value.destroy();
  });

  it('renders an explicit first-page action for each paged section', async () => {
    const value = controller();
    vi.spyOn(value, 'selectSection').mockImplementation(async (section) => { value.selectedSection = section; });
    render(FactLedger, { controller: value });

    expect(screen.getByRole('button', { name: 'First evidence page' })).toHaveProperty('disabled', true);
    await fireEvent.click(screen.getByRole('radio', { name: 'Claims' }));
    expect(screen.getByRole('button', { name: 'First claims page' })).toHaveProperty('disabled', true);
    await fireEvent.click(screen.getByRole('radio', { name: 'Decisions' }));
    expect(screen.getByRole('button', { name: 'First decisions page' })).toHaveProperty('disabled', true);
  });
});
