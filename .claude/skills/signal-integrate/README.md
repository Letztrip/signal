# signal-integrate (Claude Code skill)

Drops a single `track()` helper file into a target frontend repo, then
walks **every page, button, link, form, and auth-state change** and
inserts tracking calls at each call site. No SDK, no package — the helper
is one file (~150 lines), the rest is inline `track('event_name', { ... })`
calls in the host code.

This is the **canonical** copy. The signal repo owns it. To use it from
target repos, copy or symlink it into `~/.claude/skills/` so Claude Code
auto-discovers it from any working directory:

```sh
ln -s "$PWD/.claude/skills/signal-integrate" ~/.claude/skills/signal-integrate
```

(Run that from the signal repo root. Re-run on each machine that needs
the skill.)

## Stacks supported

- Next.js (App Router or Pages Router)
- React SPA (Vite or CRA, with react-router)
- Flutter (Navigator or go_router)

## Layout

```
.claude/skills/signal-integrate/
├── SKILL.md                            # what Claude follows
├── README.md                           # this file
└── helpers/
    ├── track.ts                        # → drop into web target as src/lib/track.ts
    └── track.dart                      # → drop into flutter target as lib/track.dart
```

## How to invoke

From a target repo (NOT the signal repo), run Claude Code and say:

> "Integrate signal — collector at https://signal-collector.example.run.app"

Claude detects the stack, drops the helper, wires init + page-view
auto-capture, and walks every interactive element to insert `track(...)`
calls. See the signal repo README §12 for the full step-by-step.

## What this skill explicitly avoids

- No `Analytics.init()` ceremony — the helper boots itself on first call.
- No singleton class to import — only top-level `track`, `setUserId`, `reset`.
- No global click listener — every interactive element is instrumented in place.
- No `onChange` instrumentation per keystroke — only commits / submits.
- No package dependency for web (uses `localStorage`, no `idb-keyval`).
