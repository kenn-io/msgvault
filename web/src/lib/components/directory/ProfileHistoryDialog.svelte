<script lang="ts">
  import { getPersonProfileHistory as generatedGetPersonProfileHistory } from '../../api/generated/api/api';
  import { Button, Modal } from '@kenn-io/kit-ui';
  import AttributeSection from './AttributeSection.svelte';
  import { sentenceCase } from '../../explore/labels';
  import { historyProvenance } from './history-format';
  import { formatDateTime as historyTime } from '../../util/format';
  import PersonMergeHistory from './PersonMergeHistory.svelte';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';
  import type { PersonSplitCommittedContext } from '../../directory/person-merge-history-controller.svelte';
  import { onMount } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type {
    PersonProfileHistory as GeneratedPersonProfileHistory,
    ValueEnvelope as GeneratedValueEnvelope,
  } from '../../api/generated/models';
  type PersonProfileHistory = GeneratedPersonProfileHistory;
  type ValueEnvelope = GeneratedValueEnvelope;
  interface Props {
    client: APIClient;
    personID: number;
    onClose: () => void;
    controller?: DirectoryProfileController;
    onOpenPerson?: (id: number) => void;
    onSplitCommitted?: (context: PersonSplitCommittedContext) => void | Promise<void>;
  }
  let { client, personID, onClose, controller, onOpenPerson, onSplitCommitted }: Props = $props();
  let history = $state<PersonProfileHistory>();
  let loading = $state(true);
  let error = $state<string | null>(null);
  onMount(() => void load());
  onMount(() => void controller?.reload(false));
  async function load(): Promise<void> {
    error = null;
    try {
      const { data, error: responseError, response } = await generatedGetPersonProfileHistory({ id: personID }, client);
      if (data) {
        history = data;
        return;
      }
      error = messageFor(responseError, response.status);
    } catch (cause: unknown) {
      error = messageFor(cause, 0);
    } finally {
      loading = false;
    }
  }
  type HistoryRow = { id: number; value: string; context?: string | null; envelope: ValueEnvelope; observedAt?: string };

  // Any closed validity or transaction interval makes a value historical.
  function ended(envelope: ValueEnvelope): string | undefined {
    return envelope.superseded_at ?? envelope.active_until;
  }

  type Item = { envelope: ValueEnvelope; original_value: string; formatted?: string; label?: string; date_kind?: string; date_text?: string; observed_at?: string; service_slug?: string; scope_kind?: string; scope_value?: string };
  function rows(items: Item[], value: ((item: Item) => string) | null = null, context: ((item: Item) => string | null) | null = null): HistoryRow[] {
    return items.map((item) => ({ id: item.envelope.id, value: value ? value(item) : item.original_value, context: context?.(item), envelope: item.envelope, observedAt: item.observed_at }));
  }

  const sections = $derived.by(() => {
    if (!history) return [];
    return [
      { title: 'Names', rows: rows(history.names ?? [], (row) => row.formatted ?? row.original_value) },
      { title: 'Contact points', rows: rows(history.contact_points ?? [], null, providerContext) },
      { title: 'Addresses', rows: rows(history.addresses ?? []) },
      { title: 'Dates', rows: rows(history.dates ?? [], (row) => `${row.label ?? sentenceCase(row.date_kind ?? '')}: ${row.date_text ?? row.original_value}`) },
      { title: 'Categories', rows: rows(history.categories ?? []) },
      { title: 'Media', rows: rows(history.media ?? []) },
      { title: 'Seen in services', rows: rows(history.observations ?? [], null, providerContext) },
    ]
      .filter((section) => section.rows.length > 0)
      .map((section) => ({
        ...section,
        current: section.rows.filter((row) => !ended(row.envelope)),
        past: section.rows.filter((row) => ended(row.envelope)).sort((left, right) => (ended(right.envelope) ?? '').localeCompare(ended(left.envelope) ?? '')),
      }));
  });

  function providerContext(point: { service_slug?: string; scope_kind?: string; scope_value?: string }): string | null {
    const scope =
      point.scope_kind && point.scope_value
        ? `${point.scope_kind} / ${point.scope_value}`
        : (point.scope_kind ?? point.scope_value);
    const details = [point.service_slug, scope].filter(Boolean);
    return details.length ? details.join(' · ') : null;
  }
  function messageFor(value: unknown, status: number): string {
    if (typeof value === 'object' && value !== null && 'message' in value) {
      const message = (
        value as {
          message?: unknown;
        }
      ).message;
      if (typeof message === 'string' && message) return message;
    }
    return status ? `Unable to load profile history (${status})` : 'Unable to load profile history';
  }
</script>

<Modal
  title="History"
  ariaLabel="History"
  closeLabel="Close profile history"
  onclose={onClose}
  maxWidth="min(760px, calc(100vw - 32px))"
>
  <div class="profile-history">
    {#if loading}
      <p role="status">Loading profile history…</p>
    {:else if error}
      <p role="alert" class="history-error">{error}</p>
    {:else if history}
      {#snippet row(item: HistoryRow)}
        <li>
          <span class="value">{item.value}{#if item.context}<small>{item.context}</small>{/if}</span>
          <span class="meta">
            {historyProvenance(item.envelope)}{#if item.observedAt} · {historyTime(item.observedAt)}{/if}
          </span>
        </li>
      {/snippet}
      {#each sections as section (section.title)}
        <section>
          <h3>{section.title}</h3>
          {#if section.current.length > 0}
            <ul>{#each section.current as item (item.id)}{@render row(item)}{/each}</ul>
          {/if}
          {#if section.past.length > 0}
            <details>
              <summary>{section.past.length} earlier {section.past.length === 1 ? 'value' : 'values'}</summary>
              <ul>{#each section.past as item (item.id)}{@render row(item)}{/each}</ul>
            </details>
          {/if}
        </section>
      {/each}
    {/if}
    {#if error}<Button label="Retry history" onclick={() => { loading = true; void load(); }} />{/if}
    {#if controller?.attributesError ?? controller?.definitionsError}
      <p role="alert" class="history-error">{controller?.attributesError ?? controller?.definitionsError}</p>
      <Button label="Retry attribute history" onclick={() => void controller?.reload(false)} />
    {:else if controller}<AttributeSection {controller} />{/if}
    {#if !loading}
      <PersonMergeHistory {client} {personID} onOpenPerson={onOpenPerson ? (id) => { onClose(); onOpenPerson?.(id); } : undefined} onSplitCommitted={async (context) => { await onSplitCommitted?.(context); await load(); }} />
    {/if}
  </div>
  {#snippet footer()}
    <Button label="Close" onclick={onClose} />
  {/snippet}
</Modal>

<style>
  .profile-history {
    display: grid;
    gap: var(--space-6);
    min-width: min(42rem, calc(100vw - 64px));
  }
  section {
    display: grid;
    gap: var(--space-2);
  }
  h3,
  p,
  ul {
    margin: 0;
  }
  ul {
    display: grid;
    padding: 0;
    list-style: none;
  }
  li {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--field-gap);
    padding: var(--space-2) 0;
    border-bottom: 1px solid var(--border-muted);
  }
  .value {
    display: grid;
    min-width: 0;
    overflow-wrap: anywhere;
  }
  .meta {
    min-width: 0;
    max-width: 100%;
    overflow-wrap: anywhere;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  small {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  details summary {
    padding: var(--space-2) 0;
    color: var(--text-secondary);
    cursor: pointer;
    font-size: var(--font-size-sm);
  }
  details li {
    flex-direction: column;
    gap: var(--space-1);
    color: var(--text-secondary);
  }
  .history-error {
    color: var(--text-danger);
  }
  @media (max-width: 640px) {
    li {
      flex-direction: column;
      gap: var(--space-1);
    }
  }
</style>
