import { getContext, onDestroy, setContext } from 'svelte';
import { getMediaSearchStatus } from '../api/generated/api/api';
import type { APIClient } from '../api/client';
import { IntegrationReadiness } from '../kata/kata-ready.svelte';

export const mediaReadinessKey = Symbol('media-readiness');

export function provideMediaReadiness(client: APIClient): IntegrationReadiness {
  const readiness = new IntegrationReadiness(async signal => {
    const { data, response } = await getMediaSearchStatus({ ...client, signal });
    return response.ok ? data?.configured : undefined;
  });
  onDestroy(() => readiness.dispose());
  return setContext(mediaReadinessKey, readiness);
}

export function mediaReadiness(): IntegrationReadiness | undefined {
  return getContext<IntegrationReadiness | undefined>(mediaReadinessKey);
}
