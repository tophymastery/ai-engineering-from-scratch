# PLAN.md — build order to (re)create Shapemon

Build in phases. Each phase ends **green** (its tests pass) before the next.
This is the clean re-derivation of how the game was actually built.

## Phase 0 — scaffold
- `index.html` (704×480 canvas, `type="module"` entry), `styles/style.css`,
  `serve.mjs` (static http), `package.json` (`type:module`, test scripts).
- `src/core/screen.js` (canvas/geometry), `src/core/rng.js` (overridable RNG),
  `src/state.js` (STATE enum, DIRV, empty state objects).
- **Done when:** `npm start` serves a black canvas with no console errors.

## Phase 1 — data layer (content)
- `data/config.js`, `data/types.js` (start with 4 types: normal/fire/water/grass;
  expand to 9 in Phase 8), `data/moves.js`, `data/species.js` (a fire starter +
  a few wild/gym creatures), `data/items.js`, `data/encounters.js`,
  `data/story.js`, `data/maps.js` (one hand-authored town + interiors).
- **Done when:** logic tests assert data completeness (every species type is in
  the chart, moves valid, sprites present) and map integrity (rectangular,
  reachable).

## Phase 2 — engine core (pure, Node-testable)
- `engine/stats.js`, `engine/creature.js`, `engine/damage.js`.
- **Done when:** logic tests verify stat formula, EXP curve (`L^3`), damage
  (STAB, super-effective > neutral, crit > non-crit, min-1), level-up recompute.

## Phase 3 — overworld
- `engine/world.js` (grid move, collision, warps, encounters, interact),
  `engine/dialog.js`, `engine/party.js`.
- `render/sprites.js`, `render/hud.js`, `render/world_render.js` (camera clamps
  to bounds; sidebar), `render/scenes.js` (title menu).
- `input.js` (held-key movement + single-step-on-tap; routes by state),
  `src/game.js` (loop + `window.__shapemon` hook).
- **Done when:** UI test: new game → dialog → collision (walls clamp) → walk into
  a building (map changes) → back.

## Phase 4 — battle engine
- `engine/battle.js` (message/fx queue, speed-ordered turns, faint/EXP/level-up),
  `render/battle_render.js` (foe/ally HP boxes, sprites on platforms, 2×2 command
  menu, move list, animation: lunge/flash/HP-drain/faint).
- Wild encounters from tall grass; hardcode the fire starter via the professor.
- **Done when:** UI test: grass encounter → win → level up; a gym battle → badge;
  win → credits.

## Phase 5 — meta systems
- Catching (`ball` items) + 6-slot party + in-battle **switch** (`engine/battle`
  party phase) + Storage PC (`engine/pc.js`, `render/pc_render.js`).
- Items + `engine/bag.js` (in-battle PACK use) + Mart shop (`engine/shop.js`,
  `render/shop_render.js`) + prize money.
- Save/continue + New Game (`engine/save.js`, localStorage).
- Trainer battles (NPC role) + villager dialog + nurse heal.
- **Done when:** coverage test asserts each item's effect, catching, switching,
  deposit/withdraw, shop buy, save round-trip.

## Phase 6 — status, stages, growth
- Status conditions (burn/poison/paralysis/sleep) + end-of-turn ticks + gating.
- Stat-stage moves (Growl/Tail Whip) + stage math in `damage.js`.
- Move-learning on level-up + evolution (`evolveIfReady`).
- **Done when:** a long deep-flow UI test asserts turn order (both directions),
  super/not-very-effective, crit, each status, a stat drop, and evolution.

## Phase 7 — guided arc + polish
- `engine/objective.js` (flag-driven goal) shown in the sidebar.
- Framed GBA-style battle HUD (rounded HP boxes, "HP" pill, EXP bar, grassy
  platforms, 2×2 move panel + PP/TYPE).
- Full-map overview renderer for snapshots.

## Phase 8 — full content (8 gyms) + battle depth
- Expand `types.js` to 9 types (add electric/rock/ice/psychic/poison) with a
  correct sparse chart + physical/special split.
- Add typed creatures + moves for each gym type.
- Generic `buildRegion()`; generate gyms 4–8 from a `LATE[]` config in a loop
  (register maps/interiors, add warps/gates/npcs/encounters); re-route the gate
  chain to end after gym 8. Add `GYM_BADGES` registry; make `objective()` iterate
  it.
- Battle depth: move `priority`, `multi`-hit, `recoil`, `flinch`, `heal` moves;
  smarter enemy AI.
- **Done when:** the UI walkthrough clears all 8 gyms to credits; coverage
  asserts the 9-type chart, the 8-gym gate chain, and every leader party.

## Phase 9 — exhaustive coverage
- `tests/coverage.test.mjs`: every warp + gate entry point, every item, party
  switching, every move, every NPC role, every trainer, every gym, statuses,
  level-up/evolution, run, blackout, save/continue. See `SKILL.md` for the full
  catalog. **Done when:** all three suites green, zero JS errors, walkthrough
  reaches credits.

## Acceptance (whole game)
`npm test` runs logic + ui + coverage, all green; the walkthrough plays
start → 8 gyms → credits; screenshots exist for every scene.
