<script lang="ts">
  import { Button, SelectDropdown } from '@kenn-io/kit-ui';
  import { listCLIAccounts, repairAccountAttribution } from '../../api/generated/api/api';
  import type { CliAccountResponse } from '../../api/generated/models';
  import type { APIClient } from '../../api/client';
  import type { ExploreFilter } from '../../explore/models';
  import { sameFilterID } from '../../explore/models';
  import { sourceMailbox } from '../../sources/labels';

  let {
    client,
    filters,
    onChange,
    showSingleMailbox = false,
  }: {
    client: APIClient;
    filters: ExploreFilter[];
    onChange: (filters: ExploreFilter[]) => void;
    showSingleMailbox?: boolean;
  } = $props();
  type Option = { value: string; label: string; sourceID: string };

  // Waits between catalog reads while the daemon reports the account catalog
  // unavailable, so the picker recovers once a slow first read finishes.
  const retryDelaysMs = [2000, 5000, 10000, 20000, 30000];

  let options = $state<Option[]>([]);
  // Every catalog entry by key, including those a source does not list
  // because its mail is not divided, so a saved pick keeps its label.
  let known = $state(new Map<string, Option>());
  let loading = $state(true);
  let error = $state('');
  let pending = $state<{ id: number; email: string; count: number }[]>([]);
  let repairing = $state<number[]>([]);
  let repairError = $state('');
  let retryRepair: (id: number) => void = () => {};
  let unavailable = $state(false);
  let retrying = $state(false);
  const selected = $derived(filters.find((f) => f.dimension === 'account')?.values[0] ?? '');
  const sourceIDs = $derived(filters.filter((f) => f.dimension === 'source').flatMap((f) => f.values));
  const visibleOptions = $derived([
    { value: '', label: 'All accounts' },
    ...options.filter((o) => sourceIDs.length === 0 || includesSource(o.sourceID) || o.value === selected),
    ...(selected && !loading && !options.some((o) => o.value === selected)
      ? [known.get(selected) ?? { value: selected, label: `${selected} (unavailable)` }]
      : []),
  ]);

  // A pick from a source the source filter no longer includes would only
  // empty the results, so it is dropped.
  $effect(() => {
    if (!selected || sourceIDs.length === 0) return;
    const pick = known.get(selected);
    if (pick && !includesSource(pick.sourceID)) select('');
  });

  function includesSource(id: string): boolean {
    return sourceIDs.some((sourceID) => sameFilterID(sourceID, id));
  }

  // List a source's children when they divide its mail, or when some of it
  // has no confirmed account or still waits for repair.
  function showChildren(account: CliAccountResponse): boolean {
    const children = account.virtual_accounts ?? [];
    const mailbox = sourceMailbox(account.email);
    let named = 0;
    for (const child of children) {
      if (child.unattributed) {
        if (child.message_count + child.source_deleted_count > 0 || (child.pending_count ?? 0) > 0) return true;
      } else if ((child.account_address ?? '').toLowerCase() !== mailbox) {
        return true;
      } else {
        named += 1;
      }
    }
    return named > 1;
  }

  function readOptions(accounts: CliAccountResponse[]): {
    options: Option[];
    known: Map<string, Option>;
    pending: { id: number; email: string; count: number }[];
  } {
    const pendingCounts: { id: number; email: string; count: number }[] = [];
    const all = new Map<string, Option>();
    const read = accounts.flatMap((account) => {
      const children = account.virtual_accounts ?? [];
      const count = children.reduce((total, child) => total + (child.pending_count ?? 0), 0);
      if (count > 0) pendingCounts.push({ id: account.id, email: account.email, count });
      const entries = children.map((child) => {
        const name = child.unattributed ? 'Unattributed' : (child.account_address ?? '');
        return {
          value: child.key,
          label: `${account.email} / ${name} (${child.message_count})`,
          sourceID: String(child.source_id),
        };
      });
      for (const entry of entries) all.set(entry.value, entry);
      return showSingleMailbox || showChildren(account) ? entries : [];
    });
    return { options: read, known: all, pending: pendingCounts };
  }

  $effect(() => {
    const lifetime = new AbortController();
    let controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    let attempt = 0;
    let shown = 0;
    let blocked = false;

    const scheduleRetry = (): void => {
      clearTimeout(timer);
      if (blocked || lifetime.signal.aborted) return;
      if (pending.length > 0 || unavailable) {
        retrying = true;
        timer = setTimeout(load, retryDelaysMs[Math.min(attempt++, retryDelaysMs.length - 1)]);
      } else {
        retrying = false;
        attempt = 0;
        timer = setTimeout(load, 30000);
      }
    };
    const load = (): void => {
      controller.abort();
      controller = new AbortController();
      const signal = controller.signal;
      void listCLIAccounts({ ...client, signal }).then(({ data, response }) => {
        if (signal.aborted) return;
        if (response.status === 401 || response.status === 403) {
          blocked = true;
          retrying = false;
          error = 'Unable to load accounts.';
          return;
        }
        if (!data) {
          if (!unavailable && pending.length === 0) error = 'Unable to load accounts.';
          scheduleRetry();
          return;
        }
        error = '';
        const read = readOptions(data.accounts);
        unavailable = data.virtual_accounts_unavailable === true;
        if (!unavailable || shown === 0) {
          options = read.options;
          known = read.known;
          pending = read.pending;
          shown = read.options.length;
        }
        scheduleRetry();
      }).catch((cause: unknown) => {
        if (signal.aborted) return;
        if (!unavailable && pending.length === 0) error = cause instanceof Error ? cause.message : 'Unable to load accounts.';
        scheduleRetry();
      }).finally(() => {
        if (!signal.aborted) loading = false;
      });
    };
    retryRepair = (id: number): void => {
      clearTimeout(timer);
      repairError = '';
      repairing = [...repairing, id];
      controller.abort();
      void repairAccountAttribution({ sourceId: id }, { ...client, signal: lifetime.signal }).then(({ response }) => {
        if (!response.ok) throw new Error('Repair failed');
      }).catch(() => {
        if (!lifetime.signal.aborted) repairError = 'Unable to retry account matching.';
      }).finally(() => {
        if (!lifetime.signal.aborted) {
          repairing = repairing.filter((sourceID) => sourceID !== id);
          load();
        }
      });
    };
    loading = true;
    error = '';
    load();
    return () => {
      lifetime.abort();
      controller.abort();
      clearTimeout(timer);
    };
  });

  function select(value: string): void {
    const other = filters.filter((f) => f.dimension !== 'account');
    onChange(value ? [...other, { dimension: 'account', values: [value] }] : other);
  }
</script>

{#if options.length > 0 || selected || error || unavailable || pending.length > 0}
  <div class="account-filter">
    <label>
      <span>Account:</span>
      <SelectDropdown title="Account" options={visibleOptions} value={selected} disabled={loading} onchange={select} />
    </label>
    {#if error}<span role="alert">{error}</span>{/if}
    {#if unavailable}<span role="status">{retrying ? 'Loading accounts…' : 'Accounts unavailable'}</span>{/if}
    {#if repairError}<span role="alert">{repairError}</span>{/if}
    {#each pending as source (source.id)}
      <div class="pending-source">
        <span role="status">{source.email}: {source.count} {source.count === 1 ? 'message' : 'messages'} awaiting account matching.</span>
        <Button size="sm" surface="soft" label="Retry" disabled={repairing.includes(source.id)} onclick={() => retryRepair(source.id)} />
      </div>
    {/each}
  </div>
{/if}

<style>
  .account-filter,
  label {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .account-filter {
    flex-wrap: wrap;
  }
  .pending-source {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    flex-basis: 100%;
    gap: var(--space-2);
  }
  span {
    color: var(--text-muted);
  }
</style>
