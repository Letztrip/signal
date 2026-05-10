// Signal analytics — single-file helper. Drop this into the target repo
// (e.g. src/lib/track.ts). Do not turn it into an "SDK" — every call site
// uses the top-level `track(...)` and `setUserId(...)` functions directly.
//
// Session ID strategy: this helper does NOT mint its own session id when
// the host app already has one. Reads `sessionStorage` in this order:
//   1. `pulse_session_id`   — injected by the Flutter native shell when
//                              this page runs inside an in-app webview.
//   2. `x-session-id`        — canonical web session id (matches the host
//                              repo's `getSessionId()` in sessionId.ts).
// If neither is set (rare — only on a brand-new tab in pure web), the
// helper mints a UUID v4 and writes it to `x-session-id` so the rest of
// the app's tracking-headers / API client see the same value.
//
// Wire endpoint + write key via env vars:
//   NEXT_PUBLIC_ANALYTICS_ENDPOINT   (or VITE_/REACT_APP_ for Vite/CRA)
//   NEXT_PUBLIC_ANALYTICS_WRITE_KEY
//   NEXT_PUBLIC_APP_VERSION

// Read env vars without depending on @types/node. Bundlers (Next.js, Webpack,
// Vite, esbuild) replace `process.env.NEXT_PUBLIC_*` at build time even
// when @types/node isn't installed.
type EnvMap = Record<string, string | undefined>;
const env: EnvMap =
  ((globalThis as { process?: { env?: EnvMap } }).process?.env) ?? {};
const viteMeta = (import.meta as unknown as { env?: EnvMap });
const viteEnv: EnvMap = viteMeta && viteMeta.env ? viteMeta.env : {};

const ENDPOINT =
  env.NEXT_PUBLIC_ANALYTICS_ENDPOINT ??
  viteEnv.VITE_ANALYTICS_ENDPOINT ??
  env.REACT_APP_ANALYTICS_ENDPOINT ??
  '';
const WRITE_KEY =
  env.NEXT_PUBLIC_ANALYTICS_WRITE_KEY ??
  viteEnv.VITE_ANALYTICS_WRITE_KEY ??
  env.REACT_APP_ANALYTICS_WRITE_KEY ??
  '';
const APP_VERSION =
  env.NEXT_PUBLIC_APP_VERSION ??
  viteEnv.VITE_APP_VERSION ??
  env.REACT_APP_APP_VERSION ??
  '0.0.0';

const FLUSH_AT = 20;
const FLUSH_INTERVAL_MS = 5_000;
const MAX_REQUEST_EVENTS = 100;

// Storage keys — signal-namespaced for our own use; `x-session-id` matches
// the host repo's canonical web session-id key (set by `getSessionId()` in
// sessionId.ts) so signal events line up with the API client's
// `X-Session-Id` header.
const ANON_KEY = 'signal.a_id';
const SESSION_KEY_WEBVIEW = 'pulse_session_id'; // Flutter shell injects this
const SESSION_KEY_WEB = 'x-session-id';         // canonical web key

type Properties = Record<string, unknown>;
type Event = {
  event_id: string;
  event_name: string;
  user_id: string | null;
  anonymous_id: string;
  session_id: string;
  client_ts: string;
  properties: Properties;
  context: Record<string, unknown>;
};

let queue: Event[] = [];
let userId: string | null = null;
let anonId = '';
let booted = false;

function uuid(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, (c) => {
    const r = (Math.random() * 16) | 0;
    return (c === 'x' ? r : (r & 0x3) | 0x8).toString(16);
  });
}

function mintId(prefix: string): string {
  return prefix + uuid().replace(/-/g, '');
}

function loadAnon(): string {
  try {
    const stored = localStorage.getItem(ANON_KEY);
    if (stored) return stored;
    const fresh = mintId('a_');
    localStorage.setItem(ANON_KEY, fresh);
    return fresh;
  } catch {
    return mintId('a_');
  }
}

// Read session id from the host app's existing storage. Falls back to
// minting one only when neither key is set — that case shouldn't happen
// in production because either the Flutter shell has injected
// `pulse_session_id` or the host's sessionId.ts has populated
// `x-session-id` before we get here.
function readSessionId(): string {
  try {
    const fromWebview = sessionStorage.getItem(SESSION_KEY_WEBVIEW);
    if (fromWebview) return fromWebview;
    const fromWeb = sessionStorage.getItem(SESSION_KEY_WEB);
    if (fromWeb) return fromWeb;
    const minted = uuid();
    sessionStorage.setItem(SESSION_KEY_WEB, minted);
    return minted;
  } catch {
    // sessionStorage unavailable (private mode, sandbox iframe, SSR).
    // Cache one for the lifetime of this module to keep events linkable.
    return uuid();
  }
}

function buildContext(): Record<string, unknown> {
  const ctx: Record<string, unknown> = {
    platform: 'web',
    app_version: APP_VERSION,
    sdk_version: '0.1.0',
  };
  try { ctx.locale = navigator.language; } catch { /* ignore */ }
  try { ctx.timezone = Intl.DateTimeFormat().resolvedOptions().timeZone; } catch { /* ignore */ }
  try { ctx.screen = { width: window.innerWidth, height: window.innerHeight }; } catch { /* ignore */ }
  try {
    ctx.page = {
      url: location.href,
      path: location.pathname,
      referrer: document.referrer,
      title: document.title,
    };
  } catch { /* ignore */ }
  return ctx;
}

function boot() {
  if (booted) return;
  if (typeof window === 'undefined') return;
  booted = true;
  anonId = loadAnon();
  setInterval(() => { void flush(); }, FLUSH_INTERVAL_MS);
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'hidden') void flush(true);
  });
  window.addEventListener('pagehide', () => { void flush(true); });
}

export function setUserId(id: string | null) {
  userId = id;
}

export function track(eventName: string, properties: Properties = {}) {
  if (typeof window === 'undefined') return;
  if (!ENDPOINT || !WRITE_KEY) return;
  if (!booted) boot();
  queue.push({
    event_id: uuid(),
    event_name: eventName,
    user_id: userId,
    anonymous_id: anonId,
    session_id: readSessionId(),
    client_ts: new Date().toISOString(),
    properties,
    context: buildContext(),
  });
  if (queue.length >= FLUSH_AT) void flush();
}

async function flush(useBeacon = false): Promise<void> {
  if (queue.length === 0) return;
  if (!ENDPOINT || !WRITE_KEY) { queue = []; return; }
  const batch = queue.splice(0, MAX_REQUEST_EVENTS);
  const payload = JSON.stringify({ batch });
  if (useBeacon && typeof navigator !== 'undefined' && navigator.sendBeacon) {
    try {
      navigator.sendBeacon(
        ENDPOINT + '/v1/events',
        new Blob([payload], { type: 'application/json' })
      );
      return;
    } catch { /* fall through to fetch */ }
  }
  try {
    const res = await fetch(ENDPOINT + '/v1/events', {
      method: 'POST',
      keepalive: true,
      headers: {
        'Content-Type': 'application/json',
        'X-Write-Key': WRITE_KEY,
        'Idempotency-Key': uuid(),
      },
      body: payload,
    });
    if (res.status >= 500 || !res.status) {
      // retry on next flush
      queue.unshift(...batch);
    }
    // 4xx → drop silently
  } catch {
    queue.unshift(...batch);
  }
}

// Resets only what this helper owns. Session id lives in sessionStorage
// under keys the host app manages; this function does not touch them.
export function reset() {
  userId = null;
  try { localStorage.removeItem(ANON_KEY); } catch { /* ignore */ }
  anonId = mintId('a_');
  try { localStorage.setItem(ANON_KEY, anonId); } catch { /* ignore */ }
}

// Scroll-depth tracking — opt-in. Call once at app boot (e.g. inside the
// AnalyticsBoot effect). Listens to scroll on either `window` (default) or
// a specific element. Fires `track('scroll_depth', { percent, name })` at
// each milestone (25/50/75/100% by default), once per page. Milestones
// reset automatically when `location.pathname` changes.
let scrollWired = false;
export function trackScrollDepth(opts: {
  element?: HTMLElement;
  milestones?: number[];
  name?: string;
} = {}): void {
  if (typeof window === 'undefined') return;
  if (scrollWired) return;
  scrollWired = true;

  const milestones = (opts.milestones ?? [25, 50, 75, 100]).slice().sort((a, b) => a - b);
  const target: HTMLElement | Window = opts.element ?? window;
  const fired = new Set<number>();
  let lastPath = location.pathname;
  let queued = false;

  function metrics(): { pct: number } | null {
    let scrollTop: number;
    let scrollHeight: number;
    let clientHeight: number;
    if (target === window) {
      scrollTop = window.scrollY;
      scrollHeight = document.documentElement.scrollHeight;
      clientHeight = window.innerHeight;
    } else {
      const el = target as HTMLElement;
      scrollTop = el.scrollTop;
      scrollHeight = el.scrollHeight;
      clientHeight = el.clientHeight;
    }
    const max = scrollHeight - clientHeight;
    if (max <= 0) return null;
    const pct = Math.min(100, Math.max(0, Math.round((scrollTop / max) * 100)));
    return { pct };
  }

  function check() {
    if (location.pathname !== lastPath) {
      lastPath = location.pathname;
      fired.clear();
    }
    const m = metrics();
    if (!m) return;
    for (const ms of milestones) {
      if (m.pct >= ms && !fired.has(ms)) {
        fired.add(ms);
        track('scroll_depth', {
          percent: ms,
          path: location.pathname,
          name: opts.name ?? location.pathname,
        });
      }
    }
  }

  function onScroll() {
    if (queued) return;
    queued = true;
    requestAnimationFrame(() => {
      queued = false;
      check();
    });
  }

  target.addEventListener('scroll', onScroll, { passive: true });
}
