<script lang="ts">
  import { SelectDropdown } from '@kenn-io/kit-ui';
  import { listCLIAccounts } from '../../api/generated/api/api';
  import type { APIClient } from '../../api/client';
  import type { ExploreFilter } from '../../explore/models';

  let { client, filters, onChange }: {
    client: APIClient;
    filters: ExploreFilter[];
    onChange: (filters: ExploreFilter[]) => void;
  } = $props();
  let options = $state<{ value: string; label: string }[]>([]);
  let loading = $state(true);
  let error = $state('');
  let pending = $state(0);
  const selected = $derived(filters.find((f) => f.dimension === 'account')?.values[0] ?? '');
  const sourceIDs = $derived(filters.filter((f) => f.dimension === 'source').flatMap((f) => f.values));
  const visibleOptions = $derived([
    { value: '', label: 'All accounts' },
    ...options.filter((o) => sourceIDs.length === 0 || sourceIDs.some((id) => o.value.startsWith(`identity:${id}:`) || o.value.startsWith(`group:${id}:`) || o.value === `unattributed:${id}`) || o.value === selected),
    ...(selected && !options.some((o) => o.value === selected) ? [{ value: selected, label: 'Unavailable account' }] : []),
  ]);
  $effect(() => {
    const controller = new AbortController();
    loading = true;
    error = '';
    void listCLIAccounts({ ...client, signal: controller.signal })
      .then(({ data }) => {
        if (controller.signal.aborted) return;
        if (!data) { error = 'Unable to load accounts.'; return; }
        pending = 0;
        options = data.accounts.flatMap((account) => {
          const children = account.virtual_accounts ?? [];
          pending += children.reduce((total, child) => total + (child.pending_count ?? 0), 0);
          const named = children.filter((v) => !v.unattributed);
          if (named.length <= 1 && !named.some((v) => v.group || v.account_address !== account.email)) return [];
          return children.map((v) => {
            const label = v.group || v.account_address || 'Unattributed';
            return { value: v.key, label: `${account.email} / ${label} (${v.message_count})` };
          });
        });
      })
      .catch((cause: unknown) => {
        if (!controller.signal.aborted) error = cause instanceof Error ? cause.message : 'Unable to load accounts.';
      })
      .finally(() => { if (!controller.signal.aborted) loading = false; });
    return () => controller.abort();
  });
  function select(value: string): void {
    const other = filters.filter((f) => f.dimension !== 'account');
    onChange(value ? [...other, { dimension: 'account', values: [value] }] : other);
  }
</script>

<div class="account-filter">
  <label>
    <span>Account:</span>
    <SelectDropdown title="Account" options={visibleOptions} value={selected} disabled={loading} onchange={select} />
  </label>
  {#if error}<span role="alert">{error}</span>{/if}
  {#if pending > 0}<span role="status">Attribution repair pending for {pending} messages.</span>{/if}
</div>

<style>
  .account-filter, label { display: flex; align-items: center; gap: var(--space-2); }
  .account-filter { flex-wrap: wrap; }
  span { color: var(--text-muted); }
</style>
