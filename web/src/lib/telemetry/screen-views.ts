import { reportWhenVisible, telemetryReporter } from './actions';
import type { APIClient } from '../api/client';
import type { ExploreWorkspace } from '../explore/models';

export function startScreenViewReporting(client: APIClient, screen: ExploreWorkspace | 'message'): () => void {
  return reportWhenVisible(() => telemetryReporter(client)?.reportScreen(screen));
}
