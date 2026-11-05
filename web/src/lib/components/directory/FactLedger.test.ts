import { requestOf } from '../../directory/entity-fixtures.test-support';
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';

import { createAPIClient } from '../../api/client';
import { FactLedgerController } from '../../directory/fact-ledger-controller.svelte';
import FactLedger from './FactLedger.svelte';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import { DirectoryEntityController } from '../../directory/entity-controller.svelte';
import { employment, organization } from '../../directory/entity-fixtures.test-support';

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
    submitted_value: 'private-sensitive-value', normalized_value: 'private-normalized-value',
    relation: 'support', origin: 'extraction', confidence: { reported_score: 80 },
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
  value.targets = [{ optionID: 'target-0', name: 'Private note', description: 'Private note', kind: 'attribute', valueType: 'string', cardinality: 'single', sensitive: true, canonical: `attribute:note:sha256:${'a'.repeat(64)}` }];
  value.evidence.rows = [{ rowToken: 'evidence-0', sourceURL: null, sourceClass: 'message', directness: 'direct', authority: 'first party', identityScore: 92,
    eventTime: '2026-08-01', recordedTime: '2026-08-02', supported: true, currentSupportLabel: 'Source reimported', createdAt: '2026-08-03',
    excerpt: 'allowed evidence excerpt', evidenceKey: 'forbidden-evidence-key' }];
  value.claims.rows = [{ rowToken: 'claim-0', submittedValue: '"allowed sensitive value"', displayValue: 'allowed sensitive value', normalizedValue: '"allowed normalized value"', relation: 'support', origin: 'extraction',
    validFrom: '2026-01-01', validUntil: null, reportedScore: 88, createdAt: '2026-08-03', targetCanonical: value.targets[0]!.canonical }];
  value.decisions.rows = [{ rowToken: 'decision-0', action: 'project', reason: 'highest score', score: { authority: 1, confidence: 2, corroboration: 3,
    directness: 4, freshness: 5, sourceClass: 6, total: 21 }, projectionKind: 'attribute', createdAt: '2026-08-03' }];
  value.pins.rows = [{ kind: 'attribute', pinned: true, targetKey: 'note', targetCanonical: value.targets[0]!.canonical }];
  return value;
}

describe('FactLedger', () => {
  it('renders allow-listed fields and keeps excerpts and sensitive values row-locally absent until reveal', async () => {
    const value = controller();
    value.selectedSection = 'evidence';
    vi.spyOn(value, 'selectSection').mockImplementation(async (section) => { value.selectedSection = section; });
    const rendered = render(FactLedger, { controller: value });

    await fireEvent.click(screen.getByRole('radio', { name: 'Evidence' }));
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
    expect(screen.getByText(/^From a message ·/)).toBeDefined();
    await fireEvent.click(screen.getByText('Details', { selector: 'summary' }));
    expect(screen.getByText('Reported confidence').nextElementSibling?.textContent).toBe('88');

    await fireEvent.click(screen.getByRole('radio', { name: 'Decisions' }));
    expect(screen.getByText(/Total 21/)).toBeDefined();
    expect(screen.getByText('Projection attribute')).toBeDefined();

    await fireEvent.click(screen.getByRole('radio', { name: 'Pins' }));
    expect(screen.getByText('Saved values unavailable.')).toBeDefined();
    expect(screen.queryByRole('button', { name: /pin/i })).toBeNull();
  });

  it('shows schema in About without putting canonical keys into the DOM', async () => {
    const value = controller();
    const rendered = render(FactLedger, { controller: value });

    await fireEvent.click(screen.getByText('About these fields', { selector: 'summary' }));
    expect(screen.getByText('About these fields').closest('details')?.open).toBe(true);
    expect(screen.getByText(/attribute · string · single · Sensitive/)).toBeDefined();
    await fireEvent.click(screen.getByText('About these fields', { selector: 'summary' }));
    expect(screen.getByText('About these fields').closest('details')?.open).toBe(false);
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

  it('hides paging for single pages and exposes first-page actions on later pages', async () => {
    const value = controller();
    vi.spyOn(value, 'selectSection').mockImplementation(async (section) => { value.selectedSection = section; });
    render(FactLedger, { controller: value });

    expect(screen.queryByRole('navigation', { name: 'claims pages' })).toBeNull();
    value.evidence.offset = 50; value.claims.offset = 50; value.decisions.offset = 50;
    await fireEvent.click(screen.getByRole('radio', { name: 'Evidence' }));
    expect(screen.getByRole('button', { name: 'First evidence page' })).toHaveProperty('disabled', false);
    await fireEvent.click(screen.getByRole('radio', { name: 'Claims' }));
    expect(screen.getByRole('button', { name: 'First claims page' })).toHaveProperty('disabled', false);
    await fireEvent.click(screen.getByRole('radio', { name: 'Decisions' }));
    expect(screen.getByRole('button', { name: 'First decisions page' })).toHaveProperty('disabled', false);
  });
});

it('preserves claim order and keeps claim relations visible', async () => {
  const value = controller();
  value.targets[0]!.sensitive = false;
  value.targets[0]!.name = 'Location';
  const claim = value.claims.rows[0]!;
  value.claims.rows = [
    { ...claim, rowToken: 'unknown', targetCanonical: 'other', submittedValue: 'unverified' },
    { ...claim, rowToken: 'current', submittedValue: '"Current City"', displayValue: 'Current City' },
    { ...claim, rowToken: 'contradicted', submittedValue: '"Contradicted City"', displayValue: 'Contradicted City', relation: 'contradict' },
    { ...claim, rowToken: 'invalid', submittedValue: '"Invalid City"', displayValue: 'Invalid City', relation: 'invalid' },
    { ...claim, rowToken: 'superseded', submittedValue: '"Superseded City"', displayValue: 'Superseded City', relation: 'supersede', origin: 'system' },
    { ...claim, rowToken: 'future', submittedValue: '"Future City"', displayValue: 'Future City', validUntil: '2099-10-01' },
  ];
  const view = render(FactLedger, { controller: value });
  const rows = view.container.querySelectorAll('[aria-label="Fact claim"]');
  expect(rows[0]!.textContent).toContain('Fact value hidden');
  expect(rows[1]!.textContent).toContain('Current City');
  expect(rows[1]!.textContent).toContain('Location');
  expect(screen.getByText(/Superseded · Added by msgvault/)).toBeDefined();
  expect(rows[2]!.textContent).toContain('Contradicted City');
  expect(rows[2]!.textContent).toContain('Contradicted · From a message');
  expect(rows[3]!.textContent).toContain('Invalid · From a message');
  expect(rows[5]!.textContent).toContain('Future City');
});

it('searches short field names and keeps sensitive metadata separate', async () => {
  const value = controller();
  render(FactLedger, { controller: value });
  await fireEvent.click(screen.getByRole('button', { name: 'Search fields: All fields' }));
  await fireEvent.input(screen.getByRole('combobox'), { target: { value: 'Private' } });
  const option = screen.getByRole('option', { name: /Private note Sensitive/ });
  expect(option.querySelector('svg')?.getAttribute('width')).toBe('12');
  expect(option.querySelector('.kit-typeahead__option-meta')?.textContent).toBe('Sensitive');
});

it.each(['claims', 'evidence', 'decisions'] as const)('names the field in empty %s', async section => {
  const value = controller();
  value.selectedSection = section;
  value.selectedTargetOption = 'target-0';
  value[section].rows = [];
  render(FactLedger, { controller: value });
  expect(screen.getByText(`No ${section} for Private note yet.`)).toBeDefined();
});

it('shows saved employment and its source in Pins', async () => {
  const value = controller();
  value.selectedSection = 'pins';
  value.pins.rows = [{ kind: 'employment', pinned: true, targetKey: 'system:employment', targetCanonical: undefined }];
  const entity = new DirectoryEntityController(createAPIClient(vi.fn<typeof fetch>()), 42);
  entity.employments = [employment()];
  entity.organizations = [organization()];
  render(FactLedger, { controller: value, entity });
  expect(screen.getByText('Engineer · Synthetic Org')).toBeDefined();
  expect(screen.getByText(/You · Updated:/)).toBeDefined();
  entity.destroy();
});

it('shows Location beside a readable submitted value', async () => {
  const fetchFn = vi.fn<typeof fetch>(async input => {
    const path = new URL(requestOf(input).url).pathname;
    if (path.endsWith('/person-fact-targets')) return Response.json({ targets: [{
      kind: 'attribute', key: 'location', revision, slug: 'location', description: 'Location',
      value_type: 'text', cardinality: 'single', sensitive: false,
    }] });
    if (path.endsWith('/fact-claims')) return Response.json({ claims: [{
      target: { kind: 'attribute', key: 'location', revision }, submitted_value: '"Example City"',
      relation: 'support', origin: 'extraction', confidence: { reported_score: 88 }, created_at: '2026-08-03',
    }] });
    return ledgerResponse(path);
  });
  const value = new FactLedgerController(createAPIClient(fetchFn));
  value.applyContext(true, 42);
  await vi.waitFor(() => expect(value.initialLoading).toBe(false));
  value.selectedSection = 'claims';
  const view = render(FactLedger, { controller: value });
  const row = view.container.querySelector('[aria-label="Fact claim"]')!;
  expect(row.textContent).toContain('Location');
  expect(screen.getByText('Example City', { exact: true })).toBeDefined();
  value.destroy();
});

it('keeps original rejected JSON alongside relation and enrichment confidence', async () => {
  const submitted = '"42"';
  const fetchFn = vi.fn<typeof fetch>(async input => {
    const path = new URL(requestOf(input).url).pathname;
    if (path.endsWith('/person-fact-targets')) return Response.json({ targets: [{
      kind: 'attribute', key: 'location', revision, slug: 'location', description: 'Location',
      value_type: 'text', cardinality: 'single', sensitive: false,
    }] });
    if (path.endsWith('/fact-claims')) return Response.json({ claims: [{
      target: { kind: 'attribute', key: 'location', revision }, submitted_value: submitted,
      relation: 'invalid', origin: 'enrichment', confidence: { reported_score: 42 }, created_at: '2026-08-03',
    }] });
    return ledgerResponse(path);
  });
  const value = new FactLedgerController(createAPIClient(fetchFn));
  value.applyContext(true, 42);
  await vi.waitFor(() => expect(value.initialLoading).toBe(false));
  const view = render(FactLedger, { controller: value });
  expect(screen.getByText(/Invalid · From enrichment/)).toBeDefined();
  await fireEvent.click(screen.getByText('Details'));
  expect(screen.getByText('Origin').nextElementSibling?.textContent).toBe('enrichment');
  expect(screen.getByText('Reported confidence').nextElementSibling?.textContent).toBe('42');
  expect(view.container.querySelector('code')!.textContent).toBe(submitted);
  value.destroy();
});

it('distinguishes unavailable employment from an empty successful read and retries', async () => {
  let failed = true;
  const entity = new DirectoryEntityController(createAPIClient(async () => failed
    ? Response.json({ message: 'Employment unavailable' }, { status: 503 })
    : Response.json({ employments: [] })), 42);
  const value = controller();
  value.selectedSection = 'pins';
  value.pins.rows = [{ kind: 'employment', pinned: true, targetKey: 'employment', targetCanonical: undefined }];
  await entity.refreshEmployments();
  render(FactLedger, { controller: value, entity });
  expect(screen.getByText('Saved employment unavailable.')).toBeDefined();
  expect(screen.queryByText('No saved value')).toBeNull();
  failed = false;
  await fireEvent.click(screen.getByRole('button', { name: 'Retry employment' }));
  await screen.findByText('No saved value');
  entity.destroy();
});

it('treats a missing saved definition as unavailable rather than empty', () => {
  const value = controller();
  value.selectedSection = 'pins';
  const profile = new DirectoryProfileController(createAPIClient(vi.fn()), 42, {
    attributes: { person_id: 42, attributes: [] }, definitions: { definitions: [] }, etags: {}, errors: {},
  });
  render(FactLedger, { controller: value, profile });
  expect(screen.getByText('Saved field unavailable')).toBeDefined();
  expect(screen.queryByText('No saved value')).toBeNull();
  profile.destroy();
});

it('filters saved pins with claims while Pins keeps every saved field', async () => {
  const value = controller();
  value.selectedTargetOption = 'target-0';
  value.pins.rows = [{ kind: 'employment', pinned: true, targetKey: 'employment', targetCanonical: 'employment:other' }];
  const entity = new DirectoryEntityController(createAPIClient(vi.fn()), 42);
  entity.employments = [employment()];
  entity.organizations = [organization()];
  render(FactLedger, { controller: value, entity });
  expect(screen.queryByText('Engineer · Synthetic Org')).toBeNull();
  value.selectedSection = 'pins';
  await tick();
  expect(screen.getByText('Engineer · Synthetic Org')).toBeDefined();
  entity.destroy();
});

it('folds historical saved employment without claiming it is empty', async () => {
  const value = controller();
  value.selectedSection = 'pins';
  value.pins.rows = [{ kind: 'employment', pinned: true, targetKey: 'employment', targetCanonical: undefined }];
  const entity = new DirectoryEntityController(createAPIClient(vi.fn()), 42);
  entity.employments = [employment(11, 1, { is_current: false })];
  entity.organizations = [organization()];
  render(FactLedger, { controller: value, entity });
  expect(screen.queryByText('No saved value')).toBeNull();
  const disclosure = screen.getByText('1 earlier value', { selector: 'summary' });
  expect(disclosure.closest('details')?.open).toBe(false);
  await fireEvent.click(disclosure);
  expect(disclosure.closest('details')?.open).toBe(true);
  expect(screen.getByText('Engineer · Synthetic Org')).toBeDefined();
  entity.destroy();
});

it.each(['current', 'history'] as const)('keeps saved %s values when the separate definition list fails', async lane => {
  const value = controller();
  value.selectedSection = 'pins';
  const profile = savedProfile(lane, false);
  profile.definitionsError = 'Definitions unavailable';
  render(FactLedger, { controller: value, profile });
  expect(screen.queryByText('Saved values unavailable.')).toBeNull();
  expect(screen.queryByText('No saved value')).toBeNull();
  if (lane === 'history') await fireEvent.click(screen.getByText('1 earlier value', { selector: 'summary' }));
  expect(screen.getByText('Example City')).toBeDefined();
  profile.destroy();
});

function savedProfile(lane: 'current' | 'history' = 'current', sensitive = true): DirectoryProfileController {
  const when = '2026-08-01T00:00:00Z';
  const definition = {
    id: 8, universal_id: 'note', object_type: 'person' as const, slug: 'location', label: 'Location',
    value_type: 'text' as const, field_type: 'text' as const, cardinality: 'single' as const, display_order: 10,
    is_required: false, ownership: 'user' as const, ui_creatable: true, ui_editable: true, api_mutable: true,
    is_searchable: true, is_sensitive: sensitive, is_audited: true, is_deletable: true, history_exempt: false,
    is_active: true, revision: 1, created_at: when, updated_at: when,
  };
  const saved = { id: 1, person_id: 42, definition_id: 8, definition_slug: 'location', ordinal: 0,
    value: { type: 'text' as const, text: 'Example City' }, source: 'user', created_at: when, active_from: when, ...(lane === 'history' ? { active_until: when } : {}) };
  return new DirectoryProfileController(createAPIClient(vi.fn()), 42, {
    attributes: { person_id: 42, attributes: [{ definition, current: lane === 'current' ? [saved] : [], history: lane === 'history' ? [saved] : [] }] },
    etags: {}, errors: {},
  });
}

it('shows a select claim label from its saved definition and preserves the submitted token in Details', async () => {
  const profile = savedProfile('current', false);
  profile.attributes!.attributes[0]!.definition.options = { choices: [{ value: 'status_active', label: 'Active' }] };
  const value = new FactLedgerController(createAPIClient(async input => {
    const path = new URL(requestOf(input).url).pathname;
    if (path.endsWith('/person-fact-targets')) return Response.json({ targets: [{
      kind: 'attribute', key: 'note', revision, slug: 'status', description: 'Status', value_type: 'text', cardinality: 'single', sensitive: false,
    }] });
    if (path.endsWith('/fact-claims')) return Response.json({ claims: [{
      target: { kind: 'attribute', key: 'note', revision }, submitted_value: '"status_active"',
      relation: 'support', origin: 'extraction', confidence: { reported_score: 920 }, created_at: '2026-08-03',
    }] });
    return ledgerResponse(path);
  }));
  value.applyContext(true, 42);
  await vi.waitFor(() => expect(value.initialLoading).toBe(false));
  render(FactLedger, { controller: value, profile });
  expect(screen.getByText('Active')).toBeDefined();
  await fireEvent.click(screen.getByText('Details', { selector: 'summary' }));
  expect(screen.getByText('Submitted JSON').nextElementSibling?.textContent).toBe('"status_active"');
  expect(screen.getByText('Reported confidence').nextElementSibling?.textContent).toBe('920');
  value.destroy();
  profile.destroy();
});

it.each([
  ['extraction', 'From a message'], ['enrichment', 'From enrichment'], ['brief', 'From a brief'],
  ['system', 'Added by msgvault'], ['invalid', 'Invalid submission'],
])('shows readable provenance for %s claims', (origin, label) => {
  const value = controller();
  value.claims.rows[0]!.origin = origin;
  const view = render(FactLedger, { controller: value });
  expect(view.container.querySelector('[aria-label="Fact claim"] .meta')?.textContent).toMatch(new RegExp(`^${label} ·`));
});

it('requires a fresh saved-value reveal after filtering or changing tabs and preserves field identity on reorder', async () => {
  const value = controller();
  value.targets.push({ ...value.targets[0]!, optionID: 'target-1', canonical: 'attribute:other', targetKey: 'other' });
  value.pins.rows.push({ pinned: true, kind: 'attribute', targetKey: 'other', targetCanonical: 'attribute:other' });
  const profile = savedProfile();
  const view = render(FactLedger, { controller: value, profile });
  await fireEvent.click(screen.getByRole('button', { name: 'Reveal Location values' }));
  expect(screen.getByText('Example City')).toBeDefined();
  value.pins.rows.reverse();
  await tick();
  expect(screen.getByText('Example City')).toBeDefined();
  value.selectedTargetOption = 'target-1';
  await tick();
  expect(screen.queryByText('Example City')).toBeNull();
  value.selectedTargetOption = 'all';
  await tick();
  expect(screen.queryByText('Example City')).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Reveal Location values' }));
  value.selectedSection = 'pins';
  await tick();
  expect(screen.queryByText('Example City')).toBeNull();
  expect(view.container.textContent).toContain('Hidden');
  profile.destroy();
});

it('shows explicit unpins in their own fold', async () => {
  const value = controller();
  value.pins.rows[0]!.pinned = false;
  value.selectedSection = 'pins';
  render(FactLedger, { controller: value });
  const fold = screen.getByText('1 unpinned field', { selector: 'summary' });
  expect(fold.closest('details')?.open).toBe(false);
  await fireEvent.click(fold);
  expect(fold.closest('details')?.open).toBe(true);
  expect(screen.getByText('Unpinned')).toBeDefined();
});

it('retains saved attributes and employment alongside loading and independent errors', async () => {
  const value = controller();
  value.pins.rows.push({ kind: 'employment', pinned: true, targetKey: 'employment', targetCanonical: 'employment:other' });
  const profile = savedProfile('current', false);
  const pending = deferredResponse();
  const entity = new DirectoryEntityController(createAPIClient(async () => pending.promise), 42);
  entity.employments = [employment()];
  entity.organizations = [organization()];
  render(FactLedger, { controller: value, profile, entity });
  value.pins.loading = true;
  profile.reloadPending = true;
  const refresh = entity.refreshEmployments();
  await tick();
  expect(screen.getByText('Loading pins…')).toBeDefined();
  expect(screen.getByText('Example City')).toBeDefined();
  expect(screen.getByText('Engineer · Synthetic Org')).toBeDefined();
  value.pins.loading = false;
  profile.reloadPending = false;
  pending.resolve(Response.json({}, { status: 503 }));
  await refresh;
  value.pins.error = 'Unable to load fact pins.';
  profile.attributesError = 'Unavailable';
  await tick();
  expect(screen.getByRole('button', { name: 'Retry pins' })).toBeDefined();
  expect(screen.getByRole('button', { name: 'Retry saved values' })).toBeDefined();
  expect(screen.getByRole('button', { name: 'Retry employment' })).toBeDefined();
  expect(screen.getByText('Example City')).toBeDefined();
  expect(screen.getByText('Engineer · Synthetic Org')).toBeDefined();
  expect(screen.queryByText('No saved value')).toBeNull();
  profile.destroy(); entity.destroy();
});
