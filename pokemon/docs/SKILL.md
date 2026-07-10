# SKILL.md — techniques + the complete test catalog

Reusable techniques to build Shapemon, then the **full test inventory** (three
suites, 639 assertions) so every test can be recreated in an empty project.

## Core techniques

### 1. Grid movement + collision + camera
- Player has tile `(x,y)` + pixel `(px,py)`. On a key, if the target tile is
  walkable, animate `progress` 0→`moveFrames` interpolating px/py; on arrival
  snap to the tile and run `finishMove` (warp / gate / encounter checks).
- `isBlocked(map,x,y)` = tile not in the WALKABLE set **or** an NPC occupies it.
- Camera centers on the player, clamped to map bounds; if the map is smaller than
  the viewport, center it. Draw only visible tiles.
- Single-step on tap **and** continuous walk on held key; guard `tryMove` with
  `if (player.moving) return` so taps don't override an in-flight step.

### 2. String tilemaps + procedural region builder
- Author small maps as arrays of equal-length strings; parse to `char[][]`.
  Validate every row width at registration.
- For repeatable towns, a `buildRegion()` returns `{grid, doors}` with fixed door
  coordinates. Building all late regions from it makes warps/gates uniform and
  coordinate-safe. Place obstacles (trees/water/rock) only onto plain grass so
  buildings stay reachable (grass fills the gaps) — verify with a BFS test.

### 3. One overridable RNG
- `core/rng.js` exports `rand/rint/rrange/setRng`. **Every** random decision uses
  it. Tests call `setRng(() => 0.9)` (max, no-crit) or `() => 0.0` (force crit /
  skip) for determinism.

### 4. Battle turn engine (message/fx queue + animation)
- A turn builds two parallel arrays: `msg[]` (text lines) and `fx[]` (per-line
  animation events: `{act, hit, hp:{who,val}}` | `{faint}` | `null`). The player
  advances lines with a key; an animation stepper tweens HP toward `hp.val` and
  plays lunge/flash/faint when a line's fx appears. Logic (HP, faint, status) is
  applied at build time so tests can read outcomes without waiting on animation.
- Multi-creature enemy parties: on faint, if `enemyIdx+1 < enemyParty.length`
  send the next; else `finishWin` (prize money, `onWin`).

### 5. Test hook `window.__shapemon`
- The main module exposes state (`game, player, flags, battle, MAPS, WARPS,
  GATES, NPCS, GYM_BADGES, ...`), pure helpers (`api.calcDamage`, `api.makeCreature`,
  `api.expForLevel`, ...), and drivers (`setRng, setNoEncounter, giveStarter,
  warpTo, healParty, startWildBattle, saveGame, continueGame, newGame,
  objective, addItem, itemQty, usableInBattle, buyItem, isBlocked, tileAt`).
  Tests drive the *real* game through it — never a mock.

### 6. Dev server + Playwright harness
- `serve.mjs`: ~30-line `http` static server (ES modules require http). Tests
  `createServer().listen(0)`, get the port, launch Chromium with
  `executablePath` to the preinstalled browser + `--no-sandbox`.
- Attach `pageerror` + console-error listeners; assert **zero** JS errors
  (filter the favicon `Failed to load resource`). Clear `localStorage`, then
  `setRng` deterministic.
- **BFS walk over real keys:** compute a dir-path on the current map with BFS
  (treat warp/door tiles as non-traversable except the destination, or you'll
  teleport mid-path), then press arrow keys step-by-step, waiting for
  `!player.moving` between steps.

## The three suites (run all via `npm test`)

### A. `logic.test.mjs` — Node, no browser (~343 assertions)
Imports engine/data modules directly (they must be DOM-free). Sections:
- **type chart**: fire>grass=2, fire<water=0.5, water>fire=2, grass>water=2,
  normal=fire=1; 9-type additions (electric>water, rock>fire, rock>ice,
  ice>grass, psychic>poison, poison>grass, fire>ice); physical/special split.
- **stat formula**: exact values for known base/level (HP and non-HP).
- **exp curve**: `expForLevel(6)=216`, `(10)=1000`, `levelForExp(216)=6`;
  `gainExp` crosses a threshold, levels up, raises maxHP.
- **damage**: super-effective > neutral; eff flag; crit > non-crit; min-1.
- **data completeness**: every species (type in chart, sprite, 6 base stats,
  positive expYield, valid learnset, evolution target exists); every move (type
  color, numeric fields); every item (kind + value or ballBonus); starting bag +
  starter valid; encounters reference valid species; trainer/gym parties valid.
- **map integrity + reachability**: every map rectangular; every warp target is a
  valid, walkable tile; BFS from home-exit reaches every town door (G,L,C,V,M,E).
- **stat stages**: `stageMult(0)=1, (+2)=2, (-2)=0.5`; higher DEF stage lowers
  damage. **burn** lowers physical damage.
- **move learning & evolution**: early moves only at L1; `movesAtLevel`;
  `learnMove` refuses a 5th; `evolveIfReady` changes species at the threshold.
- **catch chance shape**: rises as HP drops.
- **second region + shop + 8-gym data**: `GYM_BADGES.length===8`, each badge
  index maps to a real gym map; gate chain `town→north→east→r4..r8→credits`
  (each gate `need===i`, warps to the next, last is credits); every leader has a
  party; SHOP_STOCK all valid.
- **storage PC**: withdraw moves box→party; deposit moves party→box; can't
  deposit the last member; can't exceed the party cap.
- **simulated battle**: deterministic RNG, drive `battleInput` FIGHT→move until a
  wild foe faints → returns to world + leveled up.

### B. `ui.test.mjs` — Playwright walkthrough + screenshots (~100 assertions)
Plays the game start → all 8 gyms → credits via real key presses:
1. Title screenshot; New Game → intro dialog; clear → world in home.
2. **Collision**: bump house walls → position clamps; movement happened.
3. Exit house → town (building-exit warp). Overworld + both full-town map
   overview screenshots.
4. Enter Lab, talk to prof, receive starter; objective advances; exit.
5. Enter Heal Center; enter Cave, walk on cave floor, exit.
6. **Dialog** with a villager; **trainer** battle (+ prize money).
7. **Shop**: enter Mart, open shop, buy an item (money↓, qty↑), leave.
8. Tall-grass **forced encounter** → wild battle → win → **level up**.
9. **Item** use (command menu, move list, bag menus, use Potion consumes it).
10. **Catch** (capture the "Gotcha!" frame; party grows).
11. **Switch** (party menu; active changes) with a known 2-member party.
12. **Status** paralysis on the HP box; deep-flow battle asserts all statuses,
    turn order both directions, effectiveness, crit, a stat drop, evolution.
13. **State coverage**: navigate every battle sub-state (2×2 command, 2×2 moves,
    bag, party) and back; RUN exits.
14. **Storage PC**: open, withdraw, deposit, exit.
15. Gym 1 → badge; town gate → Tidewater; Gym 2 → badge; gate → Cinder; Gym 3 →
    badge; then a strong team clears gyms 4–8 (warp to each gym + battle); all 8
    badges; final gate → credits screenshot.
Screenshots: title, intro, overworld, both town maps, cave, wild/gym battles,
dialog, trainer, shop, command/moves/bag menus, catch, party, status(×2), PC,
evolution, cinder, gym8, credits.

### C. `coverage.test.mjs` — Playwright, exhaustive (~196 assertions)
Boots, `newGame`, `giveStarter`, `setNoEncounter`, opens all gates
(`flags.badges=[8×true]`). Then, each with an assertion:
- **Entry points**: iterate the whole `WARPS` table — stand next to each door and
  step onto it; assert the resulting map equals the declared target (covers every
  building door AND every interior exit).
- **Gates**: for each `GATES` entry, step onto the region's `E` tile; assert it
  warps to the next region (final gate → credits state).
- **Items**: give all; each potion heals (hp↑, qty↓); Full Heal cures a status;
  Revive restores a fainted party member; each ball catches (party↑, qty↓).
- **Switch**: 3-member party; switch to slots 1 and 2; assert active changed.
- **Every move**: for each `MOVES` id, set it as the only move vs a tanky
  tackle-only foe; assert the "used <name>!" line plus its effect (power>0 →
  enemy HP↓; heal → ally HP↑; stat → stage changed; status → foe status set).
- **NPC roles**: iterate all `NPCS`; villager→dialog, nurse→heal to full,
  shop→opens, pc→opens, prof→dialog, all 8 gym-done→dialog.
- **Trainer battles** (all regions, identified by map+coords since late-region
  trainers share a name): intro → win → defeated flag + prize money.
- **Gym battles** (all 8): reset that badge, warp into the gym, intro → win →
  badge earned.
- **Statuses**: poison/burn end-of-turn tick messages; paralysis full-skip
  (RNG=0); sleep skip.
- **Level-up + evolution**: emberling one XP short → win → level up; L15 →
  win → evolves to blazehound.
- **Run + blackout**: RUN from a wild battle → world; a 1-HP lone party member vs
  a strong foe → party wipe → warp home.
- **Save/continue**: set money/badges/party/box → `saveGame` → wipe →
  `continueGame` → all restored.

## Reproduction checklist
Copy `AGENT.md`, `CONTEXT.md`, `PLAN.md`, `SKILL.md` into an empty repo. Follow
`PLAN.md` phase by phase; encode content from `CONTEXT.md`'s schemas; wire the
`window.__shapemon` hook early; recreate the three suites from this catalog and
keep them green. When all 639 assertions pass and the walkthrough reaches the
credits, the game is rebuilt.
