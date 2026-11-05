import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';

export function organizationName(controller: DirectoryEntityController | null | undefined, id: number): string {
  return controller?.organizationRecords.get(id)?.organization.name ?? controller?.organizations.find(item => item.id === id)?.name
    ?? (controller?.employmentProjection?.organization_id === id ? controller.employmentProjection.organization_name : undefined)
    ?? `Organization ${id}`;
}
