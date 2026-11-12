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
    relation: 'support', origin: 'extraction', confidence: { reported_score: 80 },
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
  value.targets = [{ optionID: 'target-0', name: 'Private note', targetKey: 'note', description: 'Private note', kind: 'attribute', valueType: 'string', cardinality: 'single', sensitive: true, canonical: `attribute:note:sha256:${'a'.repeat(64)}` }];
  value.evidence.rows = [{ rowToken: 'evidence-0', sourceClass: 'archive', directness: 'direct-self', authority: 'ordinary', identityScore: 950,
    sourceURL: null, evidenceID: 1, eventTime: '2026-08-01', recordedTime: '2026-08-02', supported: true, lostSupport: null, createdAt: '2026-08-03',
    excerpt: 'allowed evidence excerpt', evidenceKey: 'forbidden-evidence-key' }];
  value.claims.rows = [{ rowToken: 'claim-0', targetKind: 'attribute', targetKey: 'note', displayValue: 'allowed sensitive value', claimKey: 'claim', evidenceIDs: [], submittedValue: 'allowed sensitive value', normalizedValue: 'allowed normalized value', relation: 'support', origin: 'extraction',
    validFrom: '2026-01-01', validUntil: null, reportedScore: 88, createdAt: '2026-08-03', targetCanonical: value.targets[0]!.canonical }];
  value.decisions.rows = [{ rowToken: 'decision-0', claimKey: 'claim', outcome: 'saved', targetKey: 'note', targetCanonical: value.targets[0]!.canonical, scoreMaxima: { authority: 300, confidence: 100, corroboration: 150, directness: 400, freshness: 100, sourceClass: 500, total: 1550 }, action: 'applied', reason: 'applied-projection', score: { authority: 100, confidence: 8, corroboration: 0,
    directness: 400, freshness: 100, sourceClass: 300, total: 908 }, projectionKind: 'person_attribute', createdAt: '2026-08-03' }];
  value.pins.rows = [{ kind: 'attribute', targetKey: 'note', pinned: true, targetCanonical: value.targets[0]!.canonical }];
  return value;
}

describe('FactLedger', () => {
  it.each([
    { recorded: '2026-08-01T12:00:59Z', distinct: true },
    { recorded: '2026-08-01T08:00:01-04:00', distinct: false },
  ])('shows Recorded only when the instant differs: $recorded', async ({ recorded, distinct }) => {
    const value = controller();
    value.selectedSection = 'evidence';
    value.evidence.rows[0]!.eventTime = '2026-08-01T12:00:01Z';
    value.evidence.rows[0]!.recordedTime = recorded;
    render(FactLedger, { controller: value });
    await fireEvent.click(screen.getByRole('button', { name: 'Details' }));
    expect(screen.getByText('Said').nextElementSibling?.querySelector('time')?.getAttribute('datetime')).toBe('2026-08-01T12:00:01Z');
    if (distinct) expect(screen.getByText('Recorded').nextElementSibling?.querySelector('time')?.getAttribute('datetime')).toBe(recorded);
    else expect(screen.queryByText('Recorded')).toBeNull();
  });

  it('shows submitted job details', () => {
    const value = controller();
    value.targets[0] = { ...value.targets[0]!, kind: 'employment', targetKey: 'system:employment', canonical: `employment:system:employment:${revision}`, sensitive: false };
    const job = { title: 'Engineer', role: 'Lead', department: 'Research', location: 'Boston', organization: { name: 'Acme', domain: 'acme.example' }, start_date: { year: 2020, month: 3 }, end_date: { year: 2024 } };
    value.claims.rows[0] = { ...value.claims.rows[0]!, targetKind: 'employment', targetKey: 'system:employment', targetCanonical: `employment:system:employment:${revision}`, displayValue: 'Engineer at Acme', submittedValue: JSON.stringify(job), parsedValue: job };
    render(FactLedger, { controller: value });
    const claim = screen.getByLabelText('Fact claim');
    expect(within(claim).getByText('Engineer at Acme')).toBeDefined();
    expect(claim.textContent).toContain('Lead · Research · Boston · acme.example · Started Mar 2020 · Ended 2024');
  });

  it('shows submitted job details in the saved decision label', () => {
    const value = controller();
    value.targets[0] = { ...value.targets[0]!, kind: 'employment', targetKey: 'system:employment', canonical: `employment:system:employment:${revision}`, sensitive: false };
    value.selectedSection = 'decisions';
    value.claims.rows[0] = { ...value.claims.rows[0]!, targetKind: 'employment', targetKey: 'system:employment', targetCanonical: `employment:system:employment:${revision}`, displayValue: 'Engineer at Acme', parsedValue: { title: 'Engineer', organization: { name: 'Acme' }, location: 'Boston' } };
    render(FactLedger, { controller: value });
    expect(screen.getByLabelText('Historical fact decision').textContent).toContain('Saved: Engineer at Acme · Boston');
  });

  it('shows claim validity, confidence, and creation time in Details', async () => {
    const value = controller();
    const created = new Date(new Date().getFullYear(), 0, 15, 13, 45, 23);
    value.claims.rows[0]!.createdAt = created.toISOString();
    render(FactLedger, { controller: value });

    await fireEvent.click(screen.getByRole('button', { name: 'Details' }));
    expect(screen.getByText('Valid').nextElementSibling?.textContent).toContain('No end');
    expect(screen.getByText('Reported confidence').nextElementSibling?.textContent).toBe('Weak · 88 of 1000');
    const timestamp = screen.getByText('Created').nextElementSibling!.querySelector('time')!;
    expect(timestamp.textContent).toBe(formatShortDateTime(created.toISOString()));
    expect(timestamp.title).toBe(formatDateTime(created.toISOString(), 'long'));
  });

  it('reveals private facts, shows Details labels, and marks sensitive fields in the picker', async () => {
    const value = controller();
    value.evidence.rows[0]!.supported = false;
    value.evidence.rows[0]!.lostSupport = 'Source deleted';
    vi.spyOn(value, 'selectSection').mockImplementation(async (section) => { value.selectedSection = section; });
    const rendered = render(FactLedger, { controller: value });

    await fireEvent.click(screen.getByRole('radio', { name: 'Evidence' }));
    expect(screen.getByLabelText('Fact evidence')).toBeDefined();
    expect(screen.getByText('Source deleted')).toBeDefined();
    expect(rendered.container.innerHTML).not.toContain('allowed evidence excerpt');
    expect(rendered.container.innerHTML).not.toContain('forbidden-evidence-key');
    expect(rendered.container.innerHTML).not.toContain('attribute:note:sha256');
    await fireEvent.click(screen.getByRole('button', { name: 'Reveal excerpt' }));
    expect(screen.getByText(/allowed evidence excerpt/)).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Details' }));
    expect(screen.getByText('Matches this person')).toBeDefined();

    await fireEvent.click(screen.getByRole('radio', { name: 'Claims' }));
    expect(screen.getByLabelText('Fact claim')).toBeDefined();
    expect(rendered.container.innerHTML).not.toContain('attribute:note:sha256');
    expect(rendered.container.innerHTML).not.toContain('allowed sensitive value');
    await fireEvent.click(screen.getByRole('button', { name: 'Reveal sensitive fact value' }));
    expect(screen.getByText('allowed sensitive value')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Details' }));
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

  it.each([true, false])('shows one pin notice beside a saved value while loading=%s', (loading) => {
    const value = controller();
    value.selectedTargetOption = 'target-0';
    value.pins.loading = loading;
    if (!loading) { value.pins.rows = []; value.pins.error = "Couldn't load pins."; }
    const definition = fieldDefinition('note', 'Note');
    const profile = new DirectoryProfileController(createAPIClient(vi.fn()), 42, { attributes: { person_id: 42, attributes: [{ definition, current: [{ id: 1, definition_id: 8, definition_slug: 'note', person_id: 42, ordinal: 0, active_from: '2026-08-01', value: { type: 'text', text: 'Boston' }, source: 'user', created_at: '2026-08-01' }], history: [] }] }, etags: {}, errors: {} });
    render(FactLedger, { controller: value, profile });
    const saved = screen.getByRole('region', { name: 'Selected saved value' });
    expect(saved.textContent).toContain(loading ? 'Loading pins…' : "Couldn't load pins.");
    expect(saved.textContent).not.toContain('Pinned');
    expect(saved.textContent).not.toContain('Unpinned');
    profile.destroy();
  });

  it('does not repeat Work below a Work decision', () => {
    const value = controller();
    value.selectedSection = 'decisions';
    const canonical = `employment:system:employment:${revision}`;
    value.targets[0] = { ...value.targets[0]!, name: 'Work', description: 'Work', kind: 'employment', targetKey: 'system:employment', canonical };
    value.decisions.rows[0] = { ...value.decisions.rows[0]!, targetKey: 'system:employment', targetCanonical: canonical };
    value.decisions.rows[0]!.projectionKind = 'employment';
    render(FactLedger, { controller: value });
    const row = screen.getByLabelText('Historical fact decision');
    expect(within(row).getAllByText('Work')).toHaveLength(1);
  });

  it('shows unpinned fields inside their fold', () => {
    const value = controller();
    value.pins.rows[0]!.pinned = false;
    value.selectedSection = 'pins';
    render(FactLedger, { controller: value });
    const fold = screen.getByText('1 unpinned field').closest('details');
    expect(fold?.open).toBe(false);
    expect(fold?.textContent).toContain('Unpinned');
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
    expect(screen.getByRole('status', { name: 'Hidden while field details are unavailable.' })).toBeDefined();
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
    expect(screen.getByText('Unknown field')).toBeDefined();
    expect(rendered.container.textContent).not.toContain('private-note');
    expect(screen.getByRole('status', { name: 'Hidden while field details are unavailable.' })).toBeDefined();
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



  it.each([
    { pinned: false, section: 'claims' as const, label: 'Selected saved value', expected: 'Unpinned · From a message' },
    { pinned: true, section: 'pins' as const, label: 'Home city', expected: 'Pinned · From a message' },
  ])('shows saved attribute provenance with pinned=$pinned', async ({ pinned, section, label, expected }) => {
    const value = controller();
    value.targets[0]!.sensitive = false;
    value.pins.rows[0]!.pinned = pinned;
    value.selectedSection = section;
    value.selectedTargetOption = 'target-0';
    const definition = fieldDefinition('note', 'Home city');
    const profile = new DirectoryProfileController(createAPIClient(vi.fn()), 42, { attributes: { person_id: 42, attributes: [{ definition, current: [{ id: 5, person_id: 42, definition_id: 8, definition_slug: 'note', ordinal: 0, value: { type: 'text', text: 'New York' }, source: 'extraction', source_ref: 'person-fact-decision:123', created_at: '2026-11-01T12:00:00Z', active_from: '2026-11-01T12:00:00Z' }], history: [] }] }, etags: {}, errors: {} });
    render(FactLedger, { controller: value, profile });
    const saved = screen.getByRole('region', { name: label });
    expect(within(saved).getByText('Home city', { selector: 'strong' })).toBeDefined();
    expect(saved.textContent).toContain(expected);
    if (!pinned) {
      expect(within(saved).getByText('Saved value')).toBeDefined();
      const claim = screen.getByLabelText('Fact claim');
      expect(saved.compareDocumentPosition(claim) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
      await fireEvent.click(within(saved).getByRole('button', { name: 'Details' }));
      expect(within(saved).getByText('Reference')).toBeDefined();
    }
    profile.destroy();
  });

  it.each(['current', 'past', 'none'] as const)('shows saved employment with %s jobs in comparison and Pins', async (state) => {
    const value = controller();
    const canonical = `employment:system:employment:${revision}`;
    value.targets = [{ optionID: 'work', name: 'Work', targetKey: 'system:employment', description: 'Work', kind: 'employment', valueType: 'object', cardinality: 'multi', sensitive: false, canonical }];
    value.pins.rows = [{ kind: 'employment', pinned: true, targetKey: 'system:employment', targetCanonical: canonical }];
    value.selectedTargetOption = 'work';
    const entity = new DirectoryEntityController(createAPIClient(vi.fn()), 42);
    const current = state === 'current';
    entity.employments = state === 'none' ? [] : [employment(11, 1, { is_current: current, is_primary: current, source: 'carddav_import', source_ref: 'card-3', created_at: '2026-11-01T12:00:00Z', updated_at: '2026-11-01T12:00:00Z' })];
    if (current) entity.employments.push(employment(12, 1, { title: 'Former role', is_current: false }));
    entity.organizations = [organization()];
    render(FactLedger, { controller: value, entity });
    const comparison = screen.getByRole('region', { name: 'Selected saved value' });
    expect(comparison.textContent?.includes('Engineer at Synthetic Org')).toBe(current);
    expect(comparison.textContent).not.toContain('Former role');
    if (!current) expect(comparison.textContent).toContain(`Work: ${state === 'past' ? 'No current job' : 'No saved value'}`);
    await fireEvent.click(screen.getByRole('radio', { name: 'Pins' }));
    const saved = screen.getByRole('region', { name: 'Saved employment' });
    expect(saved.textContent?.includes('Engineer at Synthetic Org')).toBe(current);
    expect(saved.textContent).not.toContain('Former role');
    if (!current) expect(saved.textContent).toContain(state === 'past' ? 'No current job' : 'No saved value');
    if (current) {
      expect(saved.textContent).toContain('Pinned · From CardDAV contacts');
      await fireEvent.click(within(saved).getByRole('button', { name: 'Details' }));
      expect(within(saved).getByText('card-3')).toBeDefined();
    }
    entity.destroy();
  });

  it.each([true, false])('shows score parts only for scored decisions: %s', async scored => {
    const value = controller();
    value.selectedSection = 'decisions';
    value.decisions.rows[0]!.scoreMaxima!.authority = 250;
    if (!scored) value.decisions.rows[0] = { ...value.decisions.rows[0]!, outcome: 'not-used', action: 'policy-rejected', reason: 'sensitive-policy', projectionKind: null, score: { authority: 0, confidence: 0, corroboration: 0, directness: 0, freshness: 0, sourceClass: 0, total: 0 } };
    render(FactLedger, { controller: value });
    const row = screen.getByLabelText('Historical fact decision');
    await fireEvent.click(within(row).getByRole('button', { name: 'Details' }));
    expect(within(row).queryAllByRole('listitem')).toHaveLength(scored ? 6 : 0);
    expect(row.textContent?.includes('Authority: 100 of 250')).toBe(scored);
    expect(row.textContent?.includes('Scored')).toBe(scored);
  });

  it('names All fields evidence and decisions', async () => {
    const value = controller();
    const claim = (index: number, claimKey: string, targetKey: string, evidenceIDs: number[]) => ({ ...value.claims.rows[0]!, rowToken: `claim-${index}`, claimKey, targetKey, evidenceIDs, targetCanonical: `attribute:${targetKey}:${revision}` });
    value.targets = [];
    value.claims.rows = [claim(0, 'a', 'city', [1, 2]), claim(1, 'b', 'employer', [2]), claim(2, 'c', 'city', [2])];
    value.evidence.rows = [1, 2, 3].map(id => ({ ...value.evidence.rows[0]!, rowToken: `evidence-${id}`, evidenceID: id }));
    value.decisions.rows = ['employer', 'missing'].map((targetKey, index) => ({ ...value.decisions.rows[0]!, rowToken: `decision-${index}`, targetKey, targetCanonical: `attribute:${targetKey}:${revision}` }));
    const profile = new DirectoryProfileController(createAPIClient(vi.fn()), 42, { attributes: { person_id: 42, attributes: [
      { definition: fieldDefinition('city', 'Home city'), current: [], history: [] }, { definition: fieldDefinition('employer', 'Employer'), current: [], history: [] }
    ] }, etags: {}, errors: {} });
    vi.spyOn(value, 'selectSection').mockImplementation(async (section) => { value.selectedSection = section; });
    const view = render(FactLedger, { controller: value, profile });
    const labels = () => Array.from(view.container.querySelectorAll('section[aria-label] .field-label'), node => node.textContent);

    await fireEvent.click(screen.getByRole('radio', { name: 'Evidence' }));
    expect(labels()).toEqual(['Home city', 'Home city, Employer', 'Evidence']);
    await fireEvent.click(screen.getByRole('radio', { name: 'Decisions' }));
    expect(labels()).toEqual(['Employer', 'Unknown field']);

    profile.destroy();
  });

  it.each([
    { sensitivity: 'visible', sensitive: false, revisionMatches: true, loaded: true, expected: 'Saved: allowed sensitive value' },
    { sensitivity: 'unknown', sensitive: false, revisionMatches: false, loaded: true, expected: 'Saved' },
    { sensitivity: 'unloaded', sensitive: false, revisionMatches: true, loaded: false, expected: 'Saved' },
  ])('shows a $sensitivity saved decision value safely', ({ sensitive, revisionMatches, loaded, expected }) => {
    const value = controller();
    value.selectedSection = 'decisions';
    value.targets[0]!.sensitive = sensitive;
    if (!revisionMatches) value.claims.rows[0] = { ...value.claims.rows[0]!, targetCanonical: `attribute:note:sha256:${'b'.repeat(64)}` };
    if (!loaded) value.claims.rows = [];
    render(FactLedger, { controller: value });
    const decision = screen.getByLabelText('Historical fact decision');
    expect(decision.querySelector('.value > span')?.textContent).toBe(expected);
  });

  it('names a sensitive claim in its decision only while it is revealed', async () => {
    const value = controller();
    vi.spyOn(value, 'selectSection').mockImplementation(async (section) => { value.selectedSection = section; });
    render(FactLedger, { controller: value });
    const sentence = () => screen.getByLabelText('Historical fact decision').querySelector('.value > span')?.textContent;
    await fireEvent.click(screen.getByRole('button', { name: 'Reveal sensitive fact value' }));
    await fireEvent.click(screen.getByRole('radio', { name: 'Decisions' }));
    expect(sentence()).toBe('Saved: allowed sensitive value');
    await fireEvent.click(screen.getByRole('radio', { name: 'Claims' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Hide sensitive fact value' }));
    await fireEvent.click(screen.getByRole('radio', { name: 'Decisions' }));
    expect(sentence()).toBe('Saved');
  });

  it.each([
    { evidenceIDs: [1], button: /show evidence/i, expectedID: 1 },
    { evidenceIDs: [1, 2], button: /Show source: Source 2/, expectedID: 2 },
  ])('offers claim sources in Details for $evidenceIDs', async ({ evidenceIDs, button, expectedID }) => {
    const value = controller();
    value.targets[0]!.sensitive = false;
    value.claims.rows[0] = { ...value.claims.rows[0]!, evidenceIDs, relation: 'supersede' };
    const navigate = vi.spyOn(value, 'showEvidence').mockResolvedValue();
    render(FactLedger, { controller: value });
    await fireEvent.click(screen.getByRole('button', { name: 'Details' }));
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
