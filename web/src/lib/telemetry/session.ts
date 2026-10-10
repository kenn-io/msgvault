import type { APIClient } from '../api/client';
import { captureTelemetryEvent } from '../api/generated/api/api';

export function startSessionReporting(client: APIClient) {
  let signedIn = true;
  let started = document.hidden ? undefined : performance.now();
  let visible = 0;
  let seen = !document.hidden;
  let hiddenSince: number | undefined;
  let hiddenTimer: ReturnType<typeof setTimeout> | undefined;
  const pause = () => {
    if (started === undefined) return;
    visible += performance.now() - started;
    started = undefined;
  };
  const clearTimer = () => {
    clearTimeout(hiddenTimer);
    hiddenTimer = undefined;
  };
  const end = () => {
    clearTimer();
    hiddenSince = undefined;
    pause();
    if (!seen) return;
    // Keep thresholds and names in sync with internal/telemetry/telemetry.go DurationBucket.
    const duration = visible < 60_000 ? 'under_1m' : visible < 300_000 ? '1_to_5m' : visible <= 1_800_000 ? '5_to_30m' : 'over_30m';
    visible = 0;
    seen = false;
    if (signedIn) void captureTelemetryEvent({ event: 'session_ended', properties: { surface: 'web', duration_bucket: duration } }, { ...client, keepalive: true }).catch(() => undefined);
  };
  const resume = () => {
    if (hiddenSince !== undefined && Date.now() - hiddenSince >= 1_800_000) end();
    if (document.hidden) return;
    clearTimer();
    hiddenSince = undefined;
    if (!signedIn) return;
    seen = true;
    if (started === undefined) started = performance.now();
  };
  const hide = () => {
    pause();
    if (hiddenSince !== undefined) return;
    hiddenSince = Date.now();
    hiddenTimer = setTimeout(end, 1_800_000);
  };
  const visibility = () => {
    if (document.hidden) hide();
    else resume();
  };
  document.addEventListener('visibilitychange', visibility);
  window.addEventListener('pagehide', end);
  window.addEventListener('pageshow', resume);
  return {
    stop() {
      clearTimer();
      document.removeEventListener('visibilitychange', visibility);
      window.removeEventListener('pagehide', end);
      window.removeEventListener('pageshow', resume);
    },
    signedIn(value: boolean) {
      signedIn = value;
      if (signedIn) resume();
      else pause();
    },
  };
}
