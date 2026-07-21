export type ThemePreference = 'system' | 'light' | 'dark';
export type DensityPreference = 'compact' | 'comfortable';

export interface AppearanceDefaults {
  theme: ThemePreference;
  density: DensityPreference;
}

export interface AppearanceSnapshot extends AppearanceDefaults {
  overridden: boolean;
}

const STORAGE_KEY = 'msgvault.appearance.override';
const ROW_GEOMETRY_READINESS_FRAMES = 300;

export class RowGeometry {
  height = $state<number | undefined>();
  #observer: MutationObserver | undefined;
  #readinessFrame: number | undefined;
  #readinessAttempts = 0;

  constructor() {
    this.#read();
    if (typeof document !== 'undefined' && typeof MutationObserver !== 'undefined') {
      this.#observer = new MutationObserver(() => this.#read());
      this.#observer.observe(document.documentElement, { attributes: true, attributeFilter: ['data-density'] });
    }
    this.#probeReadiness();
  }

  destroy(): void {
    this.#observer?.disconnect();
    this.#stopReadinessProbe();
  }

  #read(): void {
    if (typeof document === 'undefined') return;
    const parsed = Number.parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--row-height'));
    if (Number.isFinite(parsed) && parsed > 0) {
      this.height = parsed;
      this.#stopReadinessProbe();
    }
  }

  #probeReadiness(): void {
    if (
      this.height !== undefined || this.#readinessFrame !== undefined ||
      this.#readinessAttempts >= ROW_GEOMETRY_READINESS_FRAMES ||
      typeof requestAnimationFrame !== 'function'
    ) return;
    this.#readinessFrame = requestAnimationFrame(() => {
      this.#readinessFrame = undefined;
      this.#readinessAttempts += 1;
      this.#read();
      this.#probeReadiness();
    });
  }

  #stopReadinessProbe(): void {
    if (this.#readinessFrame === undefined) return;
    if (typeof cancelAnimationFrame === 'function') cancelAnimationFrame(this.#readinessFrame);
    this.#readinessFrame = undefined;
  }
}

export class AppearancePreferences {
  #defaults = $state<AppearanceDefaults>({ theme: 'system', density: 'compact' });
  #override = $state<Partial<AppearanceDefaults>>({});
  #media: MediaQueryList | undefined;
  #mediaListener: ((event: MediaQueryListEvent) => void) | undefined;

  constructor(defaults: AppearanceDefaults) {
    this.#defaults = validDefaults(defaults);
    this.#override = readOverride();
    if (typeof matchMedia === 'function') {
      this.#media = matchMedia('(prefers-color-scheme: dark)');
      this.#mediaListener = () => this.#apply();
      this.#media.addEventListener('change', this.#mediaListener);
    }
    this.#apply();
  }

  get defaults(): AppearanceDefaults {
    return { ...this.#defaults };
  }

  get current(): AppearanceSnapshot {
    return {
      theme: this.#override.theme ?? this.#defaults.theme,
      density: this.#override.density ?? this.#defaults.density,
      overridden: this.#override.theme !== undefined || this.#override.density !== undefined
    };
  }

  get temporary(): Readonly<Partial<AppearanceDefaults>> {
    return { ...this.#override };
  }

  setDefaults(defaults: AppearanceDefaults): void {
    this.#defaults = validDefaults(defaults);
    this.#apply();
  }

  setTemporary(override: Partial<AppearanceDefaults>): void {
    this.#override = {
      ...this.#override,
      ...(override.theme && isTheme(override.theme) ? { theme: override.theme } : {}),
      ...(override.density && isDensity(override.density) ? { density: override.density } : {})
    };
    writeOverride(this.#override);
    this.#apply();
  }

  clearTemporary(kind?: keyof AppearanceDefaults): void {
    if (kind) {
      const next = { ...this.#override };
      delete next[kind];
      this.#override = next;
    } else this.#override = {};
    if (typeof sessionStorage !== 'undefined') sessionStorage.removeItem(STORAGE_KEY);
    if (Object.keys(this.#override).length > 0) writeOverride(this.#override);
    this.#apply();
  }

  destroy(): void {
    if (this.#media && this.#mediaListener) this.#media.removeEventListener('change', this.#mediaListener);
  }

  #apply(): void {
    if (typeof document === 'undefined') return;
    const current = this.current;
    const dark = current.theme === 'dark' || (current.theme === 'system' && (this.#media?.matches ?? false));
    const resolvedTheme = dark ? 'dark' : 'light';
    document.documentElement.dataset.theme = resolvedTheme;
    document.documentElement.dataset.density = current.density;
    document.documentElement.classList.toggle('dark', dark);
  }
}

export function createAppearancePreferences(defaults: AppearanceDefaults): AppearancePreferences {
  return new AppearancePreferences(defaults);
}

export function rebaseVirtualScroll(scrollTop: number, previousHeight: number, nextHeight: number): number {
  if (previousHeight <= 0 || nextHeight <= 0 || previousHeight === nextHeight) return scrollTop;
  const anchorIndex = Math.floor(scrollTop / previousHeight);
  const offset = Math.min(scrollTop - anchorIndex * previousHeight, nextHeight - 1);
  return anchorIndex * nextHeight + offset;
}

export function tableViewportHeight(
  containerHeight: number,
  stickyHeaderHeight: number,
  windowHeight: number
): number {
  const container = containerHeight > 0 ? containerHeight : 360;
  const windowCap = windowHeight > 0 ? windowHeight : 360;
  return Math.max(0, Math.min(container, windowCap) - Math.max(0, stickyHeaderHeight));
}

function validDefaults(defaults: AppearanceDefaults): AppearanceDefaults {
  return {
    theme: isTheme(defaults.theme) ? defaults.theme : 'system',
    density: isDensity(defaults.density) ? defaults.density : 'compact'
  };
}

function readOverride(): Partial<AppearanceDefaults> {
  if (typeof sessionStorage === 'undefined') return {};
  try {
    const parsed = JSON.parse(sessionStorage.getItem(STORAGE_KEY) ?? '{}') as Partial<AppearanceDefaults>;
    const sanitized = {
      ...(parsed.theme && isTheme(parsed.theme) ? { theme: parsed.theme } : {}),
      ...(parsed.density && isDensity(parsed.density) ? { density: parsed.density } : {})
    };
    if (Object.keys(sanitized).length === 0) sessionStorage.removeItem(STORAGE_KEY);
    else sessionStorage.setItem(STORAGE_KEY, JSON.stringify(sanitized));
    return sanitized;
  } catch {
    sessionStorage.removeItem(STORAGE_KEY);
    return {};
  }
}

function writeOverride(override: Partial<AppearanceDefaults>): void {
  if (typeof sessionStorage === 'undefined') return;
  sessionStorage.setItem(STORAGE_KEY, JSON.stringify(override));
}

function isTheme(value: string): value is ThemePreference {
  return value === 'system' || value === 'light' || value === 'dark';
}

function isDensity(value: string): value is DensityPreference {
  return value === 'compact' || value === 'comfortable';
}
