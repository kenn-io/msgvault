<script lang="ts">
  import type { DirectoryReviewController } from '../../directory/review-controller.svelte';
  import type { FactLedgerController } from '../../directory/fact-ledger-controller.svelte';
  import DirectoryReviewCentre from './DirectoryReviewCentre.svelte';
  import type { RelationshipReviewController } from '../../directory/relationship-review-controller.svelte';

  interface Props {
    controller: DirectoryReviewController;
    relationshipController: RelationshipReviewController;
    factController: FactLedgerController;
    directoryPersonID: number | null;
    onOpenDirectory?: () => void;
    onOpenPerson?: (personID: number) => void;
    onAnnounce?: (message: string) => void;
  }
  let {
    controller,
    relationshipController,
    factController,
    directoryPersonID,
    onOpenDirectory = undefined,
    onOpenPerson = undefined,
    onAnnounce = undefined
  }: Props = $props();
</script>

<DirectoryReviewCentre
  {controller}
  {relationshipController}
  {factController}
  {directoryPersonID}
  {onOpenDirectory}
  {onOpenPerson}
  {onAnnounce}
/>
