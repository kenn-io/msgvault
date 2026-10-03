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
  let options = $state<{ value: string; label: string; sourceID: string }[]>([]);
  let loading = $state(true);
  let error = $state('');
  let pending = $state(0);
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

  $effect(() => {
    const controller = new AbortController();
    loading = true;
    error = '';
    void listCLIAccounts({ ...client, signal: controller.signal })
      .then(({ data }) => {
        if (controller.signal.aborted) return;
        if (!data) {
          error = 'Unable to load accounts.';
          return;
        }
        pending = 0;
        options = data.accounts.flatMap((account) => {
          const children = account.virtual_accounts ?? [];
          pending += children.reduce((total, child) => total + (child.pending_count ?? 0), 0);
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
      })
      .catch((cause: unknown) => {
        if (!controller.signal.aborted) error = cause instanceof Error ? cause.message : 'Unable to load accounts.';
      })
      .finally(() => {
        if (!controller.signal.aborted) loading = false;
      });
    return () => controller.abort();
  });

  function select(value: string): void {
    const other = filters.filter((f) => f.dimension !== 'account');
    onChange(value ? [...other, { dimension: 'account', values: [value] }] : other);
  }
</script>

{#if options.length > 0 || selected || error}
  <div class="account-filter">
    <label>
      <span>Account:</span>
      <SelectDropdown title="Account" options={visibleOptions} value={selected} disabled={loading} onchange={select} />
    </label>
    {#if error}<span role="alert">{error}</span>{/if}
    {#if pending > 0}<span role="status">Attribution repair pending for {pending} messages.</span>{/if}
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
