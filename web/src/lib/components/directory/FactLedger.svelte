<script lang="ts">
  import { Button, Card, EmptyState, SegmentedControl, SelectDropdown, Spinner } from '@kenn-io/kit-ui';
  import { onDestroy, tick } from 'svelte';

  import type { FactLedgerController, FactLedgerSection } from '../../directory/fact-ledger-controller.svelte';
  import FactEvidenceHistoryDialog from './FactEvidenceHistoryDialog.svelte';

  interface Props { controller: FactLedgerController }
  let { controller }: Props = $props();
  let root: HTMLElement;
  let destroyed = false;

  onDestroy(() => { destroyed = true; });

  const sectionOptions = [
    { value: 'evidence', label: 'Evidence' }, { value: 'claims', label: 'Claims' },
    { value: 'decisions', label: 'Decisions' }, { value: 'pins', label: 'Pins' }
  ];
  const targetOptions = $derived([
    { value: 'all', label: 'All targets' },
    ...controller.targets.map((target) => ({ value: target.optionID, label: `${target.description}${target.sensitive ? ' — Sensitive' : ''}` }))
  ]);

  $effect(() => {
    const request = controller.focusRequest;
    if (!request) return;
    void tick().then(() => {
      if (destroyed || !root?.isConnected || !controller.ownsFocusRequest(request)) return;
      const exact = Array.from(root.querySelectorAll<HTMLElement>('[data-fact-focus]'))
        .find((candidate) => candidate.dataset.factFocus === request.key);
      const fallback = root.querySelector<HTMLElement>('#fact-evidence-heading');
      const exactTarget = exact instanceof HTMLButtonElement ? exact : exact?.querySelector<HTMLButtonElement>('button');
      const sectionHeading = request.key.endsWith('-next')
        ? root.querySelector<HTMLElement>(`#fact-${request.key.slice(0, -'-next'.length)}-heading`)
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
      <label>Target<SelectDropdown title="Fact target" value={controller.selectedTargetOption} options={targetOptions} onchange={(value) => void controller.selectTarget(value)} /></label>
    {/if}
  </div>

  {#if controller.catalogLoading}<p class="loading" role="status"><Spinner size={12} label="Loading fact targets" /> Loading fact targets…</p>
  {:else if controller.catalogError}<div class="message" role="alert"><p>{controller.catalogError}</p><Button label="Retry fact targets" size="sm" onclick={() => void controller.retryCatalog()} /></div>{/if}

  {#if controller.targets.length > 0}
    <section class="target-catalog" aria-labelledby="fact-target-details-heading">
      <h3 id="fact-target-details-heading">Fact target details</h3>
      <ul>
        {#each controller.targets as target (target.optionID)}
          <li><strong>{target.description}</strong><span>{target.kind} · {target.valueType} · {target.cardinality}{target.sensitive ? ' · Sensitive' : ''}</span></li>
        {/each}
      </ul>
    </section>
  {/if}

  {#if controller.selectedSection === 'evidence'}
    <section aria-labelledby="fact-evidence-heading">
      <h3 id="fact-evidence-heading" tabindex="-1">Evidence</h3>
      {#if controller.evidence.loading && controller.evidence.rows.length === 0}<p class="loading" role="status"><Spinner size={12} label="Loading fact evidence" /> Loading evidence…</p>
      {:else if controller.evidence.error}<div class="message" role="alert"><p>{controller.evidence.error}</p><Button label="Retry evidence" size="sm" onclick={() => void controller.retrySection('evidence')} /></div>
      {:else}
        {#if controller.evidence.loading}<p class="loading" role="status"><Spinner size={12} label="Loading requested evidence page" /> Loading evidence page…</p>{/if}
        {#if controller.evidence.pageError}<div class="message" role="alert"><p>{controller.evidence.pageError}</p><Button label="Retry evidence" size="sm" onclick={() => void controller.retrySection('evidence')} /></div>{/if}
        <div class="cards" aria-busy={controller.evidence.loading}>
          {#each controller.evidence.rows as row (row.rowToken)}
            <Card level="default" padding="sm" ariaLabel="Fact evidence">
              <dl><div><dt>Source class</dt><dd>{row.sourceClass}</dd></div><div><dt>Directness</dt><dd>{row.directness}</dd></div><div><dt>Authority</dt><dd>{row.authority}</dd></div><div><dt>Identity score</dt><dd>{row.identityScore}</dd></div><div><dt>Event time</dt><dd>{row.eventTime}</dd></div><div><dt>Recorded time</dt><dd>{row.recordedTime}</dd></div><div><dt>Support</dt><dd>{row.supported ? 'Supported' : 'Unsupported'}</dd></div><div><dt>Current support status</dt><dd>{row.currentSupportLabel}</dd></div><div><dt>Created</dt><dd>{row.createdAt}</dd></div></dl>
              {#if row.excerpt !== null}
                {#if controller.revealedEvidence.has(row.rowToken)}<p class="revealed"><strong>Evidence excerpt</strong><span>{row.excerpt}</span></p>
                {:else}<Button label="Reveal evidence excerpt" size="sm" onclick={() => controller.revealEvidence(row.rowToken)} />{/if}
              {/if}
              <span data-fact-focus={`history-trigger:${row.rowToken}`}><Button label="View support history" size="sm" onclick={() => void controller.openEvidenceHistory(row.rowToken)} /></span>
            </Card>
          {:else}<EmptyState title="No fact evidence on this page." description="Choose another target or page." />{/each}
        </div>
        {@render Pagination(controller, 'evidence')}
      {/if}
    </section>
  {:else if controller.selectedSection === 'claims'}
    <section aria-labelledby="fact-claims-heading">
      <h3 id="fact-claims-heading" tabindex="-1">Claims</h3>
      {#if controller.claims.loading && controller.claims.rows.length === 0}<p class="loading" role="status"><Spinner size={12} label="Loading fact claims" /> Loading claims…</p>
      {:else if controller.claims.error}<div class="message" role="alert"><p>{controller.claims.error}</p><Button label="Retry claims" size="sm" onclick={() => void controller.retrySection('claims')} /></div>
      {:else}
        {#if controller.claims.loading}<p class="loading" role="status"><Spinner size={12} label="Loading requested claims page" /> Loading claims page…</p>{/if}
        {#if controller.claims.pageError}<div class="message" role="alert"><p>{controller.claims.pageError}</p><Button label="Retry claims" size="sm" onclick={() => void controller.retrySection('claims')} /></div>{/if}
        <div class="cards" aria-busy={controller.claims.loading}>
          {#each controller.claims.rows as row (row.rowToken)}
            <Card level="default" padding="sm" ariaLabel="Fact claim">
              {@const sensitivity = controller.claimSensitivity(row)}
              {#if sensitivity === 'unknown'}
                <p role="status" aria-label="Fact value hidden until target sensitivity is verified.">Fact value hidden until target sensitivity is verified.</p>
              {:else if sensitivity === 'sensitive' && !controller.revealedClaims.has(row.rowToken)}
                <p>Sensitive fact value hidden.</p><Button label="Reveal sensitive fact value" size="sm" onclick={() => controller.revealClaim(row.rowToken)} />
              {:else}<dl><div><dt>Submitted value</dt><dd>{row.submittedValue}</dd></div>{#if row.normalizedValue !== null}<div><dt>Normalized value</dt><dd>{row.normalizedValue}</dd></div>{/if}</dl>{/if}
              <dl><div><dt>Relation</dt><dd>{row.relation}</dd></div><div><dt>Origin</dt><dd>{row.origin}</dd></div><div><dt>Validity</dt><dd>{row.validFrom ?? 'No start'} – {row.validUntil ?? 'No end'}</dd></div><div><dt>Confidence</dt><dd>Reported confidence {row.reportedScore}</dd></div><div><dt>Created</dt><dd>{row.createdAt}</dd></div></dl>
            </Card>
          {:else}<EmptyState title="No fact claims on this page." description="Choose another target or page." />{/each}
        </div>
        {@render Pagination(controller, 'claims')}
      {/if}
    </section>
  {:else if controller.selectedSection === 'decisions'}
    <section aria-labelledby="fact-decisions-heading">
      <h3 id="fact-decisions-heading" tabindex="-1">Decisions</h3>
      {#if controller.decisions.loading && controller.decisions.rows.length === 0}<p class="loading" role="status"><Spinner size={12} label="Loading fact decisions" /> Loading decisions…</p>
      {:else if controller.decisions.error}<div class="message" role="alert"><p>{controller.decisions.error}</p><Button label="Retry decisions" size="sm" onclick={() => void controller.retrySection('decisions')} /></div>
      {:else}
        {#if controller.decisions.loading}<p class="loading" role="status"><Spinner size={12} label="Loading requested decisions page" /> Loading decisions page…</p>{/if}
        {#if controller.decisions.pageError}<div class="message" role="alert"><p>{controller.decisions.pageError}</p><Button label="Retry decisions" size="sm" onclick={() => void controller.retrySection('decisions')} /></div>{/if}
        <div class="cards" aria-busy={controller.decisions.loading}>
          {#each controller.decisions.rows as row (row.rowToken)}
            <Card level="default" padding="sm" ariaLabel="Historical fact decision">
              <dl><div><dt>Action</dt><dd>{row.action}</dd></div><div><dt>Reason</dt><dd>{row.reason}</dd></div>{#if row.projectionKind}<div><dt>Projection</dt><dd>Projection {row.projectionKind}</dd></div>{/if}<div><dt>Created</dt><dd>{row.createdAt}</dd></div></dl>
              <details><summary>Score details — Total {row.score.total}</summary><dl><div><dt>Authority</dt><dd>{row.score.authority}</dd></div><div><dt>Confidence</dt><dd>{row.score.confidence}</dd></div><div><dt>Corroboration</dt><dd>{row.score.corroboration}</dd></div><div><dt>Directness</dt><dd>{row.score.directness}</dd></div><div><dt>Freshness</dt><dd>{row.score.freshness}</dd></div><div><dt>Source class</dt><dd>{row.score.sourceClass}</dd></div></dl></details>
            </Card>
          {:else}<EmptyState title="No fact decisions on this page." description="Choose another target or page." />{/each}
        </div>
        {@render Pagination(controller, 'decisions')}
      {/if}
    </section>
  {:else}
    <section aria-labelledby="fact-pins-heading">
      <h3 id="fact-pins-heading" tabindex="-1">Pins</h3>
      {#if controller.pins.loading}<p class="loading" role="status"><Spinner size={12} label="Loading fact pins" /> Loading pins…</p>
      {:else if controller.pins.error}<div class="message" role="alert"><p>{controller.pins.error}</p><Button label="Retry pins" size="sm" onclick={() => void controller.retrySection('pins')} /></div>
      {:else}<ul class="pin-list">{#each controller.pins.rows as row (row.rowToken)}<li><strong>{row.description}</strong><span>{row.kind} · {row.pinned ? 'Pinned' : 'Unpinned'}</span></li>{:else}<li>No fact pins.</li>{/each}</ul>{/if}
    </section>
  {/if}
</div>

{#if controller.historyOpen}<FactEvidenceHistoryDialog {controller} onClose={() => controller.closeEvidenceHistory()} />{/if}

{#snippet Pagination(controller: FactLedgerController, section: 'evidence' | 'claims' | 'decisions')}
  <nav class="pagination" aria-label={`${section} pages`}>
    <Button label={`First ${section} page`} size="sm" disabled={page(section).loading || !controller.hasPrevious(section)} onclick={() => void controller.firstPage(section)} />
    <Button label={`Previous ${section} page`} size="sm" disabled={page(section).loading || !controller.hasPrevious(section)} onclick={() => void controller.previousPage(section)} />
    <span>Offset {page(section).offset}</span>
    <span data-fact-focus={`${section}-next`}><Button label={`Next ${section} page`} size="sm" disabled={page(section).loading || !controller.hasNext(section)} onclick={() => void controller.nextPage(section)} /></span>
  </nav>
{/snippet}

<style>
  .ledger, section, .cards, .message, .target-catalog ul, .pin-list { display: grid; gap: var(--space-3); min-width: 0; }
  .toolbar, .pagination { display: flex; align-items: end; justify-content: space-between; gap: var(--space-3); flex-wrap: wrap; }
  .toolbar label { display: grid; gap: var(--space-1); color: var(--text-secondary); font-size: var(--font-size-sm); }
  h3, p, dl, dd, ul { margin: 0; }
  dl { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(12rem, 100%), 1fr)); gap: var(--space-2) var(--space-4); }
  dl > div, .revealed { display: grid; gap: var(--space-1); min-width: 0; }
  dt, .pin-list span, .target-catalog span, .pagination { color: var(--text-muted); font-size: var(--font-size-sm); }
  dd, .revealed span { overflow-wrap: anywhere; }
  .pin-list, .target-catalog ul { padding-left: var(--space-5); }
  .pin-list li, .target-catalog li { min-width: 0; overflow-wrap: anywhere; }
  .loading { display: flex; align-items: center; gap: var(--space-2); color: var(--text-muted); }
  .message { justify-items: start; padding-left: var(--space-3); border-left: 2px solid var(--accent-red); }
  @media (max-width: 760px) {
    .toolbar { align-items: stretch; }
    .toolbar > :global(.kit-segmented), .toolbar label { width: 100%; }
    dl { grid-template-columns: 1fr; }
    .pagination { justify-content: center; }
  }
</style>
