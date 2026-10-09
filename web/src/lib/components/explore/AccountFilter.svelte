<script lang="ts">
  import { SelectDropdown } from '@kenn-io/kit-ui';
  import { listCLIAccounts } from '../../api/generated/api/api';
  import type { CliAccountResponse } from '../../api/generated/models';
  import type { APIClient } from '../../api/client';
  import type { ExploreFilter } from '../../explore/models';

  let {
    client,
    filters,
    onChange,
  }: {
    client: APIClient;
    filters: ExploreFilter[];
    onChange: (filters: ExploreFilter[]) => void;
  } = $props();
  type Option = { value: string; label: string; sourceID: string };

  // Waits between catalog reads while the daemon reports the account catalog
  // unavailable, so the picker recovers once a slow first read finishes.
  const retryDelaysMs = [2000, 5000, 10000, 20000, 30000];

  let options = $state<Option[]>([]);
  let loading = $state(true);
  let error = $state('');
  let pending = $state(0);
  let unavailable = $state(false);
  let retrying = $state(false);
  const selected = $derived(filters.find((f) => f.dimension === 'account')?.values[0] ?? '');
  const sourceIDs = $derived(filters.filter((f) => f.dimension === 'source').flatMap((f) => f.values));
  const visibleOptions = $derived([
    { value: '', label: 'All accounts' },
    ...options.filter((o) => sourceIDs.length === 0 || sourceIDs.includes(o.sourceID) || o.value === selected),
    ...(selected && !loading && !options.some((o) => o.value === selected)
      ? [{ value: selected, label: `${selected} (unavailable)` }]
      : []),
  ]);

  // The username of an "imaps://user@host" identifier, or the identifier.
  function sourceMailbox(identifier: string): string {
    const scheme = identifier.indexOf('://');
    if (scheme < 0) return identifier.toLowerCase();
    const rest = identifier.slice(scheme + 3);
    const at = rest.lastIndexOf('@');
    if (at <= 0) return identifier.toLowerCase();
    try {
      return decodeURIComponent(rest.slice(0, at)).toLowerCase();
    } catch {
      return rest.slice(0, at).toLowerCase();
    }
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

  function readOptions(accounts: CliAccountResponse[]): { options: Option[]; pending: number } {
    let pendingCount = 0;
    const read = accounts.flatMap((account) => {
      const children = account.virtual_accounts ?? [];
      pendingCount += children.reduce((total, child) => total + (child.pending_count ?? 0), 0);
      if (!showChildren(account)) return [];
      return children.map((child) => {
        const name = child.unattributed ? 'Unattributed' : (child.account_address ?? '');
        return {
          value: child.key,
          label: `${account.email} / ${name} (${child.message_count})`,
          sourceID: String(child.source_id),
        };
      });
    });
    return { options: read, pending: pendingCount };
  }

  $effect(() => {
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    let attempt = 0;
    let shown = 0;
    // Schedules the next reread, or ends retrying once the delays run out.
    const scheduleRetry = (): void => {
      retrying = attempt < retryDelaysMs.length;
      if (retrying) {
        timer = setTimeout(load, retryDelaysMs[attempt]);
        attempt += 1;
      }
    };
    // A failed reread while the catalog recovers keeps to the backoff; any
    // other failure, or one after the last retry, is reported.
    const fail = (message: string): void => {
      if (unavailable) {
        scheduleRetry();
        if (retrying) return;
      }
      error = message;
    };
    const load = (): void => {
      void listCLIAccounts({ ...client, signal: controller.signal })
        .then(({ data }) => {
          if (controller.signal.aborted) return;
          if (!data) {
            fail('Unable to load accounts.');
            return;
          }
          error = '';
          const read = readOptions(data.accounts);
          unavailable = data.virtual_accounts_unavailable === true;
          // An unavailable catalog may come back empty; keep what is shown.
          if (!unavailable || read.options.length > 0 || shown === 0) {
            options = read.options;
            pending = read.pending;
            shown = read.options.length;
          }
          if (unavailable) {
            scheduleRetry();
          } else {
            retrying = false;
          }
        })
        .catch((cause: unknown) => {
          if (!controller.signal.aborted) fail(cause instanceof Error ? cause.message : 'Unable to load accounts.');
        })
        .finally(() => {
          if (!controller.signal.aborted) loading = false;
        });
    };
    loading = true;
    error = '';
    load();
    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  });

  function select(value: string): void {
    const other = filters.filter((f) => f.dimension !== 'account');
    onChange(value ? [...other, { dimension: 'account', values: [value] }] : other);
  }
</script>

{#if options.length > 0 || selected || error || unavailable}
  <div class="account-filter">
    <label>
      <span>Account:</span>
      <SelectDropdown title="Account" options={visibleOptions} value={selected} disabled={loading} onchange={select} />
    </label>
    {#if error}<span role="alert">{error}</span>{/if}
    {#if unavailable}<span role="status">{retrying ? 'Loading accounts…' : 'Accounts unavailable'}</span>
    {:else if pending > 0}<span role="status">Attribution repair pending for {pending} messages.</span>{/if}
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
  span {
    color: var(--text-muted);
  }
</style>
