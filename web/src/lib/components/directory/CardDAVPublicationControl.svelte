<script lang="ts">
  import { Button, Card, Spinner, Toggle } from '@kenn-io/kit-ui';
  import { onDestroy, tick, untrack } from 'svelte';

  import type { APIClient } from '../../api/client';
  import { CardDAVPublicationController } from '../../carddav/publication-controller.svelte';

  interface Props {
    client: APIClient;
    personID: number;
    onOpenConflict?: (conflictID: number) => void;
    onOpenSettings?: () => void;
    onAnnounce?: (message: string) => void;
  }

  let {
    client,
    personID,
    onOpenConflict = () => undefined,
    onOpenSettings = () => undefined,
    onAnnounce = () => undefined
  }: Props = $props();
  const controller = new CardDAVPublicationController(untrack(() => client));
  let root = $state<HTMLElement>();

  $effect.pre(() => {
    const nextPersonID = personID;
    untrack(() => { void controller.setPerson(nextPersonID); });
  });

  onDestroy(() => controller.destroy());

  function stateText(state: NonNullable<typeof controller.publication>['state']): string {
    switch (state) {
      case 'unpublished': return 'Not published';
      case 'published': return 'Published';
      case 'pending': return 'Publication pending';
      case 'conflict': return 'Publication conflict';
    }
  }

  function pendingText(operation: NonNullable<NonNullable<typeof controller.publication>['pending_operation']>): string {
    switch (operation) {
      case 'create': return 'CardDAV publication is waiting to create this contact.';
      case 'update': return 'CardDAV publication is waiting to update this contact.';
      case 'delete': return 'CardDAV publication is waiting to remove this contact.';
    }
  }

  async function togglePublication(checked: boolean): Promise<void> {
    const publication = controller.publication;
    if (!publication) return;
    const outcome = publication.state === 'unpublished' && checked
      ? await controller.publish()
      : publication.state === 'published' && !checked
        ? await controller.unpublish()
        : { kind: 'ignored' as const };
    if (outcome.kind !== 'confirmed' && outcome.kind !== 'reconciled') return;
    if (controller.announcement) onAnnounce(controller.announcement);
    if (outcome.kind !== 'confirmed') return;
    await tick();
    const target = root?.querySelector<HTMLElement>('input:not(:disabled)') ??
      root?.querySelector<HTMLElement>('h3');
    if (target?.isConnected) target.focus();
  }
</script>

<section bind:this={root} class="publication" aria-labelledby={`person-${personID}-carddav-publication-heading`}>
  <Card level="default" padding="sm">
    <div class="publication-content">
      <div class="heading-row">
        <div>
          <h3 id={`person-${personID}-carddav-publication-heading`} tabindex="-1">CardDAV publication</h3>
          <p>Publish this durable person to the selected CardDAV address book.</p>
        </div>
        {#if controller.loading}
          <span class="working" aria-label="Loading CardDAV publication" aria-busy="true">
            <Spinner size={14} label="Loading CardDAV publication" />
          </span>
        {/if}
      </div>

      {#if controller.error}
        <div class="notice notice--error" role="alert">
          <span>{controller.error}</span>
          <Button
            size="sm"
            label={controller.stateUnknown ? 'Retry CardDAV publication state' : 'Retry CardDAV publication'}
            disabled={controller.loading}
            onclick={() => void controller.retryState()}
          />
        </div>
      {/if}

      {#if controller.unavailable}
        <div class="state-copy">
          <p>CardDAV publication is unavailable. Configure or repair it in CardDAV settings.</p>
          <Button label="Open CardDAV settings" onclick={onOpenSettings} />
        </div>
      {:else if controller.publication}
        {@const publication = controller.publication}
        <div class="state-copy">
          <strong>{stateText(publication.state)}</strong>
          <span>Desired publication: {publication.desired ? 'Published' : 'Unpublished'}</span>
          {#if publication.address_book}
            <span>Publication address book: {publication.address_book.name}.</span>
          {:else}
            <span>No publish address book is selected.</span>
          {/if}
        </div>

        {#if controller.pendingAction}
          <p class="working" role="status" aria-busy="true">
            <Spinner size={14} label="Updating CardDAV publication" />
            {controller.pendingAction === 'publish' ? 'Publishing this person to CardDAV…' : 'Removing this person from CardDAV…'}
          </p>
        {/if}

        {#if publication.state === 'unpublished' && publication.address_book}
          <Toggle
            checked={false}
            ariaLabel="Publish person to CardDAV"
            disabled={!controller.canPublish()}
            onchange={(checked) => void togglePublication(checked)}
          />
        {:else if publication.state === 'published'}
          <Toggle
            checked
            ariaLabel="Remove person from CardDAV"
            disabled={!controller.canUnpublish()}
            onchange={(checked) => void togglePublication(checked)}
          />
        {:else if publication.state === 'pending'}
          <Toggle
            checked={publication.desired}
            ariaLabel={publication.desired ? 'Publish person to CardDAV' : 'Remove person from CardDAV'}
            disabled
          />
          <p>{publication.pending_operation ? pendingText(publication.pending_operation) : 'CardDAV publication is pending.'}</p>
        {:else if publication.state === 'conflict'}
          <Toggle
            checked={publication.desired}
            ariaLabel={publication.desired ? 'Publish person to CardDAV' : 'Remove person from CardDAV'}
            disabled
          />
          {#if publication.conflict_id}
            <Button
              tone="workflow"
              surface="solid"
              label={`Review CardDAV conflict ${publication.conflict_id}`}
              onclick={() => onOpenConflict(publication.conflict_id!)}
            />
          {:else}
            <p>CardDAV conflict details are unavailable.</p>
          {/if}
        {/if}

        {#if !publication.address_book}
          <Button label="Open CardDAV settings" onclick={onOpenSettings} />
        {/if}
      {:else if !controller.loading && !controller.error}
        <p>CardDAV publication state is unavailable.</p>
      {/if}
    </div>
  </Card>
</section>

<style>
  .publication, .publication-content, .state-copy { display: grid; gap: var(--space-3); min-width: 0; }
  .heading-row { display: flex; align-items: flex-start; justify-content: space-between; gap: var(--space-3); }
  .heading-row > div { display: grid; gap: var(--space-1); }
  h3, p { margin: 0; }
  .heading-row p, .state-copy span { color: var(--text-muted); font-size: var(--font-size-sm); }
  .working { display: flex; align-items: center; gap: var(--space-2); }
  .notice { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); padding: var(--space-3); border: 1px solid var(--border-default); border-radius: var(--radius-md); }
  .notice--error { border-color: var(--status-error-ink); background: var(--status-error-bg); color: var(--status-error-ink); }

  @media (max-width: 760px) {
    .heading-row, .notice { align-items: stretch; flex-direction: column; }
  }
</style>
