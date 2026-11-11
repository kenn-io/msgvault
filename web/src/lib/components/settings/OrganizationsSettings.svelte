<script lang="ts">
  import { Button, SearchInput } from '@kenn-io/kit-ui';
  import { onMount } from 'svelte';

  import { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { Organization } from '../../directory/models';
  import OrganizationEditor from '../directory/OrganizationEditor.svelte';

  let { controller, onOpenPerson }: { controller: DirectoryEntityController; onOpenPerson?: (personID: number) => void } = $props();
  let query = $state('');
  let editing = $state<Organization | null>();

  onMount(() => { if (controller.organizations.length === 0) void controller.refreshOrganizations(); });

  function closeEditor(): void {
    editing = undefined;
  }

  function finishEditor(): void {
    closeEditor();
    void controller.refreshOrganizations(query);
  }
</script>

<section class="block" aria-label="Organizations">
  <div class="head">
    <h3>Organizations</h3>
    <Button size="sm" label="Add organization" onclick={() => (editing = null)} />
  </div>
  <SearchInput value={query} ariaLabel="Search organizations" placeholder="Search organizations" block
    oninput={(value) => { query = value; void controller.refreshOrganizations(value); }} />
  {#if controller.organizationsLoading}<p role="status">Loading organizations…</p>{/if}
  {#if controller.errors.organizations}
    <p class="error" role="alert">{controller.errors.organizations}<Button label="Retry organizations" onclick={() => void controller.refreshOrganizations(query)} /></p>
  {:else if !controller.organizationsLoading && controller.organizations.length === 0}
    <p class="muted">No organizations found</p>
  {:else}
    <ul class="list">
      {#each controller.organizations as organization (organization.id)}
        <li>
          <span class="name">{organization.name}</span>
          {#if organization.primary_domain}<span class="muted">{organization.primary_domain}</span>{/if}
          <Button size="sm" surface="soft" label="Edit" ariaLabel={`Edit ${organization.name}`} onclick={() => (editing = organization)} />
        </li>
      {/each}
    </ul>
  {/if}
</section>

{#if editing !== undefined}
  <OrganizationEditor {controller} {onOpenPerson} organization={editing ?? undefined} onDone={finishEditor} onClose={closeEditor} />
{/if}

<style>
  .block {
    display: grid;
    gap: var(--field-gap);
    min-width: 0;
  }
  .head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--control-gap);
  }
  h3 {
    margin: 0;
    font-size: var(--font-size-md);
    font-weight: var(--font-weight-semibold);
  }
  .list {
    display: grid;
    margin: 0;
    padding: 0;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    list-style: none;
  }
  .list li {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--control-gap);
    min-height: var(--control-h-lg);
    padding: var(--space-1) var(--pane-pad-x);
  }
  .list li + li {
    border-top: 1px solid var(--border-muted);
  }
  .name {
    flex: 1;
    min-width: min(12rem, 100%);
    overflow-wrap: anywhere;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }
  .muted {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  .error {
    margin: 0;
    color: var(--text-danger);
    font-size: var(--font-size-sm);
  }
</style>
