<script lang="ts">
  import { Button, DatePicker, NumberInput, SearchInput, TextInput } from '@kenn-io/kit-ui';
  import { onDestroy, onMount, tick } from 'svelte';
  import type { TimelineEntry } from '../../api/generated/models';
  import type { TimelineController, TimelineTab } from '../../timeline/controller.svelte';
  import DirectoryList from '../directory/DirectoryList.svelte';
  import PageHeader from '../shell/PageHeader.svelte';
  import TimelineEntryView from './TimelineEntry.svelte';

  interface Props {
    controller: TimelineController;
    onOpenMessage?: (id: number) => void;
    onOpenConversation?: (conversationID: number, messageID?: number) => void;
    onOpenPerson?: (id: number) => void;
    onOpenMeeting?: (messageID: number) => void;
  }
  let { controller, onOpenMessage, onOpenConversation, onOpenPerson, onOpenMeeting }: Props = $props();
  const uid = $props.id();
  const tabs: { id: TimelineTab; label: string }[] = [{ id: 'day', label: 'Day' }, { id: 'person', label: 'Person' }, { id: 'around', label: 'Around' }];
  const navigation = $derived({ onOpenMessage, onOpenConversation, onOpenPerson, onOpenMeeting });
  const result = $derived(controller.result);
  const directory = $derived(controller.directory);
  const status = $derived.by(() => {
    if (!result) return '';
    const parts: string[] = [];
    for (const source of result.freshness.sources ?? []) {
      if ((source.status && source.status !== 'current') || (source.last_run && source.last_run !== 'ok') || source.warnings?.length) {
        parts.push([source.source, source.status, source.last_run !== 'ok' ? source.last_run : '', ...(source.warnings ?? []), source.sync_command].filter(Boolean).join(' · '));
      }
    }
    for (const projection of result.freshness.projections ?? []) {
      if (projection.state && projection.state !== 'current' && projection.state !== 'complete') parts.push(`${projection.name}: ${projection.state}`);
    }
    parts.push(...(result.freshness.warnings ?? []));
    parts.push(...(result.gaps ?? []).map((gap) => `${gap.section}: ${gap.code}${gap.retryable ? ' (retry available)' : ''}`));
    return parts.join(' · ');
  });
  const omissions = $derived.by(() => {
    if (!result) return '';
    const parts = Object.entries(result.omitted.budget ?? {}).map(([section, count]) => `${section}: ${count} omitted by budget`);
    if (result.omitted.noise) parts.push(`${result.omitted.noise} noise messages omitted`);
    if (result.omitted.cold) parts.push(`${result.omitted.cold} cold messages omitted`);
    return parts.join(' · ');
  });

  onMount(() => { if (!controller.result && controller.tab === 'day') void controller.day(); });
  onDestroy(() => controller.cancel());

  async function changeTab(tab: TimelineTab): Promise<void> {
    void controller.selectTab(tab);
    await tick();
  }
  function tabKeydown(event: KeyboardEvent): void {
    const index = tabs.findIndex((tab) => tab.id === controller.tab);
    const next = event.key === 'ArrowRight' ? (index + 1) % tabs.length : event.key === 'ArrowLeft' ? (index + tabs.length - 1) % tabs.length : event.key === 'Home' ? 0 : event.key === 'End' ? tabs.length - 1 : -1;
    if (next < 0) return;
    event.preventDefault();
    const tab = tabs[next]!;
    void changeTab(tab.id).then(() => document.getElementById(`${uid}-${tab.id}`)?.focus());
  }
</script>

<section class="timeline-workspace" aria-label="Timeline">
  <PageHeader title="Timeline" description="Activity by day, person, or a nearby moment." />
  <div class="tabs" role="tablist" aria-label="Timeline view">
    {#each tabs as tab (tab.id)}
      <button id={`${uid}-${tab.id}`} role="tab" type="button" aria-selected={controller.tab === tab.id}
        aria-controls={`${uid}-panel`} tabindex={controller.tab === tab.id ? 0 : -1}
        onkeydown={tabKeydown} onclick={() => void changeTab(tab.id)}>{tab.label}</button>
    {/each}
  </div>
  <div id={`${uid}-panel`} role="tabpanel" aria-labelledby={`${uid}-${controller.tab}`} tabindex="0">
    {#if controller.tab === 'day'}
      <form onsubmit={(event) => { event.preventDefault(); void controller.day(); }}>
        <DatePicker value={controller.date || null} onchange={(date) => { controller.date = date; }} ariaLabel="Date" />
        <Button type="submit" label="Show day" disabled={controller.loading} />
      </form>
    {:else if controller.tab === 'person'}
      <form onsubmit={(event) => { event.preventDefault(); void controller.searchPeople(); }}>
        <SearchInput ariaLabel="Find a person" placeholder="Name, email, or person reference" value={controller.personQuery} oninput={(value) => { controller.personQuery = value; }} />
        <Button type="submit" label="Search people" disabled={directory.loading} />
        <Button label="Show person timeline" disabled={!controller.personQuery.trim() || controller.loading} onclick={() => void controller.person()} />
      </form>
      <DirectoryList rows={directory.rows} loading={directory.loading} loadingMore={directory.loadingMore}
        error={directory.error} pageError={directory.pageError} pageRecovery={directory.pageRecovery}
        hasMore={directory.cursor !== null} selectedPersonID={result?.person?.person.person_id ?? null}
        onSelect={(id) => void controller.person(`person:${id}`)} onLoadMore={() => void directory.loadNextPage()}
        onReload={() => void directory.reloadFirstPage()} />
    {:else}
      <form onsubmit={(event) => { event.preventDefault(); void controller.around(); }}>
        <label>Anchor<TextInput bind:value={controller.anchor} placeholder="Message, event, conversation, or date" /></label>
        <NumberInput ariaLabel="Before" min={0} max={50} step={1} required value={controller.before} oninput={(value) => { if (value !== null) controller.before = value; }} />
        <NumberInput ariaLabel="After" min={0} max={50} step={1} required value={controller.after} oninput={(value) => { if (value !== null) controller.after = value; }} />
        <Button type="submit" label="Show around" disabled={!controller.anchor.trim() || controller.loading} />
      </form>
    {/if}
    {#if controller.loading}<p role="status">Loading timeline…</p>{/if}
    {#if controller.error}<p role="alert">{controller.error}</p>{/if}
    {#if controller.candidates.length}
      <section aria-label="Person candidates">
        <h2>Choose a person</h2>
        {#each controller.candidates as candidate, index (`${candidate.person_uid}:${index}`)}
          <Button label={`${candidate.display_name} · ${candidate.address}`} onclick={() => void controller.chooseCandidate(candidate)} />
        {/each}
      </section>
    {/if}
    {#if result}
      <p class="muted">{result.window.from} – {result.window.to} · {result.timezone}{result.timezone_is_default ? ' (default timezone)' : ''}</p>
      {#if result.person}
        <section aria-label="Person timeline summary">
          <h2>{result.person.person.name}</h2>
          <p>{result.person.interaction_count} interactions · {result.person.channels.join(', ')}</p>
          {#if result.person.cadence_status}<p>{result.person.cadence_status}{result.person.due_at ? ` · due ${result.person.due_at}` : ''}</p>{/if}
          {#if result.person.brief}<p class="prose">{result.person.brief.text}</p>{/if}
          {#each result.person.agenda ?? [] as item, index (index)}<p class="prose">{item.text}</p>{/each}
        </section>
      {/if}
      {#if result.anchor?.filtered}<p class="muted">The anchor is outside the selected filters.</p>{/if}
      {#if result.open_loops}
        {@render section('Open loops', result.open_loops.items)}
        {#if result.open_loops.truncated}<p class="muted">More open loops are available.</p>{/if}
        {#if result.open_loops.coverage_incomplete}<p class="muted">Open loop coverage is incomplete.</p>{/if}
        {#if result.open_loops.skipped_unparseable}<p class="muted">{result.open_loops.skipped_unparseable} open loops could not be parsed.</p>{/if}
      {/if}
      {#if result.upcoming?.length}
        <p class="muted">Upcoming activity comes from the archive.</p>
        {@render section('Upcoming', result.upcoming)}
      {/if}
      {#each result.days ?? [] as day (day.date)}
        <section aria-label={day.date}>
          <h2>{day.date}</h2>
          {#if day.summary}
            <p>{day.summary.meetings} meetings · {day.summary.people_count} people{day.summary.truncated ? ' · more activity available' : ''}</p>
          {:else}
            {@render section('Meetings', day.meetings ?? [])}
            {@render section('Conversations', day.bursts ?? [])}
            {@render section('Calls', day.calls ?? [])}
            {@render section('Notes', day.notes ?? [])}
          {/if}
        </section>
      {/each}
      {#if result.history?.length}{@render section('History', result.history)}{/if}
      {#if !(result.days?.length || result.history?.length || result.upcoming?.length || result.open_loops?.items.length)}<p>No activity in this window.</p>{/if}
      {#if omissions}<p class="muted">{omissions}</p>{/if}
      {#if status}<p class="muted" aria-label="Timeline freshness and gaps">{status}</p>{/if}
      {#if result.cursor}<Button label="Next page" onclick={() => void controller.nextPage()} disabled={controller.loading} />{/if}
    {/if}
  </div>
</section>

{#snippet section(title: string, rows: TimelineEntry[])}
  <section aria-label={title}>
    <h3>{title}</h3>
    {#each rows as entry (entry.key)}
      <TimelineEntryView {entry} timezone={result?.timezone ?? 'UTC'} onAround={(reference) => void controller.around(reference)} {...navigation} />
    {:else}<p class="muted">None in this window.</p>{/each}
  </section>
{/snippet}

<style>
  .timeline-workspace { padding: var(--space-5) var(--page-gutter); overflow-y: auto; height: 100%; }
  .tabs, form { display: flex; flex-wrap: wrap; align-items: end; gap: var(--space-3); margin-bottom: var(--space-4); }
  .tabs { border-bottom: 1px solid var(--border-muted); }
  .tabs button { padding: var(--space-2) var(--space-3); background: transparent; border: 0; color: var(--text-muted); cursor: pointer; }
  .tabs button[aria-selected='true'] { color: var(--text-primary); border-bottom: 2px solid currentColor; }
  label { display: flex; flex-direction: column; gap: var(--space-1); }
  h2, h3 { margin-top: var(--space-5); }
  .muted { color: var(--text-muted); font-size: var(--font-size-sm); }
  .prose { white-space: pre-wrap; overflow-wrap: anywhere; }
</style>
