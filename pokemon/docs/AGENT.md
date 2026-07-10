# AGENT.md — how to (re)build Shapemon

You are an autonomous engineering agent. Your job: build **Shapemon — Ember Quest**,
an original, Gen-1/3-style creature-battle RPG that runs in the browser with no
build step, is **100% data-driven**, and is verified by an automated test suite.

Read `CONTEXT.md` (what to build), `PLAN.md` (the order), and `SKILL.md` (how,
including the full test catalog). This file is the working contract.

## Non-negotiable guardrails

1. **Original content only.** Do **not** reproduce any copyrighted game's
   creatures, names, sprites, maps, movepools, item lists, or per-species stat
   lines. Invent your own creatures, moves, towns, and story. Game *mechanics
   and formulas* (stat/damage math, type effectiveness, EXP curve) are systems,
   not creative content — reproducing those faithfully is fine and encouraged.
2. **Sprites are simple geometric shapes** (circle, flame, blob, spiked star)
   drawn on Canvas — no external art, no image assets. Everything is inlined.
3. **No external network assets.** Self-contained: vanilla ES modules + Canvas.

## Working method

- **Data first.** All content lives in `src/data/*` as plain, human-readable
  objects. The engine reads data; it never hardcodes content. Adding a creature,
  move, town, or gym must be a data edit, not an engine edit.
- **Deterministic testability.** Route *all* randomness through one overridable
  RNG (`src/core/rng.js`). Expose a test hook `window.__shapemon` from the main
  module with state + helpers so tests can drive the real game.
- **Test every feature.** Nothing is "done" until it has an assertion. Keep three
  suites green at all times: `logic` (Node), `ui` (Playwright walkthrough),
  `coverage` (Playwright, exhaustive). See `SKILL.md` for the full catalog.
- **Verify by playing.** The UI suite must play the whole game start → all gyms →
  credits via real key presses (BFS pathfinding). Capture screenshots of every
  scene.
- **Small, green increments.** Build one system, test it, commit it. Never
  commit red.

## Definition of done (per feature / overall)

- All three test suites pass; the full walkthrough reaches the credits.
- `npm start` serves a playable game; opening it and playing works end-to-end.
- Screenshots of each new scene are captured by the tests.

## Environment gotchas (learned the hard way)

- **`data/` may be git-ignored.** Some repos ignore any `data/` directory. If
  `src/data/*` silently won't commit, force-add it (`git add -f src/data/*.js`)
  and add a negation to the local `.gitignore` (`!src/data/`, `!src/data/**`).
  Symptom: the game runs locally (reads disk) but is broken after a fresh clone
  (module-not-found for data files).
- **ES modules need HTTP, not `file://`.** Ship a tiny zero-dependency dev
  server (`serve.mjs`) and load the game over `http://localhost`. Playwright
  tests must serve the game, not open `file://`.
- **A module load error is silent** except as a `pageerror` ("does not provide
  an export named X"). Always attach a `pageerror` listener in tests and assert
  zero JS errors, or `window.__shapemon` will be `undefined` and everything
  times out.
- **Working directory can reset between shell calls.** Run tests from the
  project directory explicitly.
- **Favicon 404** is harmless noise; filter `Failed to load resource` out of the
  console-error assertion.

## Git / PR workflow

- Branch per unit of work. Commit only green code. One feature ≈ one PR.
- If your branch was already merged (squash), restart it from the latest default
  branch and cherry-pick/rebase your new commit; push with `--force-with-lease`.
- Keep `node_modules/` and `package-lock.json` out of git.
