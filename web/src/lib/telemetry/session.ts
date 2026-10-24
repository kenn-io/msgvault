import type { BrowserTelemetryQuietReason } from '@kenn-io/kit-ui/utils/browser-telemetry';
import type { APIClient } from '../api/client';
import { telemetryReporter } from './actions';

// Visible time accrues while the reporter checks or is unavailable; only a disabled receipt discards it.
export function startSessionReporting(client: APIClient) {
  let reporter = telemetryReporter(client);
  let signedIn = true;
  let listening = false;
  let started: number | undefined;
  let visible = 0;
  let seen = false;
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
    if (signedIn) reporter?.reportVisit({ event: 'session_ended', properties: { surface: 'web', duration_bucket: duration } }, { keepalive: true });
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
    if (!signedIn) return;
    if (hiddenSince !== undefined) return;
    hiddenSince = Date.now();
    hiddenTimer = setTimeout(end, 1_800_000);
  };
  const visibility = () => {
    if (document.hidden) hide();
    else resume();
  };
  const listen = (value: boolean) => {
    if (listening === value) return;
    listening = value;
    if (value) {
      document.addEventListener('visibilitychange', visibility);
      window.addEventListener('pagehide', end);
      window.addEventListener('pageshow', resume);
    } else {
      document.removeEventListener('visibilitychange', visibility);
      window.removeEventListener('pagehide', end);
      window.removeEventListener('pageshow', resume);
    }
  };
  const quietChanged = (_quiet: boolean, reason: BrowserTelemetryQuietReason) => {
    if (reason !== 'disabled') {
      listen(true);
      // Signing in while hidden starts the hidden-session deadline.
      visibility();
      return;
    }
    clearTimer();
    listen(false);
    started = undefined;
    hiddenSince = undefined;
    visible = 0;
    seen = false;
  };
  let unsubscribe = reporter?.onQuietChange(quietChanged) ?? (() => {});
  return {
    stop() {
      clearTimer();
      unsubscribe();
      listen(false);
    },
    signedIn(value: boolean) {
      if (signedIn === value) return;
      signedIn = value;
      if (signedIn) {
        reporter = telemetryReporter(client);
        unsubscribe = reporter?.onQuietChange(quietChanged) ?? (() => {});
      } else {
        pause();
        clearTimer();
        hiddenSince = undefined;
        unsubscribe();
      }
    },
  };
}
