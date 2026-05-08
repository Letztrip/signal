---
name: signal-integrate
description: Drop a single `track()` helper file into a target frontend repo (Next.js / React / Vite / Flutter) and walk every page, button, link, and form in that repo to insert tracking calls at each call site. No SDK, no package — the helper is one file, the rest is inline `track('event_name', { ... })` calls. Use when the user says "integrate signal", "instrument this app with signal", "add analytics to every button/page", or names the collector endpoint and asks for setup.
---

# signal-integrate

This is **not** an SDK installer. It writes a single ~150-line helper file
into the target repo and then walks every interactive surface — pages,
buttons, links, form submits, auth state changes — and inserts `track(...)`
calls at each call site. The result: every meaningful user action in the
target app sends an event to the Signal collector, with no abstraction
layer between the call site and the HTTP POST.

## When to invoke

The user runs Claude Code from inside the **target repo** (the Next.js app,
the Flutter app, etc.). Trigger phrases:

- "integrate signal"
- "instrument this app with signal"
- "add analytics to every page and button"
- mentions of the collector URL + asks for full instrumentation

If invoked from the signal repo itself (the collector source), abort:
> "Run this from the target app repo, not from the signal repo."

## Workflow at a glance

1. Detect stack + router
2. Drop one helper file (`lib/track.ts` or `lib/track.dart`)
3. Wire init (one place)
4. Wire page-view auto-capture via the repo's own router/navigator (one place)
5. **Walk the codebase and insert `track('button_clicked', ...)` at every
   button/link, `track('form_submitted', ...)` at every form, and
   `track('identify', ...)` wherever the auth state changes.**
6. Add env-var docs
7. Verify build (typecheck / `dart analyze`)
8. Print smoke-test instructions

Step 5 is where the bulk of work happens — and where this skill is
different from "install an SDK and call init."

---

## Step 1 — Detect the stack

Read these in parallel:

```bash
test -f pubspec.yaml && echo flutter
test -f package.json && jq -r '.dependencies."next" // .devDependencies."next" // empty' package.json
test -f package.json && jq -r '.dependencies."react-router-dom" // .dependencies."react-router" // empty' package.json
test -d app && echo "next-app-router-likely"
test -d pages && echo "next-pages-router-likely"
ls pnpm-lock.yaml yarn.lock package-lock.json 2>/dev/null
```

Classify into exactly one of:
- `flutter`
- `nextjs-app` (has `next` + `app/`)
- `nextjs-pages` (has `next` + `pages/` only)
- `react-spa` (has `react` + `react-router-dom`/`react-router`, no `next`)
- `unknown` → abort, ask the user to specify

For Flutter, also detect:
- `go_router` in `pubspec.yaml` deps → use go_router pattern
- otherwise → standard `MaterialApp.navigatorObservers`

For web, detect package manager from lockfile (`pnpm`, `yarn`, `npm`).

## Step 2 — Drop the helper file

Pick the destination (respect existing convention):

| Stack | Destination |
|---|---|
| Flutter | `lib/track.dart` |
| Next.js | `lib/track.ts` if `lib/` exists at repo root; else `src/lib/track.ts` |
| React SPA | `src/lib/track.ts` |

Copy verbatim from this skill:
- Web → `helpers/track.ts` from this skill's directory
- Flutter → `helpers/track.dart` from this skill's directory

Add deps via the detected package manager:
- Web: `idb-keyval` is **NOT** required — the helper uses `localStorage` (smaller, fewer deps). No deps to add.
- Flutter: `flutter pub add hive_ce hive_ce_flutter http uuid`

If `track.ts` / `track.dart` already exists, diff before overwriting.

## Step 3 — Wire init (one place)

### 3a. Reuse the host app's existing session id

The helper does NOT mint its own session id when the host already has one.
Wire it to the host's existing source so analytics events carry the same
id that goes into HTTP headers (`X-Session-Id`), webview-injected storage
(`pulse_session_id`), and the rest of the host's tracking plumbing.

| Stack | Existing session-id source | How the helper picks it up |
|---|---|---|
| Web (Next.js / React) | `getSessionId()` in `sessionId.ts` writes to `sessionStorage['x-session-id']`. Webview-rendered pages get `sessionStorage['pulse_session_id']` from the Flutter shell. | **Automatic.** The helper reads both keys (webview key first), no host code change needed. Make sure the host's `getSessionId()` runs before the first `track(...)` — in practice it always does, the `apiClient` interceptors call it on the first request. |
| Flutter | `applicationVariables.sessionID` (UUID v4 minted once per app launch in `lib/shared/utils/applicationVariables.dart`). | **Pass it explicitly** in `initAnalytics(sessionId: applicationVariables.sessionID)`. |

Verify the host has a session-id source before wiring:

```bash
# Web
grep -rn 'sessionStorage' --include='*.ts' --include='*.tsx' src lib 2>/dev/null \
  | grep -iE 'x-session-id|pulse_session_id|getSessionId'

# Flutter
grep -rn 'sessionID\|session_id' --include='*.dart' lib 2>/dev/null \
  | grep -iE 'applicationVariables|secure_storage|callAPI'
```

If the Flutter target has no `applicationVariables` or equivalent, drop in
this minimal source first (matches the existing host pattern):

```dart
// lib/shared/utils/applicationVariables.dart
import 'package:uuid/uuid.dart';
class ApplicationVariables {
  final String sessionID = const Uuid().v4();
}
final applicationVariables = ApplicationVariables();
```

…then pass it the same way.

### 3b. Wire init by stack

| Stack | Where | How |
|---|---|---|
| Next.js App Router | `app/layout.tsx` | Create a tiny client component `app/AnalyticsBoot.tsx` that fires `track('session_started')` once on mount. Helper auto-detects session id from `sessionStorage`. Mount inside `<body>`. |
| Next.js Pages Router | `pages/_app.tsx` | `useEffect(() => { track('session_started'); }, [])`. Helper auto-detects from `sessionStorage`. |
| React SPA | `src/main.tsx` or `src/App.tsx` | Same `useEffect` pattern. |
| Flutter | `lib/main.dart` | After `WidgetsFlutterBinding.ensureInitialized()` and before `runApp()`: `await initAnalytics(sessionId: applicationVariables.sessionID);` |

For all: track is idempotent — the first `track(...)` call from anywhere in
the app boots persistence, kicks the flush timer, and registers the
visibility/pagehide listeners. Subsequent calls are cheap.

If the host app receives a new session id at runtime (rare — e.g. a
downstream API rotates it), call `setSessionId(newId)` once and subsequent
events will carry it. Already-queued events keep the previous id —
that's intentional, the previous session ended at that moment.

## Step 4 — Wire page-view auto-capture (one place)

Don't manually add `track('page_viewed')` at every page. Use the router.

### Next.js App Router

Add to `app/AnalyticsBoot.tsx` (or wherever Step 3 lives):

```tsx
'use client';
import { useEffect, Suspense } from 'react';
import { usePathname, useSearchParams } from 'next/navigation';
import { track } from '@/lib/track';

function PageViewTracker() {
  const pathname = usePathname();
  const searchParams = useSearchParams();
  useEffect(() => {
    if (!pathname) return;
    track('page_viewed', {
      name: pathname,
      query: searchParams ? Object.fromEntries(searchParams.entries()) : {},
    });
  }, [pathname, searchParams]);
  return null;
}

export function AnalyticsBoot() {
  return <Suspense fallback={null}><PageViewTracker /></Suspense>;
}
```

Mount `<AnalyticsBoot />` inside `<body>` in `app/layout.tsx`.

### Next.js Pages Router

In `pages/_app.tsx`:

```tsx
useEffect(() => {
  const handler = (url: string) => {
    const path = url.split('?')[0] ?? url;
    track('page_viewed', { name: path });
  };
  handler(router.asPath);
  router.events.on('routeChangeComplete', handler);
  return () => { router.events.off('routeChangeComplete', handler); };
}, [router]);
```

### React SPA (react-router v6+)

Inside the `<BrowserRouter>` tree, mount a small component:

```tsx
import { useLocation } from 'react-router-dom';
function RouteTracker() {
  const loc = useLocation();
  useEffect(() => {
    track('page_viewed', { name: loc.pathname });
  }, [loc.pathname]);
  return null;
}
```

### Flutter (Navigator)

In `lib/main.dart`:

```dart
MaterialApp(
  navigatorObservers: [TrackNavigatorObserver()],
  ...
)
```

(`TrackNavigatorObserver` is exported from `lib/track.dart`.)

### Flutter (go_router)

```dart
final _router = GoRouter(
  observers: [TrackNavigatorObserver()],
  routes: [...],
);
```

For path-only routes (no `name:`), set the name explicitly so the observer
fires:

```dart
GoRoute(path: '/foo', name: 'foo', builder: ...)
```

## Step 5 — Walk and instrument every interactive element (the main work)

This is the part that's different from an SDK install. Walk every source
file and instrument each interactive surface in place. Use the patterns
below per stack. Report what you're doing in batches — one PR worth at a
time, not 200 files in one shot.

### 5a. Discover interactive elements

**React / Next.js (TSX/JSX):**

```bash
# Buttons + links + clickables
grep -rn 'onClick=\|onSubmit=\|onChange=' --include='*.tsx' --include='*.jsx' src app components pages 2>/dev/null
grep -rn '<button\|<Button\|<a \|<Link\|<Pressable' --include='*.tsx' --include='*.jsx' src app components pages 2>/dev/null

# Forms
grep -rn '<form\|<Form' --include='*.tsx' --include='*.jsx' src app components pages 2>/dev/null

# Auth state hooks (so we know where to insert setUserId)
grep -rn 'useSession\|useAuth\|useUser\|onAuthStateChanged\|signIn\|signOut' --include='*.ts' --include='*.tsx' src app components pages 2>/dev/null
```

**Flutter (Dart):**

```bash
grep -rn 'onPressed:\|onTap:\|onChanged:\|onSubmitted:' --include='*.dart' lib
grep -rn 'ElevatedButton\|TextButton\|OutlinedButton\|InkWell\|GestureDetector\|IconButton' --include='*.dart' lib
grep -rn 'Form(' --include='*.dart' lib
grep -rn 'firebase_auth\|onAuthStateChanged\|FirebaseAuth' --include='*.dart' lib
```

### 5b. Instrument buttons / links / clickables

**Goal:** every click/tap calls `track('button_clicked', { ... })` BEFORE the
existing handler runs.

**React/Next.js — instrument by case:**

| Existing pattern | Transformation |
|---|---|
| `<button onClick={() => save()}>Save</button>` | `<button onClick={() => { track('button_clicked', { id: 'save' }); save(); }}>Save</button>` |
| `<button onClick={handleSave}>Save</button>` | `<button onClick={(e) => { track('button_clicked', { id: 'save' }); handleSave(e); }}>Save</button>` |
| `<Link href="/foo">Go</Link>` | `<Link href="/foo" onClick={() => track('button_clicked', { id: 'nav-foo', href: '/foo' })}>Go</Link>` |
| `<a href="/foo">Go</a>` | `<a href="/foo" onClick={() => track('button_clicked', { id: 'nav-foo', href: '/foo' })}>Go</a>` |

For the `id`/`name` field, derive a stable label from:
1. An existing `data-testid`, `id`, or `aria-label` attribute (use it).
2. The button text content if it's a single-string child.
3. The component name + a short suffix (e.g. `'Header.SignIn'`).

If the handler has more than three lines or is shared across components,
prefer adding `data-track-id="..."` and instrumenting via a parent
`onClickCapture` rather than rewriting each handler. Keep diffs small.

**Flutter — instrument by case:**

| Existing pattern | Transformation |
|---|---|
| `ElevatedButton(onPressed: () => save(), child: Text('Save'))` | `ElevatedButton(onPressed: () { track('button_clicked', {'id': 'save'}); save(); }, child: Text('Save'))` |
| `ElevatedButton(onPressed: handleSave, child: Text('Save'))` | `ElevatedButton(onPressed: () { track('button_clicked', {'id': 'save'}); handleSave(); }, child: Text('Save'))` |
| `InkWell(onTap: ..., child: ...)` | Same wrapping pattern |
| `IconButton(icon: Icon(Icons.save), onPressed: handleSave)` | Same wrapping pattern, with `'icon': 'save'` in properties |

For Flutter widgets that take a typed callback (e.g. `ValueChanged<String>`),
preserve the parameter:

```dart
TextField(onChanged: (value) {
  track('input_changed', {'field': 'email'});
  handleEmailChange(value);
})
```

### 5c. Instrument form submits

| Stack | Pattern |
|---|---|
| React/Next.js | Wrap `onSubmit`: `onSubmit={(e) => { track('form_submitted', { id: 'checkout', step: 1 }); handleSubmit(e); }}` |
| Flutter | Wrap the submit callback inside `Form.onChanged` or the explicit submit `onPressed` |

For `<form>` elements without an explicit `onSubmit` (HTML default submit),
add an empty handler that tracks first:

```tsx
<form action="/api/login" method="post" onSubmit={() => track('form_submitted', { id: 'login' })}>
```

### 5d. Instrument identify on auth state changes

**React/Next.js — add to the auth hook / provider:**

```tsx
import { setUserId, track } from '@/lib/track';

useEffect(() => {
  if (session?.user?.id) {
    setUserId(session.user.id);
    track('identify', { plan: session.user.plan });
  } else {
    setUserId(null);
  }
}, [session]);
```

Find the project's auth surface (NextAuth `useSession`, Firebase
`onAuthStateChanged`, custom `useAuth`, etc.) and add the snippet there
exactly once. Don't sprinkle `setUserId` calls.

**Flutter — wire to FirebaseAuth (or the app's auth stream):**

```dart
FirebaseAuth.instance.authStateChanges().listen((user) {
  if (user != null) {
    setUserId(user.uid);
    track('identify');
  } else {
    setUserId(null);
  }
});
```

### 5e. Confirm before doing it all in one shot

If 50+ files need editing, batch:

1. Apply changes to one feature area (e.g. `app/checkout/`) and `git diff`.
2. Show the user the diff — confirm the pattern is right.
3. Continue to the next area.

Do not silently rewrite the entire codebase in one pass. Each batch should
be reviewable.

## Step 6 — Env-var documentation

Web — append to `.env.local.example` (create if missing):

```
NEXT_PUBLIC_ANALYTICS_ENDPOINT=https://signal-collector.example.run.app
NEXT_PUBLIC_ANALYTICS_WRITE_KEY=wk_dev_change_me
NEXT_PUBLIC_APP_VERSION=
```

For Vite, use `VITE_*`. For CRA, use `REACT_APP_*`. The helper auto-detects.

Flutter — append to README:

```sh
flutter run \
  --dart-define=ANALYTICS_ENDPOINT=https://signal-collector.example.run.app \
  --dart-define=ANALYTICS_WRITE_KEY=wk_dev_change_me \
  --dart-define=APP_VERSION=1.0.0
```

## Step 7 — Verify

Pick the first that exists:

| Stack | Verify command |
|---|---|
| Next.js / React (TS) | `pnpm typecheck` / `yarn typecheck` / `npm run typecheck` if defined; else `npx tsc --noEmit` |
| React (JS) | `node --check src/lib/track.ts` is meaningless — run the project's lint / build instead |
| Flutter | `dart analyze` |

Don't declare done with a failing verify. If imports are wrong, fix paths.
If `'use client'` is missing on a wrapping client component, add it. If
Flutter analyzer flags `unused_element` on a typed callback, preserve the
parameter you wrapped.

## Step 8 — Smoke test

Print these for the user:

```sh
# Web — start dev server, click around, watch the Network tab for POSTs
# to /v1/events. You should see 202 responses.

# Flutter — flutter run with --dart-define vars set, navigate, observe
# the collector's logs.

# Both: confirm rows in BigQuery
bq query --use_legacy_sql=false --project_id=letztrip-production-account \
  'SELECT event_id, event_name, anonymous_id, properties, server_ts
   FROM analytics.events
   WHERE server_ts > TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 10 MINUTE)
   ORDER BY server_ts DESC LIMIT 20'
```

## Guardrails

- **NEVER** edit files inside the signal repo itself.
- **NEVER** include the write key in committed code. Always env var.
- **NEVER** rewrite the whole codebase in one batch — feature-area at a time, with diffs reviewable.
- **NEVER** skip the verify step. A green typecheck is the gate.
- **NEVER** turn `track.ts` / `track.dart` into a class-based SDK. The whole point is direct call sites — the helper exposes only top-level functions: `track`, `setUserId`, `reset`.
- **NEVER** add a global click listener that fires `track('button_clicked')` for every DOM click. We're instrumenting deliberately — global listeners create noise and miss meaningful properties.
- **NEVER** instrument `onChange` for every input — that fires per-keystroke. Only instrument `onSubmit`, `onBlur`, or specific value commits.
- If a target repo already imports `@example/analytics-web` or a similar package, this is a previous "SDK" install. Stop and tell the user — they need to remove the old package before this skill writes inline `track()` calls.

## What NOT to instrument

To avoid noise:

- Routine UI state changes (collapse/expand, hover, focus, keystroke).
- Programmatic side effects (re-renders, effect cleanups).
- Internal navigation that's not a user action (router redirects after auth).
- Loading states / skeletons.

Use product judgment — every event is a row in BigQuery. Aim for "what would
a PM actually want in a funnel?", not "every line of code that runs."
