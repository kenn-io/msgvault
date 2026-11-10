<script lang="ts">
  import { getPersonProfileHistory as generatedGetPersonProfileHistory } from '../../api/generated/api/api';
  import InlineNotice from '../common/InlineNotice.svelte';
  import HistorySection, { type HistoryRow as SectionRow } from './HistorySection.svelte';
  import AttributeSection from './AttributeSection.svelte';
  import { sentenceCase } from '../../explore/labels';
  import { sourceTypeLabel } from '../../sources/labels';
  import { valueProvenance } from './history-format';
  import { formatDateTime, formatShortDate } from '../../util/format';
  import { scopeText, type MergedInAddress } from '../../directory/merge-labels';
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
    loading = true;
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
  type HistoryRow = { id: number; value: string; context?: Item; envelope: ValueEnvelope; observedAt?: string };
  /**
   * An address a merge brought in, under Email and phone: "Merged in · Nov 6", opening that merge.
   */
  function mergedInRows(openMerge: (mergeID: number) => void, mergedIn: MergedInAddress[], workspaces: Map<string, string>): { rows: SectionRow[]; seen: Set<number> } {
    // An address that is a current contact point shows once. One a service has seen shows here once, naming the service.
    const listed = new Set((history?.contact_points ?? []).filter(point => !ended(point.envelope)).map(point => point.identity_key));
    const observations = (history?.observations ?? []).filter(point => !ended(point.envelope));
    const seen = new Set<number>();
    const rows = mergedIn.filter(address => {
      if (listed.has(address.identity_key)) return false;
      listed.add(address.identity_key);
      return true;
    }).map(address => {
      const matches = observations.filter(point => point.identity_key === address.identity_key);
      for (const point of matches) seen.add(point.envelope.id);
      const services = [...new Set(matches.map(point => providerContext(point, workspaces, false)).filter(Boolean))];
      const details = matches.map(point => {
        const line = valueProvenance(point.envelope, [['Seen', point.observed_at]]);
        return [providerContext(point, workspaces), line.text, line.title].filter(Boolean).join(' · ');
      });
      return {
        id: `merged-${address.mergeID}-${address.participantID}-${address.identity_key}`, value: address.value,
        detail: services.filter(service => address.value !== service && !address.value.startsWith(`${service} · `)).join(' · ') || undefined,
        provenance: {
          text: `Merged in · ${formatShortDate(address.mergedAt)}`,
          title: [`Merged ${formatDateTime(address.mergedAt)}`, ...details].join('\n'),
        },
        onclick: () => openMerge(address.mergeID), actionLabel: `Open the merge that brought in ${address.value}`,
      };
    });
    return { rows, seen };
  }

  // Any closed validity or transaction interval makes a value historical.
  function ended(envelope: ValueEnvelope): string | undefined {
    return envelope.superseded_at ?? envelope.active_until;
  }

  type Item = { envelope: ValueEnvelope; original_value: string; formatted?: string; label?: string; date_kind?: string; date_text?: string; observed_at?: string; service_slug?: string; service_label?: string; scope_kind?: string; scope_value?: string };
  function rows(items: Item[], value: ((item: Item) => string) | null = null, context = false): HistoryRow[] {
    return items.map((item) => ({ id: item.envelope.id, value: value ? value(item) : item.original_value, context: context ? item : undefined, envelope: item.envelope, observedAt: item.observed_at }));
  }

  const sections = $derived.by(() => {
    if (!history) return [];
    return [
      { title: 'Names', rows: rows(history.names ?? [], (row) => row.formatted ?? row.original_value) },
      { title: 'Email and phone', rows: rows(history.contact_points ?? [], null, true) },
      { title: 'Addresses', rows: rows(history.addresses ?? []) },
      { title: 'Dates', rows: rows(history.dates ?? [], (row) => `${row.label ?? sentenceCase(row.date_kind ?? '')}: ${row.date_text ?? row.original_value}`) },
      { title: 'Categories', rows: rows(history.categories ?? []) },
      { title: 'Media', rows: rows(history.media ?? []) },
      { title: 'Seen in services', rows: rows(history.observations ?? [], null, true) },
    ]
      // Email and phone stays for the addresses merges brought in, which join it when it renders.
      .filter((section) => section.rows.length > 0 || section.title === 'Email and phone')
      .map((section) => ({
        ...section,
        current: section.rows.filter((row) => !ended(row.envelope)),
        past: section.rows.filter((row) => ended(row.envelope)).sort((left, right) => (ended(right.envelope) ?? '').localeCompare(ended(left.envelope) ?? '')),
      }));
  });

  function historyRow(item: HistoryRow, workspaces: Map<string, string>): SectionRow {
    return { id: item.id, value: item.value, detail: item.context && providerContext(item.context, workspaces), provenance: valueProvenance(item.envelope, [['Seen', item.observedAt]]) };
  }

  function providerContext(point: Item, workspaces: Map<string, string>, includeScope = true): string | null {
    const details = [point.service_label || (point.service_slug ? sourceTypeLabel(point.service_slug) : undefined), includeScope && scopeText(point, workspaces)].filter(Boolean);
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
    return status ? `The server answered ${status}.` : 'The server didn’t answer.';
  }
</script>

<PersonMergeHistory {client} {personID} {onClose} scopeSources={[...(history?.contact_points ?? []), ...(history?.observations ?? [])]} definitions={controller?.definitions}
  onOpenPerson={onOpenPerson ? (id) => { onClose(); onOpenPerson?.(id); } : undefined}
  onSplitCommitted={async (context) => { await onSplitCommitted?.(context); await load(); }}>
  {#snippet fields(openMerge, mergedIn, workspaces)}
    <div class="profile-history">
      {#if loading}<p role="status">Loading profile history…</p>
      {:else if history}
        {@const merged = mergedInRows(openMerge, mergedIn, workspaces)}
        {#each sections as section (section.title)}
          {@const current = section.title === 'Seen in services' ? section.current.filter(row => !merged.seen.has(row.id)) : section.current}
          {@const rows = [...current.map(row => historyRow(row, workspaces)), ...(section.title === 'Email and phone' ? merged.rows : [])]}
          {#if rows.length || section.past.length}<HistorySection title={section.title} {rows} earlier={section.past.map(row => historyRow(row, workspaces))} />{/if}
        {/each}
      {/if}
      {#if error}<InlineNotice message="Couldn't load names, email, phone, and other details." actionLabel="Retry" onaction={() => void load()} details={error} />{/if}
      {#if controller?.attributesError ?? controller?.definitionsError}
        <InlineNotice message="Couldn't load field history." actionLabel="Retry" onaction={() => void controller?.reload(false)} details={controller?.attributesError ?? controller?.definitionsError ?? undefined} />
      {:else if controller}<AttributeSection {controller} />{/if}
    </div>
  {/snippet}
</PersonMergeHistory>

<style>
  /* Groups (Names, Email and phone, each field) are 24 apart, each heading 8 above its rows. */
  .profile-history {
    display: grid;
    gap: var(--space-section);
    min-width: 0;
  }
  p { margin: 0; font-size: var(--type-meta-size); line-height: var(--type-meta-line); color: var(--text-muted); }
</style>
