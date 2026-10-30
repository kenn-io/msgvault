import { getContext, onDestroy, setContext } from 'svelte';
import { getVisualAttachmentStatus } from '../api/generated/api/api';
import type { APIClient } from '../api/client';
import { IntegrationReadiness } from '../kata/kata-ready.svelte';

export const visualReadinessKey = Symbol('visual-readiness');
export function provideVisualReadiness(client: APIClient): IntegrationReadiness {
  const readiness = new IntegrationReadiness(async signal => {
    const { data, response } = await getVisualAttachmentStatus({ ...client, signal });
    return response.ok && data ? data.current > 0 : undefined;
  });
  onDestroy(() => readiness.dispose());
  return setContext(visualReadinessKey, readiness);
}
export function visualReadiness(): IntegrationReadiness | undefined {
  return getContext<IntegrationReadiness | undefined>(visualReadinessKey);
}
