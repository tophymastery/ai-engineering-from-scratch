# CONTEXT.md — what Shapemon is

An original creature-battle RPG on HTML5 Canvas. Grid overworld, warps between
towns/interiors, wild + trainer + gym battles, catching, party management,
items/shop, save/continue, evolution, an 8-badge story arc, credits.

## Architecture (folders = responsibilities)

```
index.html            <canvas width=704 height=480> + <script type="module" src="src/game.js">
styles/style.css
serve.mjs             zero-dependency static server (ES modules need http)
src/
  game.js             main loop (update+render), wiring, window.__shapemon test hook
  input.js            keyboard -> state-routed actions
  state.js            ALL mutable state (single source of truth) + STATE enum + DIRV
  core/
    screen.js         canvas/ctx + geometry (TILE, SCALE, TS, viewport, sidebar)
    rng.js            single overridable RNG: rand(), rint(n), rrange(lo,hi), setRng(fn)
  data/               ALL CONTENT (plain objects, human-readable)
    config.js         grid, screen, party cap, economy, battle constants, animation, catch
    types.js          TYPE_COLORS, PHYSICAL_TYPES, TYPE_CHART, typeEffectiveness, isSpecial
    moves.js          MOVES dex
    species.js        SPECIES dex (base stats, sprite, expYield, learnset, evolvesTo)
    items.js          ITEMS dex, STARTING_BAG, SHOP_STOCK
    encounters.js     ENCOUNTERS per map (rate + weighted table)
    story.js          ALL dialog/script text + CREDITS
    maps.js           map builders, MAPS, WARPS, GATES, NPCS, DOORS, GYM_BADGES, tile helpers
  engine/             pure-ish logic (no DOM except where noted) -> Node-unit-testable
    stats.js          calcStat, expForLevel, levelForExp, recomputeStats, gainExp, expYield
    creature.js       makeMove, makeCreature, makeParty, movesAtLevel, learnMove, evolveIfReady, resetStages
    damage.js         calcDamage, critChance, stageMult
    dialog.js         say(lines, after), advanceDialog
    world.js          movement, collision, warps, gates, encounters, interact, startGame
    battle.js         full battle engine (wild/trainer/gym), animation stepper
    party.js          healParty, firstHealthy, partyWiped
    bag.js            addItem, removeItem, itemQty, usableInBattle, applyItem
    shop.js           canAfford, buyItem
    pc.js             depositToBox, withdrawFromBox
    save.js           hasSave, saveGame, continueGame, newGame (localStorage)
    objective.js      objective() -> current guided goal string (flag-driven)
  render/             Canvas drawing (DOM)
    sprites.js        COLORS, drawTile, drawCreature, drawPlayer, drawNPC
    hud.js            roundRect, drawBox, wrap, hpColor, drawHPBox, drawSidebar
    world_render.js   camera + tilemap viewport + player/NPCs + sidebar + dialog box
    battle_render.js  framed battle layout + 2x2 menus + animation
    scenes.js         title (New Game/Continue), credits
    shop_render.js    mart screen
    pc_render.js      storage box screen
tests/
  logic.test.mjs      Node, no browser (formulas, data integrity, map reachability, sim battle)
  ui.test.mjs         Playwright: full start->8 gyms->credits walkthrough + per-scene screenshots
  coverage.test.mjs   Playwright: exhaustive (every warp/gate/item/move/npc/trainer/gym/status/...)
```

## Game states (`STATE`)
`title, world, dialog, battle, shop, pc, credits`. `render()` switches on
`game.state`; `input.js` routes key presses per state.

## State shape (`src/state.js`)
- `flags`: `{ hasStarter, badges: [] }` (badges[i] === true once gym i cleared)
- `player`: `{ map, x, y, dir, px, py, moving, from, to, progress, party[], bag[], box[], money }`
- `game`: `{ state, afterDialog, tick, forceEncounter, noEncounter, titleIndex }`
- `dialog`: `{ lines[], index, active, speaker }`
- `battle`: `{ kind, enemy, ally, enemyParty[], enemyIdx, phase, cmd, moveIndex, bagIndex, partyIndex, mustSwitch, foeName, canRun, msg[], fx[], msgIndex, afterMsg, onWin, anim }`
- `shop`, `pc`: small menu-cursor objects

## Data schemas

**Species** (`SPECIES[id]`): `{ name, type, sprite:{shape,color}, base:{hp,atk,def,spAtk,spDef,speed}, expYield, learnset:[{level,move}], evolvesTo?:{level,into} }`

**Move** (`MOVES[id]`): `{ id, name, type, power, accuracy, pp, priority?, multi?, recoil?, heal?, effect? }`
where `effect` is one of `{status,chance}` | `{stat,stages,target}` | `{flinch}`.

**Item** (`ITEMS[id]`): `{ id, name, kind:"heal"|"status"|"revive"|"ball", value?, ballBonus?, price, desc, usableInBattle, usableInField }`

**Map**: registered as `{ name, grid:char[][], w, h, theme }`. Tile legend:
`. grass  _ path  : tall-grass(encounter)  T tree  # roof  b blue-roof  = wall
R rock  ~ water  F floor  D interior-exit  H/L/G/C/M/V overworld doors  E gate`.

**Warp**: `WARPS["<map>:<x>,<y>"] = { map, x, y, dir }` (stepping the tile teleports).

**Gate**: `GATES["<map>"] = { need:<badgeIndex>, warp:{map,x,y,dir} | credits:true }`
(stepping an `E` tile warps onward if that badge is earned, else shows a lock).

**NPC**: `NPCS["<map>"] = [{ x, y, color, role, name, ... }]`; roles:
`prof, nurse, shop, pc, villager(dialog id), trainer(party, dialog id, defeated), gym(badge, intro, done, party)`.

**Encounter**: `ENCOUNTERS["<map>"] = { rate, table:[{species,min,max,weight}] }`.

## Mechanics (implement exactly — these are the FireRed/Gen-3 formulas)

- **Stat** (IV=EV=0, neutral): `HP = floor(2*Base*Level/100)+Level+10`;
  `Other = floor(2*Base*Level/100)+5`.
- **Damage**: `base = floor(floor(floor((2*Level/5+2)*Power*A/D)/50))+2`, then
  `×2 if crit`, `×1.5 if STAB`, `×typeEff`, `×rand(85..100)/100`; `max(1,·)` when
  `power>0 && eff>0`. `A/D` are Atk/Def (physical) or SpAtk/SpDef (special),
  category by move type (`isSpecial`). **Stat stages** multiply A/D:
  `+n → (2+n)/2`, `-n → 2/(2-n)`. **Burn** halves physical Atk. **Crit** chance
  `1/16`.
- **Type chart**: 9 types (`normal, fire, water, grass, electric, rock, ice,
  psychic, poison`), sparse rows, default 1×. Physical types: normal, rock,
  poison. Key 2× pairs: fire→grass/ice, water→fire/rock, grass→water/rock,
  electric→water, rock→fire/ice, ice→grass, psychic→poison, poison→grass.
- **Status**: burn/poison lose `1/8 maxHP` end of turn; paralysis 25% skip +
  ×0.5 speed; sleep lasts `rrange(1,3)` turns (skips while asleep). One status
  at a time.
- **Turn order**: higher move `priority` first, else higher effective speed,
  ties random. **Flinch** set by a faster attacker skips the target this turn.
  **Multi-hit** repeats damage; **recoil** costs the attacker a fraction of
  damage dealt; **heal** moves restore a fraction of maxHP.
- **EXP**: total for level L = `L^3` (medium-fast). Award on defeat =
  `floor(baseYield*foeLevel/7)`. Level-up recomputes stats, learns learnset
  moves at that level, and evolves if `evolvesTo.level` reached.
- **Catch**: `p = clamp(ballBonus*(3*maxHP-2*hp)/(3*maxHP)*statusBonus, 0..1)`;
  on success add to party (≤6) or box.
- **Enemy AI**: 25% random, else the highest-damage move vs the current ally.

## World & story arc (8 badges)

Gate chain (each gate needs the badge from the gym in that region):
`Willow Town(0, Fern/Grass) → Tidewater(1, Marina/Water) → Cinder(2, Rocco/Normal)
→ Voltage City(3, Volt/Electric) → Stonehaven(4, Terra/Rock) → Glacia(5,
Frost/Ice) → Mindspire(6, Sage/Psychic) → Miasma Marsh(7, Venia/Poison) → credits`.

The first three regions are hand-authored (`buildTown` + two `buildRegion`s);
gyms 4–8 are generated in a loop from a `LATE[]` config that reuses the generic
`buildRegion` (same door coords → uniform warps/gates). Each region has a gym, an
Ace trainer, a heal center + Storage PC, and scaled wild encounters. The sidebar
shows badges, money, party, and a flag-driven `objective()` guiding the player
gym-by-gym.
