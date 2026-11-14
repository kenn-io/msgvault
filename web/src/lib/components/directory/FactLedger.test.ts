import { employment, organization, requestOf } from '../../directory/entity-fixtures.test-support';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';

import { createAPIClient } from '../../api/client';
import { DirectoryEntityController } from '../../directory/entity-controller.svelte';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import { FactLedgerController } from '../../directory/fact-ledger-controller.svelte';
import { formatDateTime, formatShortDateTime } from '../../util/format';
import FactLedger from './FactLedger.svelte';

afterEach(() => cleanup());

const revision = `sha256:${'a'.repeat(64)}`;

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
    submitted_value: JSON.stringify('private-sensitive-value'), normalized_value: JSON.stringify('private-normalized-value'),
    relation: 'support', origin: 'extraction', kind: 'preference', status: 'stale', confidence: { reported_score: 80 },
    created_at: '2026-08-03T00:00:00Z', program_fingerprint: 'private', program_id: 'private',
    program_version: 'private', value_fingerprint: 'private'
  }] });
  if (path.endsWith('/fact-decisions')) return Response.json({ decisions: [] });
  if (path.endsWith('/fact-pins')) return Response.json({ pins: [] });
  throw new Error(`unexpected ${path}`);
}

function fieldDefinition(universal_id: string, label: string) {
  return { id: 8, universal_id, object_type: 'person' as const, slug: universal_id, label, value_type: 'text' as const, field_type: 'text' as const, cardinality: 'single' as const, display_order: 1, is_required: false, ownership: 'user' as const, ui_creatable: true, ui_editable: true, api_mutable: true, is_searchable: true, is_sensitive: false, is_audited: true, is_deletable: true, history_exempt: false, is_active: true, revision: 1, created_at: '2026-08-01', updated_at: '2026-08-01' };
}

function controller(): FactLedgerController {
  const value = new FactLedgerController(createAPIClient(vi.fn<typeof fetch>()));
  value.personID = 42;
  value.targets = [{ optionID: 'target-0', name: 'Private note', targetKey: 'note', description: 'Private note', kind: 'attribute', valueType: 'text', cardinality: 'single', sensitive: true, canonical: `attribute:note:sha256:${'a'.repeat(64)}` }];
  value.evidence.rows = [{ rowToken: 'evidence-0', sourceClass: 'archive', directness: 'direct-self', authority: 'ordinary', identityScore: 950,
    evidenceID: 1, eventTime: '2026-08-01', recordedTime: '2026-08-02', supported: true, currentSupportLabel: 'Supported — Source reimported', createdAt: '2026-08-03',
    excerpt: 'allowed evidence excerpt', evidenceKey: 'forbidden-evidence-key' }];
  value.claims.rows = [{ rowToken: 'claim-0', targetKind: 'attribute', targetKey: 'note', displayValue: 'allowed sensitive value', claimKey: 'claim', evidenceIDs: [], submittedValue: 'allowed sensitive value', normalizedValue: 'allowed normalized value', relation: 'support', origin: 'extraction', kind: 'fact', status: 'fresh',
    validFrom: '2026-01-01', validUntil: null, reportedScore: 88, createdAt: '2026-08-03', targetCanonical: value.targets[0]!.canonical }];
  value.decisions.rows = [{ rowToken: 'decision-0', claimKey: 'claim', displayValue: 'allowed sensitive value', parsedValue: 'allowed sensitive value', targetKind: 'attribute', outcome: 'saved', targetKey: 'note', targetCanonical: value.targets[0]!.canonical, scoreMaxima: { authority: 300, confidence: 100, corroboration: 150, directness: 400, freshness: 100, sourceClass: 500, total: 1550 }, action: 'applied', reason: 'applied-projection', score: { authority: 100, confidence: 8, corroboration: 0,
    directness: 400, freshness: 100, sourceClass: 300, total: 908 }, projectionKind: 'person_attribute', createdAt: '2026-08-03' }];
  value.pins.rows = [{ rowToken: 'pin-0', kind: 'attribute', description: 'Private note', pinned: true, targetCanonical: value.targets[0]!.canonical }];
  return value;
}

describe('FactLedger', () => {
  it('reveals only the selected decision claim', async () => {
    const value = controller();
    value.selectedSection = 'decisions';
    value.decisions.rows.push({ ...value.decisions.rows[0]!, rowToken: 'decision-1', claimKey: 'another', displayValue: 'another private value', parsedValue: 'another private value' });
    render(FactLedger, { controller: value });
    const rows = screen.getAllByLabelText('Historical fact decision');
    expect(rows[0]!.textContent).toContain('Saved: Hidden');
    expect(rows[1]!.textContent).not.toContain('another private value');
    await fireEvent.click(within(rows[0]!).getByRole('button', { name: 'Reveal sensitive fact value' }));
    expect(rows[0]!.textContent).toContain('allowed sensitive value');
    expect(rows[1]!.textContent).not.toContain('another private value');
    await fireEvent.click(within(rows[0]!).getByRole('button', { name: 'Hide sensitive fact value' }));
    expect(rows[0]!.textContent).not.toContain('allowed sensitive value');
  });

  it('uses the value carried by a decision outside the claims page', () => {
    const value = controller();
    value.selectedSection = 'decisions';
    value.claims.rows = [];
    value.targets[0] = { ...value.targets[0]!, sensitive: false };
    Object.assign(value.decisions.rows[0]!, { displayValue: 'New York', parsedValue: 'New York' });

    render(FactLedger, { controller: value });

    expect(screen.getByLabelText('Historical fact decision').textContent).toContain('Saved: New York');
  });

  it('keeps a decision value hidden until its claim key is revealed', async () => {
    const value = controller();
    value.selectedSection = 'decisions';
    value.claims.rows = [];
    Object.assign(value.decisions.rows[0]!, { displayValue: 'allowed sensitive decision value', parsedValue: 'allowed sensitive decision value' });
    render(FactLedger, { controller: value });

    const row = screen.getByLabelText('Historical fact decision');
    expect(row.textContent).toContain('Saved: Hidden');
    expect(row.textContent).not.toContain('allowed sensitive decision value');
    await fireEvent.click(within(row).getByRole('button', { name: 'Reveal sensitive fact value' }));
    expect(row.textContent).toContain('Saved: allowed sensitive decision value');
    await fireEvent.click(within(row).getByRole('button', { name: 'Hide sensitive fact value' }));
    expect(row.textContent).toContain('Saved: Hidden');
  });

  it('shows submitted job details', () => {
    const value = controller();
    value.targets[0] = { ...value.targets[0]!, kind: 'employment', targetKey: 'system:employment', canonical: `employment:system:employment:${revision}`, sensitive: false };
    const job = { title: 'Engineer', role: 'Lead', department: 'Research', location: 'Boston', organization: { name: 'Acme', domain: 'acme.example' }, start_date: { year: 2020, month: 3 }, end_date: { year: 2024 } };
    value.claims.rows[0] = { ...value.claims.rows[0]!, targetKind: 'employment', targetKey: 'system:employment', targetCanonical: `employment:system:employment:${revision}`, displayValue: 'Engineer at Acme', submittedValue: JSON.stringify(job), parsedValue: job };
    render(FactLedger, { controller: value });
    const claim = screen.getByLabelText('Fact claim');
    expect(claim.textContent).toContain('Lead · Research · Boston · acme.example · Started Mar 2020 · Ended 2024');
  });

  it('distinguishes saved past jobs with the same title and employer', () => {
    const value = controller();
    const canonical = `employment:system:employment:${revision}`;
    value.targets = [{ optionID: 'work', name: 'Work', targetKey: 'system:employment', description: 'Work', kind: 'employment', valueType: 'object', cardinality: 'multi', sensitive: false, canonical }];
    value.selectedTargetOption = 'work';
    const entity = new DirectoryEntityController(createAPIClient(vi.fn()), 42);
    entity.employments = [
      employment(21, 1, { person_id: 42, title: 'Engineer', role: 'Engineer', is_current: false, department: 'Design', start_date: { year: 2018, month: 4 }, end_date: { year: 2020, month: 7 } }),
      employment(22, 1, { person_id: 42, title: 'Engineer', role: 'Engineer', is_current: false, department: 'Research', start_date: { year: 2021, month: 1 }, end_date: { year: 2024, month: 2 } }),
    ];
    entity.organizations = [organization()];
    render(FactLedger, { controller: value, entity });
    const saved = screen.getByRole('region', { name: 'Selected saved value' });
    expect(saved.textContent).toContain('Past · Design · Started Apr 2018 · Ended Jul 2020');
    expect(saved.textContent).toContain('Past · Research · Started Jan 2021 · Ended Feb 2024');
    entity.destroy();
  });

  it('shows submitted job details in the saved decision label', () => {
    const value = controller();
    value.targets[0] = { ...value.targets[0]!, name: 'Work', description: 'Work', kind: 'employment', targetKey: 'system:employment', canonical: `employment:system:employment:${revision}`, sensitive: false };
    value.selectedSection = 'decisions';
    Object.assign(value.claims.rows[0]!, { targetKind: 'employment', targetKey: 'system:employment', targetCanonical: value.targets[0]!.canonical, displayValue: 'Engineer at Acme', parsedValue: { title: 'Engineer', organization: { name: 'Acme' }, location: 'Boston' } });
    Object.assign(value.decisions.rows[0]!, { targetKind: 'employment', targetKey: 'system:employment', targetCanonical: value.targets[0]!.canonical,
      displayValue: 'Engineer at Acme', parsedValue: { title: 'Engineer', organization: { name: 'Acme' }, location: 'Boston' } });
    render(FactLedger, { controller: value });
    expect(screen.getByLabelText('Historical fact decision').textContent).toContain('Saved: Engineer at Acme · Boston');
  });

  it('hides a decision value when target sensitivity is unknown', () => {
    const value = controller();
    value.selectedSection = 'decisions';
    value.claims.rows = [];
    value.targets = [];
    render(FactLedger, { controller: value });
    const row = screen.getByLabelText('Historical fact decision');
    expect(row.textContent).toContain('Saved: Hidden');
    expect(row.textContent).not.toContain('allowed sensitive value');
    expect(within(row).queryByRole('button', { name: 'Reveal sensitive fact value' })).toBeNull();
  });

  it('shows claim validity, confidence, and creation time in Details', async () => {
    const value = controller();
    const created = new Date(new Date().getFullYear(), 0, 15, 13, 45, 23);
    value.claims.rows[0]!.createdAt = created.toISOString();
    render(FactLedger, { controller: value });

    await fireEvent.click(screen.getByRole('button', { name: 'Details' }));
    expect(screen.getByText('Valid').nextElementSibling?.textContent).toContain('No end');
    expect(screen.getByText('Reported confidence').nextElementSibling?.textContent).toBe('88 of 1000');
    const timestamp = screen.getByText('Created').nextElementSibling!.querySelector('time')!;
    expect(timestamp.textContent).toBe(formatShortDateTime(created.toISOString()));
    expect(timestamp.title).toBe(formatDateTime(created.toISOString(), 'long'));
  });

  it('reveals evidence excerpts without exposing internal keys', async () => {
    const value = controller();
    value.selectedSection = 'evidence';
    value.evidence.rows[0]!.supported = false;
    value.evidence.rows[0]!.currentSupportLabel = 'Unsupported — Source deleted';
    const rendered = render(FactLedger, { controller: value });
    expect(screen.getByText('Unsupported — Source deleted')).toBeDefined();
    expect(rendered.container.innerHTML).not.toContain('allowed evidence excerpt');
    expect(rendered.container.innerHTML).not.toContain('forbidden-evidence-key');
    expect(rendered.container.innerHTML).not.toContain('attribute:note:sha256');
    await fireEvent.click(screen.getByRole('button', { name: 'Reveal evidence excerpt' }));
    expect(screen.getByText(/allowed evidence excerpt/)).toBeDefined();
  });

  it('uses readable source and date labels on linked evidence cards', () => {
    const value = controller();
    value.selectedSection = 'evidence';
    render(FactLedger, { controller: value });

    const card = screen.getByLabelText('Fact evidence');
    expect(card.textContent).toContain('From a message they sent');
    expect(card.textContent).toContain(formatShortDateTime('2026-08-01T00:00:00Z'));
    expect(card.textContent).not.toContain('archive');
    expect(card.textContent).not.toContain('direct-self');
    expect(card.textContent).not.toContain('2026-08-01T00:00:00Z');
  });

  it('reveals sensitive claims', async () => {
    const value = controller();
    const rendered = render(FactLedger, { controller: value });
    expect(screen.getByText('fact')).toBeDefined();
    expect(screen.getByText('fresh')).toBeDefined();
    expect(rendered.container.innerHTML).not.toContain('attribute:note:sha256');
    expect(rendered.container.innerHTML).not.toContain('allowed sensitive value');
    await fireEvent.click(screen.getByRole('button', { name: 'Reveal sensitive fact value' }));
    expect(screen.getByText('allowed sensitive value')).toBeDefined();
  });

  it('marks sensitive fields in the picker', async () => {
    render(FactLedger, { controller: controller() });
    await fireEvent.click(screen.getByRole('button', { name: 'Search fields: All fields' }));
    expect(screen.getByRole('option', { name: 'Private note Sensitive' })).toBeDefined();
  });

  it('retries a failed field catalog', async () => {
    const value = controller();
    value.catalogError = "Couldn't load fields.";
    const retry = vi.spyOn(value, 'retryCatalog').mockResolvedValue();
    render(FactLedger, { controller: value });
    expect(screen.getByRole('alert').textContent).toContain("Couldn't load fields.");
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    expect(retry).toHaveBeenCalledOnce();
  });

  it('does not repeat Work below a Work decision', () => {
    const value = controller();
    value.selectedSection = 'decisions';
    const canonical = `employment:system:employment:${revision}`;
    value.targets[0] = { ...value.targets[0]!, name: 'Work', description: 'Work', kind: 'employment', targetKey: 'system:employment', canonical, sensitive: false };
    value.claims.rows[0] = { ...value.claims.rows[0]!, targetKind: 'employment', targetKey: 'system:employment', targetCanonical: canonical, displayValue: 'Engineer at Acme', parsedValue: { title: 'Engineer', organization: { name: 'Acme' } } };
    value.decisions.rows[0] = { ...value.decisions.rows[0]!, targetKind: 'employment', targetKey: 'system:employment', targetCanonical: canonical, projectionKind: 'employment' };
    render(FactLedger, { controller: value });
    const row = screen.getByLabelText('Historical fact decision');
    expect(within(row).getAllByText('Work')).toHaveLength(1);
  });

  it('names the field on each row only while every field is shown', async () => {
    const value = controller();
    value.selectedSection = 'claims';
    render(FactLedger, { controller: value });
    expect(within(screen.getByLabelText('Fact claim')).queryByText('Private note')).not.toBeNull();
    value.selectedTargetOption = 'target-0';
    await tick();
    expect(within(screen.getByLabelText('Fact claim')).queryByText('Private note')).toBeNull();
    value.selectedSection = 'decisions';
    await tick();
    expect(within(screen.getByLabelText('Historical fact decision')).queryByText('Private note')).toBeNull();
  });

  it('shows the choice label for a boolean claim', () => {
    const value = controller();
    value.targets[0]!.sensitive = false;
    value.targets[0]!.valueType = 'boolean';
    value.claims.rows[0]!.submittedValue = 'true';
    value.claims.rows[0]!.parsedValue = true;
    value.claims.rows[0]!.displayValue = 'Yes';
    const definition = { ...fieldDefinition('note', 'Available'), value_type: 'boolean' as const, field_type: 'boolean' as const, options: { choices: [{ value: 'true', label: 'Ready' }] } };
    const profile = new DirectoryProfileController(createAPIClient(vi.fn()), 42, { attributes: { person_id: 42, attributes: [{ definition, current: [], history: [] }] }, etags: {}, errors: {} });
    render(FactLedger, { controller: value, profile });
    expect(screen.getByText('Ready')).toBeDefined();
    profile.destroy();
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
      value_type: 'text', cardinality: 'single', sensitive: true, choices: null, fields: null,
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
        value_type: 'text', cardinality: 'single', choices: null, fields: null,
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
    expect(screen.getByText('Unknown field')).toBeDefined();
    expect(rendered.container.textContent).not.toContain('private-note');
    expect(screen.getByRole('status', { name: 'Fact value hidden until target sensitivity is verified.' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Reveal sensitive fact value' })).toBeNull();
  });

  it.each([
    { name: 'replacement person', replace: (value: FactLedgerController) => value.applyContext(true, 43, false) },
    { name: 'same-person history restoration', replace: (value: FactLedgerController) => value.applyContext(true, 42, true) }
  ])('does not apply queued focus after $name invalidates its owner', async ({ replace }) => {
    const value = controller();
    value.active = true;
    render(FactLedger, { controller: value });
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
    outside.remove();
    value.destroy();
  });



  it('omits scores for a decision rejected before scoring', () => {
    const value = controller();
    value.selectedSection = 'decisions';
    Object.assign(value.decisions.rows[0]!, {
      action: 'policy-rejected', reason: 'sensitive-policy', outcome: 'not-used', projectionKind: null,
      score: { authority: 0, confidence: 0, corroboration: 0, directness: 0, freshness: 0, sourceClass: 0, total: 0 },
    });
    render(FactLedger, { controller: value });
    const row = screen.getByLabelText('Historical fact decision');
    expect(row.textContent).not.toContain('Claim score:');
    expect(within(row).queryByRole('button', { name: 'Details' })).toBeNull();
  });

  it.each([
    {
      maxima: true,
      total: 'Claim score: 908 of 1550',
      parts: ['Source: 300 of 500', 'Directness: 400 of 400', 'Authority: 100 of 300', 'Corroboration: 0 of 150', 'Freshness: 100 of 100', 'Confidence: 8 of 100'],
    },
    {
      maxima: false,
      total: 'Claim score: 908',
      parts: ['Source: 300', 'Directness: 400', 'Authority: 100', 'Corroboration: 0', 'Freshness: 100', 'Confidence: 8'],
    },
  ])('shows recorded scores with maxima=$maxima', async ({ maxima, total, parts }) => {
    const value = controller();
    value.selectedSection = 'decisions';
    if (!maxima) value.decisions.rows[0]!.scoreMaxima = null;
    render(FactLedger, { controller: value });
    const row = screen.getByLabelText('Historical fact decision');
    await fireEvent.click(within(row).getByRole('button', { name: 'Details' }));
    expect(within(row).getAllByRole('listitem').map(item => item.textContent)).toEqual(parts);
    expect(row.querySelector('.meta')?.textContent).toContain(total);
  });

  it.each([
    { sensitivity: 'visible', sensitive: false, revisionMatches: true, expected: 'Saved: allowed sensitive value' },
    { sensitivity: 'unknown', sensitive: false, revisionMatches: false, expected: 'Saved: Hidden' },
  ])('shows a $sensitivity saved decision value safely', ({ sensitive, revisionMatches, expected }) => {
    const value = controller();
    value.selectedSection = 'decisions';
    value.targets[0]!.sensitive = sensitive;
    if (!revisionMatches) value.decisions.rows[0] = { ...value.decisions.rows[0]!, targetCanonical: `attribute:note:sha256:${'b'.repeat(64)}` };
    render(FactLedger, { controller: value });
    const decision = screen.getByLabelText('Historical fact decision');
    expect(decision.querySelector('.value > span')?.textContent).toBe(expected);
  });

  it.each([
    { caseName: 'one source', evidenceIDs: [1], available: true, button: /show evidence/i, expectedID: 1 },
    { caseName: 'two loaded sources', evidenceIDs: [1, 2], available: true, button: 'Show source 2: From a message they sent', expectedID: 2 },
    { caseName: 'a source without loaded evidence', evidenceIDs: [1, 2], available: false, button: 'Show source 2', expectedID: 2 },
  ])('offers claim sources in Details for $caseName', async ({ evidenceIDs, available, button, expectedID }) => {
    const value = controller();
    value.targets[0]!.sensitive = false;
    value.claims.rows[0] = { ...value.claims.rows[0]!, evidenceIDs };
    if (!available) value.evidence.rows = [];
    else if (evidenceIDs.length > 1) value.evidence.rows.push({ ...value.evidence.rows[0]!, evidenceID: 2, rowToken: 'evidence-1' });
    const navigate = vi.spyOn(value, 'showEvidence').mockResolvedValue();
    render(FactLedger, { controller: value });
    await fireEvent.click(screen.getByRole('button', { name: 'Details' }));
    if (available && evidenceIDs.length > 1) expect(screen.getByRole('button', { name: 'Show source 1: From a message they sent' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: button }));
    expect(navigate).toHaveBeenCalledWith(expectedID, value.claims.rows[0]!.targetCanonical);
  });

  it('focuses a related decision when requested', async () => {
    const value = controller();
    value.selectedSection = 'decisions';
    value.focusRequest = { key: 'decision:claim', contextGeneration: value['contextGeneration'], nonce: 1 };
    render(FactLedger, { controller: value });
    await vi.waitFor(() => expect(screen.getByLabelText('Historical fact decision').contains(document.activeElement)).toBe(true));
    value.destroy();
  });

});
