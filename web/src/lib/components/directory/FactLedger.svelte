<script lang="ts">
  import { Button, EmptyState, SegmentedControl, Typeahead, Spinner } from '@kenn-io/kit-ui';
  import Lock from '@lucide/svelte/icons/lock';
  import { formatShortDate } from '../../util/format';
  import { onDestroy, tick } from 'svelte';

  import type { FactLedgerController, FactLedgerSection } from '../../directory/fact-ledger-controller.svelte';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';
  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import AttributeSection from './AttributeSection.svelte';
  import { joinDefinitions } from './attribute-value';
  import { historyProvenance } from './history-format';
  import { organizationName } from './organization-name';
  import FactEvidenceHistoryDialog from './FactEvidenceHistoryDialog.svelte';

  interface Props { controller: FactLedgerController; profile?: DirectoryProfileController | null; entity?: DirectoryEntityController | null }
  let { controller, profile, entity }: Props = $props();
  let root: HTMLElement;
  let destroyed = false;
  const searchID = $props.id();
  const fields = $derived(joinDefinitions(profile?.definitions ?? [], profile?.attributes?.attributes ?? []));
  const definitions = $derived(fields.map(field => field.definition));
  const selectedTarget = $derived(controller.targets.find(target => target.optionID === controller.selectedTargetOption));
  const selectedField = $derived(selectedTarget ? controller.fieldName({ targetCanonical: selectedTarget.canonical, targetKey: selectedTarget.targetKey }, definitions) : undefined);
  const pins = $derived(controller.pins.rows.filter(pin => pin.pinned && (controller.selectedSection === 'pins' || !selectedTarget || pin.targetCanonical === selectedTarget.canonical)));
  const unpins = $derived(controller.pins.rows.filter(pin => !pin.pinned && (controller.selectedSection === 'pins' || !selectedTarget || pin.targetCanonical === selectedTarget.canonical)));
  const attributeIDs = $derived(pins.filter(pin => pin.kind === 'attribute').flatMap(pin => pin.targetKey ? [pin.targetKey] : []));
  function emptyTitle(section: string): string { return selectedField ? `No ${section} for ${selectedField} yet.` : `No ${section} yet`; }

  onDestroy(() => { destroyed = true; });

  const sectionOptions = [
    { value: 'claims', label: 'Claims' }, { value: 'evidence', label: 'Evidence' },
    { value: 'decisions', label: 'Decisions' }, { value: 'pins', label: 'Pins' }
  ];
  const targetOptions = $derived([
    { name: 'all', label: 'All fields' },
    ...controller.targets.map((target) => ({ name: target.optionID, label: controller.fieldName({ targetCanonical: target.canonical, targetKey: target.targetKey }, definitions), meta: target.sensitive ? 'Sensitive' : undefined }))
  ]);

  $effect(() => {
    const request = controller.focusRequest;
    if (!request) return;
    void tick().then(() => {
      if (destroyed || !root?.isConnected || !controller.ownsFocusRequest(request)) return;
      const exact = Array.from(root.querySelectorAll<HTMLElement>('[data-fact-focus]'))
        .find((candidate) => candidate.dataset.factFocus === request.key);
      const fallback = root.querySelector<HTMLElement>('#fact-evidence-section');
      const exactTarget = exact instanceof HTMLButtonElement ? exact : exact?.querySelector<HTMLButtonElement>('button');
      const sectionHeading = request.key.endsWith('-next')
        ? root.querySelector<HTMLElement>(`#fact-${request.key.slice(0, -'-next'.length)}-section`)
        : fallback;
      const target = exactTarget?.isConnected && !exactTarget.disabled ? exactTarget : sectionHeading;
      if (target?.isConnected) target.focus();
      controller.consumeFocusRequest(request);
    });
  });

  function page(section: 'evidence' | 'claims' | 'decisions') { return controller[section]; }
</script>

<div class="ledger" bind:this={root}>
  <div class="toolbar">
    <SegmentedControl options={sectionOptions} value={controller.selectedSection} onchange={(value) => void controller.selectSection(value as FactLedgerSection)} ariaLabel="Fact ledger section" />
    {#if controller.selectedSection !== 'pins'}
      <div class="field"><Typeahead inputAttributes={{ id: searchID }} title="Field" placeholder="Search fields" fallbackLabel="All fields" value={controller.selectedTargetOption} options={targetOptions} onselect={(value) => void controller.selectTarget(value)}>
        {#snippet header()}<label for={searchID}>Search fields</label>{/snippet}
        {#snippet icon(option)}{#if option.meta}<Lock size={12} />{/if}{/snippet}
      </Typeahead></div>
    {/if}
  </div>

  {#if controller.catalogLoading}<p class="loading" role="status"><Spinner size={12} label="Loading fact targets" /> Loading fact targets…</p>
  {:else if controller.catalogError}<div class="message" role="alert"><p>{controller.catalogError}</p><Button label="Retry fact targets" size="sm" onclick={() => void controller.retryCatalog()} /></div>{/if}

  {#if controller.selectedSection === 'claims'}{@render SavedPins()}{/if}

  {#if controller.selectedSection === 'evidence'}
    <section id="fact-evidence-section" aria-label="Evidence" tabindex="-1">
      <h3>Evidence</h3>
      {#if controller.evidence.loading && controller.evidence.rows.length === 0}<p class="loading" role="status"><Spinner size={12} label="Loading fact evidence" /> Loading evidence…</p>
      {:else if controller.evidence.error}<div class="message" role="alert"><p>{controller.evidence.error}</p><Button label="Retry evidence" size="sm" onclick={() => void controller.retrySection('evidence')} /></div>
      {:else}
        {#if controller.evidence.loading}<p class="loading" role="status"><Spinner size={12} label="Loading requested evidence page" /> Loading evidence page…</p>{/if}
        {#if controller.evidence.pageError}<div class="message" role="alert"><p>{controller.evidence.pageError}</p><Button label="Retry evidence" size="sm" onclick={() => void controller.retrySection('evidence')} /></div>{/if}
        <div class="rows" aria-busy={controller.evidence.loading}>
          {#each controller.evidence.rows as row (row.rowToken)}
            <div class="row" aria-label="Fact evidence">
              <div class="value"><strong>{row.sourceClass}</strong><small>{row.directness}</small>
              {#if row.excerpt !== null}
                {#if controller.revealedEvidence.has(row.rowToken)}<p class="revealed"><strong>Evidence excerpt</strong><span>{row.excerpt}</span></p>
                {:else}<Button label="Reveal evidence excerpt" size="sm" onclick={() => controller.revealEvidence(row.rowToken)} />{/if}
              {/if}
              <details><summary>Details</summary><dl><div><dt>Authority</dt><dd>{row.authority}</dd></div><div><dt>Identity score</dt><dd>{row.identityScore}</dd></div><div><dt>Event time</dt><dd>{formatShortDate(row.eventTime)}</dd></div><div><dt>Recorded time</dt><dd>{formatShortDate(row.recordedTime)}</dd></div><div><dt>Current support status</dt><dd>{row.currentSupportLabel}</dd></div></dl>
              <span data-fact-focus={`history-trigger:${row.rowToken}`}><Button label="View support history" size="sm" onclick={() => void controller.openEvidenceHistory(row.rowToken)} /></span></details></div>
              <span class="meta">{row.supported ? 'Supported' : 'Unsupported'} · {formatShortDate(row.createdAt)}{#if row.sourceURL} · <a href={row.sourceURL} target="_blank" rel="noreferrer">Source</a>{/if}</span>
            </div>
          {:else}<EmptyState title={emptyTitle('evidence')} />{/each}
        </div>
        {@render Pagination(controller, 'evidence')}
      {/if}
    </section>
  {:else if controller.selectedSection === 'claims'}
    <section id="fact-claims-section" aria-label="Claims" tabindex="-1">
      <h3>Claims</h3>
      {#if controller.claims.loading && controller.claims.rows.length === 0}<p class="loading" role="status"><Spinner size={12} label="Loading fact claims" /> Loading claims…</p>
      {:else if controller.claims.error}<div class="message" role="alert"><p>{controller.claims.error}</p><Button label="Retry claims" size="sm" onclick={() => void controller.retrySection('claims')} /></div>
      {:else}
        {#if controller.claims.loading}<p class="loading" role="status"><Spinner size={12} label="Loading requested claims page" /> Loading claims page…</p>{/if}
        {#if controller.claims.pageError}<div class="message" role="alert"><p>{controller.claims.pageError}</p><Button label="Retry claims" size="sm" onclick={() => void controller.retrySection('claims')} /></div>{/if}
        <div class="rows" aria-busy={controller.claims.loading}>
          {#each controller.claims.rows as row (row.rowToken)}
            {@const sensitivity = controller.claimSensitivity(row)}
            {@const shown = sensitivity === 'non_sensitive' || (sensitivity === 'sensitive' && controller.revealedClaims.has(row.rowToken))}
            <div class="row" aria-label="Fact claim">
              <div class="value">
                <strong>{controller.fieldName(row, definitions)}</strong>
                {#if sensitivity === 'unknown'}
                  <p role="status" aria-label="Fact value hidden until target sensitivity is verified.">Fact value hidden until target sensitivity is verified.</p>
                {:else if !shown}
                  <p>Sensitive fact value hidden.</p><Button label="Reveal sensitive fact value" size="sm" onclick={() => controller.revealClaim(row.rowToken)} />
                {:else}<span>{row.displayValue}</span>{/if}
                <details><summary>Details</summary>
                  <dl><div><dt>Relation</dt><dd>{row.relation}</dd></div><div><dt>Origin</dt><dd>{row.origin}</dd></div><div><dt>Reported confidence</dt><dd>{row.reportedScore}</dd></div></dl>
                  {#if row.validFrom || row.validUntil}<p>{row.validFrom ? formatShortDate(row.validFrom) : 'No start'} – {row.validUntil ? formatShortDate(row.validUntil) : 'No end'}</p>{/if}
                  {#if shown}<dl><div><dt>Submitted JSON</dt><dd><code>{row.submittedValue}</code></dd></div>{#if row.normalizedValue !== null}<div><dt>Normalized JSON</dt><dd><code>{row.normalizedValue}</code></dd></div>{/if}</dl>{/if}
                </details>
              </div>
              <span class="meta">{row.relation === 'contradict' ? 'Contradicted' : row.relation === 'invalid' ? 'Invalid' : row.relation === 'supersede' ? 'Superseded' : 'Support'} · {row.origin === 'extraction' ? 'From a message' : row.origin === 'enrichment' ? 'From enrichment' : row.origin} · {formatShortDate(row.createdAt)}</span>
            </div>
          {:else}<EmptyState title={emptyTitle('claims')} description={selectedField ? undefined : "Facts appear as msgvault reads this person's messages."} />{/each}
        </div>
        {@render Pagination(controller, 'claims')}
      {/if}
    </section>
  {:else if controller.selectedSection === 'decisions'}
    <section id="fact-decisions-section" aria-label="Decisions" tabindex="-1">
      <h3>Decisions</h3>
      {#if controller.decisions.loading && controller.decisions.rows.length === 0}<p class="loading" role="status"><Spinner size={12} label="Loading fact decisions" /> Loading decisions…</p>
      {:else if controller.decisions.error}<div class="message" role="alert"><p>{controller.decisions.error}</p><Button label="Retry decisions" size="sm" onclick={() => void controller.retrySection('decisions')} /></div>
      {:else}
        {#if controller.decisions.loading}<p class="loading" role="status"><Spinner size={12} label="Loading requested decisions page" /> Loading decisions page…</p>{/if}
        {#if controller.decisions.pageError}<div class="message" role="alert"><p>{controller.decisions.pageError}</p><Button label="Retry decisions" size="sm" onclick={() => void controller.retrySection('decisions')} /></div>{/if}
        <div class="rows" aria-busy={controller.decisions.loading}>
          {#each controller.decisions.rows as row (row.rowToken)}
            <div class="row" aria-label="Historical fact decision">
              <div class="value"><strong>{row.action}</strong><small>{row.reason}</small>{#if row.projectionKind}<small>Projection {row.projectionKind}</small>{/if}
              <details><summary>Score details</summary><dl><div><dt>Authority</dt><dd>{row.score.authority}</dd></div><div><dt>Confidence</dt><dd>{row.score.confidence}</dd></div><div><dt>Corroboration</dt><dd>{row.score.corroboration}</dd></div><div><dt>Directness</dt><dd>{row.score.directness}</dd></div><div><dt>Freshness</dt><dd>{row.score.freshness}</dd></div><div><dt>Source class</dt><dd>{row.score.sourceClass}</dd></div></dl></details></div><span class="meta">Total {row.score.total} · {formatShortDate(row.createdAt)}</span>
            </div>
          {:else}<EmptyState title={emptyTitle('decisions')} />{/each}
        </div>
        {@render Pagination(controller, 'decisions')}
      {/if}
    </section>
  {:else}
    <section id="fact-pins-section" aria-label="Pins" tabindex="-1">
      {@render SavedPins()}
    </section>
  {/if}

  {#if controller.targets.length > 0}
    <details class="about">
      <summary>About these fields</summary>
      <ul class="schema">{#each controller.targets as target (target.optionID)}<li class="row"><div class="value"><strong>{controller.fieldName({ targetCanonical: target.canonical, targetKey: target.targetKey }, definitions)}</strong><small>{target.description}</small></div><span class="meta">{target.kind} · {target.valueType} · {target.cardinality}{target.sensitive ? ' · Sensitive' : ''}</span></li>{/each}</ul>
    </details>
  {/if}

</div>

{#snippet SavedPins()}
  {#key `${controller.personID}:${controller.selectedSection}:${selectedTarget?.canonical ?? 'all'}`}
  <section aria-label="Saved pinned values">
    {#if controller.pins.loading}<p class="loading" role="status"><Spinner size={12} label="Loading fact pins" /> Loading pins…</p>{/if}
    {#if controller.pins.error}<div class="message" role="alert"><p>{controller.pins.error}</p><Button label="Retry pins" size="sm" onclick={() => void controller.retrySection('pins')} /></div>{/if}
    {#if pins.length}
      <h3>Saved pinned values</h3>
      {#if attributeIDs.length}
        {#if profile?.reloadPending}<p role="status">Loading saved values…</p>{/if}
        {#if !profile || profile.attributesError || !profile.attributes}
          <div class="message" role="alert"><p>Saved values unavailable.</p>{#if profile}<Button label="Retry saved values" size="sm" onclick={() => void profile?.reload()} />{/if}</div>
        {/if}
        {#if profile?.attributes}
          <AttributeSection controller={profile} universalIDs={attributeIDs} />
          {#each pins.filter(pin => pin.kind === 'attribute') as pin (pin.targetKey)}
            {@const field = fields.find(field => field.definition.universal_id === pin.targetKey)}
            {#if !field || (!field.current.length && !field.history.length && !profile.attributesError && !profile.reloadPending)}
              <div class="row"><strong>{controller.fieldName(pin, definitions)}</strong><span class="meta">{field ? 'No saved value' : 'Saved field unavailable'}</span></div>
            {/if}
          {/each}
        {/if}
      {/if}
      {#each pins.filter(pin => pin.kind === 'employment') as pin (pin.targetKey)}
        <section aria-label="Saved employment">
          <h3>{controller.fieldName(pin, definitions)}</h3>
          {#if entity?.employmentsLoading}<p role="status">Loading employment…</p>{/if}
          {#if !entity || entity.errors.employments}<div class="message" role="alert"><p>Saved employment unavailable.</p>{#if entity}<Button label="Retry employment" size="sm" onclick={() => void entity?.refreshEmployments()} />{/if}</div>{/if}
          {#if entity}
            {#each entity.employments.filter(value => value.is_current) as value (value.id)}
              {@render EmploymentRow(value)}
            {:else}{#if !entity.employments.length && !entity.errors.employments && !entity.employmentsLoading}<div class="row"><span>No saved value</span></div>{/if}{/each}
            {@const history = entity.employments.filter(value => !value.is_current)}
            {#if history.length}
              <details><summary>{history.length} earlier {history.length === 1 ? 'value' : 'values'}</summary>
                {#each history as value (value.id)}{@render EmploymentRow(value)}{/each}
              </details>
            {/if}
          {/if}
        </section>
      {/each}
    {:else if controller.selectedSection === 'pins' && !controller.pins.loading && !controller.pins.error}<EmptyState title="No pinned fields yet" />{/if}
    {#if unpins.length}
      <details><summary>{unpins.length} unpinned {unpins.length === 1 ? 'field' : 'fields'}</summary>
        {#each unpins as pin (pin.targetCanonical)}<div class="row"><strong>{controller.fieldName(pin, definitions)}</strong><span class="meta">Unpinned</span></div>{/each}
      </details>
    {/if}
  </section>
  {/key}
{/snippet}

{#snippet EmploymentRow(value: import('../../directory/models').Employment)}
  <div class="row"><span>{[value.title ?? value.role, organizationName(entity, value.organization_id)].filter(Boolean).join(' · ')}</span><span class="meta">{historyProvenance(value)}</span></div>
{/snippet}

{#if controller.historyOpen}<FactEvidenceHistoryDialog {controller} onClose={() => controller.closeEvidenceHistory()} />{/if}

{#snippet Pagination(controller: FactLedgerController, section: 'evidence' | 'claims' | 'decisions')}
  {#if controller.hasNext(section) || controller.hasPrevious(section)}
  <nav class="pagination" aria-label={`${section} pages`}>
    <Button label={`First ${section} page`} size="sm" disabled={page(section).loading || !controller.hasPrevious(section)} onclick={() => void controller.firstPage(section)} />
    <Button label={`Previous ${section} page`} size="sm" disabled={page(section).loading || !controller.hasPrevious(section)} onclick={() => void controller.previousPage(section)} />
    <span>{page(section).offset + 1}–{page(section).offset + page(section).rows.length}</span>
    <span data-fact-focus={`${section}-next`}><Button label={`Next ${section} page`} size="sm" disabled={page(section).loading || !controller.hasNext(section)} onclick={() => void controller.nextPage(section)} /></span>
  </nav>
  {/if}
{/snippet}

<style>
  .ledger, section, .message { display: grid; gap: var(--space-3); min-width: 0; }
  .toolbar, .pagination { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); }
  .field { margin-left: auto; display: grid; gap: var(--label-gap); min-width: 0; font-size: var(--font-size-sm); color: var(--text-secondary); --typeahead-min-width: 180px; }
  /* kit-ui-check-ignore: Typeahead owns this span, so its accessible metadata needs the local equivalent of kit-sr-only. */
  .field :global(.kit-typeahead__option-meta) { position: absolute; width: 1px; height: 1px; padding: 0; margin: -1px; overflow: hidden; clip-path: inset(50%); white-space: nowrap; }
  .row { display: flex; align-items: baseline; justify-content: space-between; gap: var(--field-gap); padding: var(--space-2) 0; border-bottom: 1px solid var(--border-muted); min-width: 0; }
  .value { display: grid; gap: var(--space-1); min-width: 0; overflow-wrap: anywhere; }
  .meta { text-align: right; max-width: 55%; }
  .meta, small, dt, summary, .pagination { color: var(--text-muted); font-size: var(--font-size-sm); overflow-wrap: anywhere; }
  h3, p, dl, dd, ul { margin: 0; }
  h3 { font-size: var(--font-size-sm); color: var(--text-secondary); }
  .schema { padding: 0; list-style: none; }
  dl { display: grid; gap: var(--space-2); padding: var(--space-2) 0; }
  summary { cursor: pointer; }
  .about { padding: var(--space-2) 0; }
  .about > summary { width: fit-content; text-decoration: underline; }
  .about .schema { margin-top: var(--space-2); }

  .loading { display: flex; align-items: center; gap: var(--space-2); color: var(--text-muted); }
  .message { justify-items: start; padding-left: var(--space-3); border-left: 2px solid var(--accent-red); border-start-start-radius: 0; border-end-start-radius: 0; }
  .revealed { display: grid; gap: var(--space-1); }
  @media (max-width: 760px) { .toolbar { flex-direction: column; align-items: stretch; } .field { margin-left: 0; } .pagination { flex-wrap: wrap; } }
  @media (max-width: 640px) {
    .ledger :global(.history-rows li) { flex-direction: column; align-items: stretch; gap: var(--space-1); overflow-wrap: anywhere; }
    .row { flex-direction: column; gap: var(--space-1); }
    .meta { text-align: left; max-width: 100%; }
  }
</style>
