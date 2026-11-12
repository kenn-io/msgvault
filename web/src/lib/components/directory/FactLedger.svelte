<script lang="ts">
  import { Button, SegmentedControl, Spinner } from '@kenn-io/kit-ui';
  import { formatDateTime, formatShortDate, formatShortDateTime } from '../../util/format';
  import { onDestroy, tick } from 'svelte';

  import type { FactLedgerController, FactLedgerSection } from '../../directory/fact-ledger-controller.svelte';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';
  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import FieldPicker from '../common/FieldPicker.svelte';
  import EmptyLine from '../common/EmptyLine.svelte';
  import InlineNotice from '../common/InlineNotice.svelte';
  import Pager from '../common/Pager.svelte';
  import { Disclosure } from '@kenn-io/kit-ui';
  import DetailRow from '../people/DetailRow.svelte';
  import AttributeSection from './AttributeSection.svelte';
  import { displayAttributeValue, displayClaimValue, employmentClaimDetailItems, joinDefinitions } from './attribute-value';
  import { valueProvenance } from './history-format';
  import ProvenanceDetails from './ProvenanceDetails.svelte';
  import FactEvidenceHistoryDialog from './FactEvidenceHistoryDialog.svelte';
  import type { FactClaimRow, FactDecisionRow, FactEvidenceRow } from '../../directory/fact-ledger-controller.svelte';
  import { SCORE_PARTS, authorityWords, decisionSentence, evidenceSourceWords, originWords, partWords, projectionWords, relationWords, scoreWords } from '../../directory/fact-words';

  interface Props { controller: FactLedgerController; profile?: DirectoryProfileController | null; entity?: DirectoryEntityController | null; onManageFields?: () => void }
  let { controller, profile, entity, onManageFields }: Props = $props();
  let root: HTMLElement;
  let destroyed = false;
  let fieldQuery = $state('');
  const fields = $derived(joinDefinitions(profile?.definitions ?? [], profile?.attributes?.attributes ?? []));
  const definitions = $derived(fields.map(field => field.definition));
  const selectedTarget = $derived(controller.targets.find(target => target.optionID === controller.selectedTargetOption));
  const selectedField = $derived(selectedTarget ? controller.fieldName({ targetCanonical: selectedTarget.canonical, targetKey: selectedTarget.targetKey }, definitions) : undefined);
  const pins = $derived(controller.pins.rows.filter(pin => pin.pinned && (!selectedTarget || pin.targetCanonical === selectedTarget.canonical)));
  const unpins = $derived(controller.pins.rows.filter(pin => !pin.pinned && (!selectedTarget || pin.targetCanonical === selectedTarget.canonical)));
  const attributeIDs = $derived(pins.filter(pin => pin.kind === 'attribute').flatMap(pin => pin.targetKey ? [pin.targetKey] : []));
  const loadedClaims = $derived(controller.claims.rows);
  function evidenceLabel(row: FactEvidenceRow): string {
    if (selectedField) return selectedField;
    const names = new Set(loadedClaims.filter(claim => claim.evidenceIDs.includes(row.evidenceID)).map(claim => controller.fieldName(claim, definitions)));
    return names.size ? [...names].join(', ') : 'Evidence';
  }
  function decisionClaim(row: FactDecisionRow): FactClaimRow | undefined { return loadedClaims.find(claim => claim.claimKey === row.claimKey); }
  function decisionLabel(row: FactDecisionRow): string { return controller.fieldName(row, definitions); }
  function decisionText(row: FactDecisionRow): string {
    const claim = decisionClaim(row);
    const sensitivity = claim && controller.claimSensitivity(claim);
    const value = claim && (sensitivity === 'non_sensitive' || (sensitivity === 'sensitive' && controller.revealedClaims.has(claim.rowToken)))
      ? [claimText(claim, definitions.find(field => field.universal_id === claim.targetKey)), claim.targetKind === 'employment' ? employmentClaimDetailItems(claim.parsedValue).join(' · ') : ''].filter(Boolean).join(' · ') : undefined;
    return decisionSentence(row.outcome, row.reason, value);
  }
  function relatedDecision(row: FactClaimRow): string | undefined {
    const decision = controller.decisions.rows.find(decision => decision.claimKey === row.claimKey);
    return decision && decisionText(decision);
  }
  function relatedSource(id: number, index: number): string {
    const evidence = controller.evidence.rows.find(evidence => evidence.evidenceID === id);
    return evidence ? evidenceSourceWords(evidence.sourceClass, evidence.directness) : `Source ${index + 1}`;
  }
  function claimText(row: FactClaimRow, definition: import('../../api/generated/models').AttributeDefinition | undefined): string {
    if (!definition) return row.displayValue;
    const parsed = row.parsedValue;
    const value = parsed && typeof parsed === 'object' && 'type' in parsed ? parsed as import('../../api/generated/models').AttributeValue :
      typeof parsed === 'boolean' ? { type: 'boolean' as const, boolean: parsed } : { type: 'text' as const, text: row.displayValue };
    return displayAttributeValue(definition, value);
  }
  function emptyTitle(section: string): string { return selectedField ? `No ${section} for ${selectedField} yet` : `No ${section} yet`; }
  /** "Garden designer at Lumen Grove"; the role stands in when there's no title. */
  function jobText(value: import('../../directory/models').Employment): string {
    return displayClaimValue({ title: value.title, role: value.role, organization: entity?.organizationName(value.organization_id) }, 'employment');
  }
  function sameInstant(a: string, b: string): boolean { return Date.parse(a) === Date.parse(b); }
  onDestroy(() => { destroyed = true; });

  const sectionOptions = [
    { value: 'claims', label: 'Claims' }, { value: 'evidence', label: 'Evidence' },
    { value: 'decisions', label: 'Decisions' }, { value: 'pins', label: 'Pins' }
  ];
  const targetOptions = $derived([
    { name: 'all', label: 'All fields', meta: undefined },
    ...controller.targets.map((target) => ({ name: target.optionID, label: controller.fieldName({ targetCanonical: target.canonical, targetKey: target.targetKey }, definitions), meta: target.sensitive ? 'Sensitive' : undefined }))
  ].filter(option => [option.label, option.name, option.meta ?? ''].some(text => text.toLowerCase().includes(fieldQuery.trim().toLowerCase()))));

  $effect(() => {
    const request = controller.focusRequest;
    if (!request) return;
    void tick().then(() => {
      if (destroyed || !root?.isConnected || !controller.ownsFocusRequest(request)) return;
      const exact = Array.from(root.querySelectorAll<HTMLElement>('[data-fact-focus]'))
        .find((candidate) => candidate.dataset.factFocus === request.key);
      const fallback = root.querySelector<HTMLElement>(`#fact-${controller.selectedSection}-section`);
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
    <div class="field"><FieldPicker placeholder="Search fields" fallbackLabel="All fields" value={controller.selectedTargetOption} options={targetOptions}
      loading={controller.catalogLoading} onquery={query => fieldQuery = query}
      onselect={(value) => controller.selectTarget(value)} onmanage={onManageFields} /></div>
  </div>


  {#if controller.catalogError}<InlineNotice message={controller.catalogError} actionLabel="Retry" onaction={() => void controller.retryCatalog()} />{/if}

  {#if controller.selectedSection === 'claims' && selectedTarget}{@render SelectedSaved()}{/if}

  {#if controller.selectedSection === 'evidence'}
    <section id="fact-evidence-section" aria-label="Evidence" tabindex="-1">
      {#if controller.exactFilter?.section === 'evidence'}<div><Button label="Show all" class="text-action" size="sm" onclick={() => void controller.showAll()} /></div>{/if}
      {#if controller.evidence.loading && controller.evidence.rows.length === 0}<p class="loading" role="status"><Spinner size={12} label="Loading fact evidence" /> Loading evidence…</p>
      {:else}
        {#if controller.evidence.loading}<p class="loading" role="status"><Spinner size={12} label="Loading requested evidence page" /> Loading evidence page…</p>{/if}
        <div class="rows" aria-busy={controller.evidence.loading}>
          {#each controller.evidence.rows as row (row.rowToken)}
            {@const authority = authorityWords(row.authority)}
            <div aria-label="Fact evidence" data-fact-focus={`evidence:${row.evidenceID}`}><DetailRow exception={row.lostSupport ?? undefined}
              note={row.excerpt !== null && controller.revealedEvidence.has(row.rowToken) ? `“${row.excerpt}”` : undefined}>
              <div class="value"><strong class="field-label">{evidenceLabel(row)}</strong><span>{evidenceSourceWords(row.sourceClass, row.directness)}</span></div>
              {#snippet actions()}{#if row.excerpt !== null && !controller.revealedEvidence.has(row.rowToken)}<Button label="Reveal excerpt" class="text-action" size="sm" onclick={() => controller.revealEvidence(row.rowToken)} />{/if}{/snippet}
              {#snippet details()}<dl>
                {#if authority}<div><dt>Source</dt><dd>{authority}</dd></div>{/if}
                <div><dt>Matches this person</dt><dd>{scoreWords(row.identityScore)}</dd></div>
                <div><dt>Said</dt><dd><time datetime={row.eventTime} title={formatDateTime(row.eventTime, 'long')}>{formatShortDateTime(row.eventTime)}</time></dd></div>
                {#if !sameInstant(row.eventTime, row.recordedTime)}<div><dt>Recorded</dt><dd><time datetime={row.recordedTime} title={formatDateTime(row.recordedTime, 'long')}>{formatShortDateTime(row.recordedTime)}</time></dd></div>{/if}
              </dl>
              <Disclosure variant="plain" title="Raw values"><dl><div><dt>Source class</dt><dd><code>{row.sourceClass}</code></dd></div></dl></Disclosure>
              <span class="history-trigger" data-fact-focus={`history-trigger:${row.rowToken}`}><Button label="View source history…" size="sm" onclick={() => void controller.openEvidenceHistory(row.rowToken)} /></span>{/snippet}
              {#snippet meta()}<span class="meta"><time datetime={row.createdAt} title={formatDateTime(row.createdAt)}>{formatShortDate(row.createdAt)}</time>{#if row.sourceURL}{' · '}<a href={row.sourceURL} target="_blank" rel="noreferrer">Source</a>{/if}</span>{/snippet}
            </DetailRow></div>
          {:else}{#if !controller.evidence.error}<EmptyLine message={controller.exactFilter?.section === 'evidence' ? 'Source unavailable' : emptyTitle('evidence')} />{/if}{/each}
        </div>
        {#if controller.evidence.error || controller.evidence.pageError}<InlineNotice message={controller.evidence.error ?? controller.evidence.pageError ?? ''} actionLabel="Retry" onaction={() => void controller.retrySection('evidence')} />{/if}
        {@render Pagination(controller, 'evidence')}
      {/if}
    </section>
  {:else if controller.selectedSection === 'claims'}
    <section id="fact-claims-section" aria-label="Claims" tabindex="-1">
      {#if controller.claims.loading && controller.claims.rows.length === 0}<p class="loading" role="status"><Spinner size={12} label="Loading fact claims" /> Loading claims…</p>
      {:else}
        {#if controller.claims.loading}<p class="loading" role="status"><Spinner size={12} label="Loading requested claims page" /> Loading claims page…</p>{/if}
        <div class="rows" aria-busy={controller.claims.loading}>
          {#each controller.claims.rows as row (row.rowToken)}
            {@const definition = definitions.find(field => field.universal_id === row.targetKey)}
            {@const sensitivity = controller.claimSensitivity(row)}
            {@const shown = sensitivity === 'non_sensitive' || (sensitivity === 'sensitive' && controller.revealedClaims.has(row.rowToken))}
            <div aria-label="Fact claim"><DetailRow exception={relationWords(row.relation) ?? undefined}>
              <div class="value">
                <strong class="field-label">{controller.fieldName(row, definitions)}</strong>
                {#if sensitivity === 'unknown'}
                  <p role="status" aria-label="Hidden while field details are unavailable.">Hidden while field details are unavailable.</p>
                {:else if !shown}<span class="hidden-value">Hidden</span>
                {:else}<span>{claimText(row, definition)}</span>{#if row.targetKind === 'employment'}{@const items = employmentClaimDetailItems(row.parsedValue)}{#if items.length}<span class="job-details">{#each items as item, i}<span class="job-item">{item}{#if i < items.length - 1}{' ·'}{/if}</span>{#if i < items.length - 1}{' '}{/if}{/each}</span>{/if}{/if}{/if}
              </div>
              {#snippet actions()}
                {#if sensitivity === 'sensitive'}{#if shown}<Button label="Hide" ariaLabel="Hide sensitive fact value" class="text-action" size="sm" onclick={() => controller.revealedClaims.delete(row.rowToken)} />{:else}<Button label="Reveal" ariaLabel="Reveal sensitive fact value" class="text-action" size="sm" onclick={() => controller.revealClaim(row.rowToken)} />{/if}{/if}
              {/snippet}
              {#snippet details()}
                {@const decision = relatedDecision(row)}
                <dl>
                  <div><dt>Reported confidence</dt><dd>{scoreWords(row.reportedScore)}</dd></div>
                  <div><dt>Decision</dt><dd><Button label={decision ?? 'Show decision'} ariaLabel={decision ? `Show decision: ${decision}` : 'Show decision'} class="text-action" size="sm" onclick={() => void controller.showDecision(row.claimKey, row.targetCanonical)} /></dd></div>
                  {#if row.evidenceIDs.length === 1}
                    <div><dt>Source</dt><dd><Button label="Show evidence" class="text-action" size="sm" onclick={() => void controller.showEvidence(row.evidenceIDs[0]!, row.targetCanonical)} /></dd></div>
                  {:else if row.evidenceIDs.length > 1}
                    <div><dt>Sources</dt><dd class="links">{#each row.evidenceIDs as id, index (id)}{@const words = relatedSource(id, index)}<Button label={words} ariaLabel={`Show source: ${words}`} class="text-action" size="sm" onclick={() => void controller.showEvidence(id, row.targetCanonical)} />{/each}</dd></div>
                  {/if}
                  {#if row.validFrom || row.validUntil}<div><dt>Valid</dt><dd>{row.validFrom ? formatShortDate(row.validFrom) : 'No start'} – {row.validUntil ? formatShortDate(row.validUntil) : 'No end'}</dd></div>{/if}
                  <div><dt>Created</dt><dd><time datetime={row.createdAt} title={formatDateTime(row.createdAt, 'long')}>{formatShortDateTime(row.createdAt)}</time></dd></div>
                </dl>
                <Disclosure variant="plain" title="Raw values"><dl><div><dt>Origin</dt><dd><code>{row.origin}</code></dd></div><div><dt>Relation</dt><dd><code>{row.relation}</code></dd></div>{#if shown}<div><dt>Submitted</dt><dd><code>{row.submittedValue}</code></dd></div>{#if row.normalizedValue !== null}<div><dt>Normalized</dt><dd><code>{row.normalizedValue}</code></dd></div>{/if}{/if}</dl></Disclosure>
              {/snippet}
              {#snippet meta()}<span class="meta">{#if originWords(row.origin)}{originWords(row.origin)}{' · '}{/if}<time datetime={row.createdAt} title={formatDateTime(row.createdAt)}>{formatShortDate(row.createdAt)}</time></span>{/snippet}
            </DetailRow>
            </div>
          {:else}{#if !controller.claims.error}<EmptyLine message={emptyTitle('claims')} />{/if}{/each}
        </div>
        {#if controller.claims.error || controller.claims.pageError}<InlineNotice message={controller.claims.error ?? controller.claims.pageError ?? ''} actionLabel="Retry" onaction={() => void controller.retrySection('claims')} />{/if}
        {@render Pagination(controller, 'claims')}
      {/if}
    </section>
  {:else if controller.selectedSection === 'decisions'}
    <section id="fact-decisions-section" aria-label="Decisions" tabindex="-1">
      {#if controller.exactFilter?.section === 'decisions'}<div><Button label="Show all" class="text-action" size="sm" onclick={() => void controller.showAll()} /></div>{/if}
      {#if controller.decisions.loading && controller.decisions.rows.length === 0}<p class="loading" role="status"><Spinner size={12} label="Loading fact decisions" /> Loading decisions…</p>
      {:else}
        {#if controller.decisions.loading}<p class="loading" role="status"><Spinner size={12} label="Loading requested decisions page" /> Loading decisions page…</p>{/if}
        <div class="rows" aria-busy={controller.decisions.loading}>
          {#each controller.decisions.rows as row (row.rowToken)}
            <div aria-label="Historical fact decision" data-fact-focus={`decision:${row.claimKey}`}><DetailRow description={projectionWords(row.projectionKind) !== decisionLabel(row) ? projectionWords(row.projectionKind) ?? undefined : undefined}>
              <div class="value"><strong class="field-label">{decisionLabel(row)}</strong><span>{decisionText(row)}</span></div>
              {#snippet details()}
                {#if row.scoreMaxima && row.score.total > 0}<ul class="score-parts">{#each SCORE_PARTS as [key, label] (key)}<li>{label}: {partWords(row.score[key], row.scoreMaxima[key])}</li>{/each}</ul>{/if}
                <Disclosure variant="plain" title="Raw values"><dl><div><dt>Action</dt><dd><code>{row.action}</code></dd></div><div><dt>Reason</dt><dd><code>{row.reason}</code></dd></div>{#if row.projectionKind}<div><dt>Projection</dt><dd><code>{row.projectionKind}</code></dd></div>{/if}
                  {#each SCORE_PARTS as [key, label] (key)}<div><dt>{label}</dt><dd><code>{row.score[key]}</code></dd></div>{/each}<div><dt>Score</dt><dd><code>{row.score.total}</code></dd></div></dl></Disclosure>
              {/snippet}
              {#snippet meta()}<span class="meta">{#if row.scoreMaxima && row.score.total > 0}{`Scored ${row.score.total} · `}{/if}<time datetime={row.createdAt} title={formatDateTime(row.createdAt)}>{formatShortDate(row.createdAt)}</time></span>{/snippet}
            </DetailRow></div>
          {:else}{#if !controller.decisions.error}<EmptyLine message={emptyTitle('decisions')} />{/if}{/each}
        </div>
        {#if controller.decisions.error || controller.decisions.pageError}<InlineNotice message={controller.decisions.error ?? controller.decisions.pageError ?? ''} actionLabel="Retry" onaction={() => void controller.retrySection('decisions')} />{/if}
        {@render Pagination(controller, 'decisions')}
      {/if}
    </section>
  {:else}
    <section id="fact-pins-section" aria-label="Pins" tabindex="-1">
      {@render SavedPins()}
    </section>
  {/if}


</div>

{#snippet SavedPins()}
  {#key `${controller.personID}:${controller.selectedSection}:${selectedTarget?.canonical ?? 'all'}`}
  <section aria-label="Saved pinned values">
    {#if controller.pins.loading}<p class="loading" role="status"><Spinner size={12} label="Loading fact pins" /> Loading pins…</p>{/if}
    {#if controller.pins.error}<InlineNotice message={controller.pins.error} actionLabel="Retry" onaction={() => void controller.retrySection('pins')} />{/if}
    {#if pins.length}

      {#if attributeIDs.length}
        {#if profile?.reloadPending}<p role="status">Loading saved values…</p>
        {:else if !profile || profile.attributesError || !profile.attributes}
          <InlineNotice message="Couldn't load saved values." actionLabel={profile ? 'Retry' : undefined} onaction={() => void profile?.reload()} />
        {/if}
        {#if profile?.attributes}
          <AttributeSection controller={profile} universalIDs={attributeIDs} facts pinState={controller.pins.loading || controller.pins.error ? '' : 'Pinned'} />
          {#each pins.filter(pin => pin.kind === 'attribute') as pin (pin.targetKey)}
            {@const field = fields.find(field => field.definition.universal_id === pin.targetKey)}
            {#if !field || (!field.current.length && !profile.attributesError && !profile.reloadPending)}
              <DetailRow><strong class="field-label">{controller.fieldName(pin, definitions)}</strong><EmptyLine message={field ? 'No saved value' : 'Saved field unavailable'} /></DetailRow>
            {/if}
          {/each}
        {/if}
      {/if}
      {#each pins.filter(pin => pin.kind === 'employment') as pin (pin.targetKey)}
        <section aria-label="Saved employment">
          {#if entity && !entity.employments.some(value => value.is_current) && !entity.errors.employments && !entity.employmentsLoading}
            <DetailRow><strong class="field-label">{controller.fieldName(pin, definitions)}</strong><EmptyLine message={entity.employments.length ? "No current job" : "No saved value"} /></DetailRow>
          {:else if !entity?.employments.some(value => value.is_current)}<strong class="field-label">{controller.fieldName(pin, definitions)}</strong>{/if}
          {#if entity?.employmentsLoading}<p role="status">Loading employment…</p>{/if}
          {#if !entity || entity.errors.employments}<InlineNotice message="Couldn't load saved employment." actionLabel={entity ? 'Retry' : undefined} onaction={() => void entity?.refreshEmployments()} />{/if}
          {#if entity}
            {#each entity.employments.filter(value => value.is_current) as value (value.id)}
              {@render EmploymentRow(value, controller.pins.loading || controller.pins.error ? '' : 'Pinned', controller.fieldName(pin, definitions))}
            {/each}

          {/if}
        </section>
      {/each}
    {:else if controller.selectedSection === 'pins' && !controller.pins.loading && !controller.pins.error}<EmptyLine message="No pins yet" />{/if}
    {#if unpins.length}
      <Disclosure variant="plain" title={`${unpins.length} unpinned ${unpins.length === 1 ? 'field' : 'fields'}`}>
        {#each unpins as pin (pin.targetCanonical)}<DetailRow><strong class="field-label">{controller.fieldName(pin, definitions)}</strong>{#snippet meta()}Unpinned{/snippet}</DetailRow>{/each}
      </Disclosure>
    {/if}
  </section>
  {/key}
{/snippet}

{#snippet SelectedSaved()}
  {#key `${controller.personID}:${selectedTarget?.canonical}`}
    {@const field = fields.find(field => field.definition.universal_id === selectedTarget?.targetKey)}
    {@const pinned = controller.pins.rows.some(pin => pin.pinned && pin.targetCanonical === selectedTarget?.canonical)}
    {@const pinState = controller.pins.loading || controller.pins.error ? '' : pinned ? 'Pinned' : 'Unpinned'}
    {@const jobs = entity?.employments.filter(value => value.is_current) ?? []}
    <section aria-label="Selected saved value">
      {#if (selectedTarget?.kind === 'employment' && jobs.length) || (selectedTarget?.kind !== 'employment' && field?.current.length)}<strong class="saved-label">Saved value</strong>{/if}
      {#if selectedTarget?.kind === 'employment'}
        {#each jobs as value (value.id)}{@render EmploymentRow(value, pinState, selectedField)}
        {:else}{#if entity && !entity.employmentsLoading && !entity.errors.employments}<EmptyLine message={`${selectedField}: ${entity.employments.length ? "No current job" : "No saved value"}${pinState ? ` · ${pinState}` : ''}`} />{/if}{/each}
        {#if entity?.employmentsLoading}<p role="status">Loading employment…</p>{/if}
        {#if !entity || entity.errors.employments}<InlineNotice message="Couldn't load saved employment." actionLabel={entity ? 'Retry' : undefined} onaction={() => void entity?.refreshEmployments()} />{/if}
      {:else if field?.current.length && profile}
        <AttributeSection controller={profile} universalIDs={[field.definition.universal_id]} facts {pinState} />
      {:else if profile?.attributes && !profile.reloadPending && !profile.attributesError}<EmptyLine message={`${selectedField}: No saved value${pinState ? ` · ${pinState}` : ''}`} />{/if}
      {#if controller.pins.loading}<p role="status">Loading pins…</p>{/if}
      {#if controller.pins.error}<InlineNotice message={controller.pins.error} actionLabel="Retry" onaction={() => void controller.retrySection('pins')} />{/if}
      {#if selectedTarget?.kind !== 'employment'}
        {#if profile?.reloadPending}<p role="status">Loading saved values…</p>
        {:else if !profile || profile.attributesError || !profile.attributes}<InlineNotice message="Couldn't load saved values." actionLabel={profile ? 'Retry' : undefined} onaction={() => void profile?.reload()} />{/if}
      {/if}
    </section>
  {/key}
{/snippet}

{#snippet EmploymentRow(value: import('../../directory/models').Employment, summaryState = '', label = '')}
  <DetailRow>
    {#if label}<strong class="field-label">{label}</strong>{/if}<span>{jobText(value)}</span>
    {#snippet meta()}<span title={valueProvenance(value).title}>{#if summaryState}{summaryState}{' · '}{/if}{valueProvenance(value).text}</span>{/snippet}
    {#snippet details()}<ProvenanceDetails {value} />{/snippet}
  </DetailRow>
{/snippet}

{#if controller.historyOpen}<FactEvidenceHistoryDialog {controller} onClose={() => controller.closeEvidenceHistory()} />{/if}

{#snippet Pagination(controller: FactLedgerController, section: 'evidence' | 'claims' | 'decisions')}
  <div data-fact-focus={`${section}-next`}><Pager offset={page(section).offset} count={page(section).rows.length}
    hasNext={controller.hasNext(section)} disabled={page(section).loading} label={`${section} pages`}
    onprevious={() => void controller.previousPage(section)} onnext={() => void controller.nextPage(section)} /></div>
{/snippet}

<style>
  /* The tabs and field select share one 28 row, 8 below the section header and 8 above the first row. Rows carry
   * their own 8 padding, so a list stacks with no gap; a notice sits 16 from the rows, and the pager 16 below. */
  .ledger { display: grid; gap: var(--space-group); min-width: 0; }
  /* Field groups are 24 apart: each row's 12 above and below, with its rule halfway between. */
  .ledger { --row-pad-y: var(--space-box); --fact-label-width: 120px; }
  section { display: grid; min-width: 0; }
  section > :global(.inline-notice), .rows + :global(.inline-notice), .rows :global(.inline-notice) { margin-top: var(--space-pane); }
  .toolbar { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: var(--space-group); }
  .field { margin-left: auto; min-width: 0; }
  .hidden-value { margin: 0; color: var(--text-muted); }
  /* A field group: the field's name (Heading) on the first line with the row's meta and Details, then the
   * value on a Body line. */
  .ledger :global(.detail-row:has(.job-details)) { container-type: inline-size; }
  .job-details { color: var(--text-muted); width: calc(100cqw - 2 * var(--row-pad-x)); }
  .job-item { display: inline-block; max-width: 100%; overflow-wrap: anywhere; }
  .value { display: grid; min-width: 0; overflow-wrap: anywhere; }
  .field-label { display: block; font-size: var(--type-heading-size); font-weight: 600; line-height: var(--type-heading-line); }
  .meta, dt { color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  dd { font-size: var(--type-body-size); line-height: var(--type-body-line); overflow-wrap: anywhere; }
  .history-trigger { display: flex; margin-top: var(--space-group); }
  .links { display: flex; flex-wrap: wrap; column-gap: var(--space-group); }
  time { white-space: nowrap; }
  p, dl, dd { margin: 0; }
  /* Each score part says what it found; its technical name stays in Raw values. */
  .score-parts { display: grid; margin: 0; padding: 0; list-style: none; }
  .saved-label { display: block; padding: var(--row-pad-y) var(--row-pad-x) 0; color: var(--text-muted); font-size: var(--type-meta-size); }
  .score-parts li { padding: var(--space-tight) 0; font-size: var(--type-body-size); line-height: var(--type-body-line); overflow-wrap: anywhere; }
  dl { display: grid; }
  dl > div { display: grid; grid-template-columns: var(--fact-label-width) minmax(0, 1fr); align-items: baseline; column-gap: var(--space-group); padding: var(--space-tight) 0; }
  /* A link in a value keeps the value's line, as a link in a sentence does. */
  :global(:root:root) dd :global(.kit-button.text-action) { min-height: var(--type-body-line); max-width: 100%; text-align: left; white-space: normal; }
  dd :global(.kit-button__label) { block-size: auto; }
  dd :global(.kit-button__label-text) { white-space: normal; overflow-wrap: anywhere; }
  .loading { display: flex; align-items: center; gap: var(--space-group); padding-block: var(--row-pad-y); color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
  /* On a phone the select wraps under the tabs at the left; it never floats alone at the right. */
  @media (max-width: 640px) {
    .field { flex-basis: 100%; margin-left: 0; }
    .toolbar :global(.kit-segmented) { width: 100%; }
    .toolbar :global(.kit-segmented__btn) { flex: 1; }
    dl > div { grid-template-columns: minmax(0, 1fr); }
  }
  /* On a phone the meta and actions wrap under the value as its second line, with no gap, as a description does. */
  @media (max-width: 640px) { .ledger :global(div.detail-row) { flex-wrap: wrap; row-gap: 0; } .ledger :global(.detail-row > .value) { flex-basis: 100%; } }
</style>
