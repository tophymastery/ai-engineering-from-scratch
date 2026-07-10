# Shapemon — Ember Quest

A web-based, Gen-1/3-style **creature-battle RPG** (HTML5 Canvas, no build step),
built to be **fully data-driven** and **completely testable**.

> **Original work.** This game is *inspired by* the classic monster-RPG formula
> but shares **no assets, names, maps, creatures, movepools, items, or story**
> with any existing game. Every creature/move/item/map/line of dialog here is
> original. Sprites are simple geometric shapes sized to the tile grid.
>
> The **battle engine reproduces the authentic FireRed (Gen-3) mechanics** —
> the six-stat model, the stat formula, the damage formula (STAB, type chart,
> 1/16 critical hits, physical/special split, 85–100 random factor, min-1 rule),
> PP, and the `level³` EXP curve — because game *mechanics/formulas* are systems,
> not copyrightable content. Plug your own numbers into `src/data/*` to retune.

## Rebuild blueprint (for AI agents)

`docs/` contains a self-contained spec to recreate this whole game — copy the
four files into an empty project and follow them:

- **[docs/AGENT.md](docs/AGENT.md)** — the working contract: guardrails (original
  content only), method, definition of done, environment gotchas, git workflow.
- **[docs/CONTEXT.md](docs/CONTEXT.md)** — what the game is: architecture, data
  schemas, exact mechanics/formulas, and the 8-badge world arc.
- **[docs/PLAN.md](docs/PLAN.md)** — phase-by-phase build order with acceptance
  criteria.
- **[docs/SKILL.md](docs/SKILL.md)** — reusable techniques + the **complete test
  catalog** (all three suites, every assertion).

## Play

```bash
cd pokemon
npm start           # serves http://localhost:8080  (ES modules need http, not file://)
```

- **Move:** Arrows / WASD  ·  **Confirm:** Z / Enter  ·  **Back:** X / Esc
- Title screen: **New Game** / **Continue** (save is automatic on every map change).

## The journey (start → two gyms → ending)

1. Wake in your room, step outside into **Willow Town**.
2. Visit **Prof. Cedar's lab** for your starter — the Fire-type **Emberling**.
3. Explore: **Heal Center**, **Mart** (buy items with prize money), **Rocky
   Cave**, villagers, and **trainer battles** (Camper Rick, Scout Mia).
4. Tall grass triggers **random wild encounters** — battle them, **catch** them
   with a Snarebell (party of up to 6), and level up (creatures **evolve** and
   **learn moves**).
5. Beat **Leader Fern** at **Fernwood Gym** (Grass) to earn the **Leaf Badge**.
6. The gate north opens → **Tidewater Town**: catch a Grass type, then beat
   **Leader Marina's** Water gym for the **Tidewater Badge**.
7. Step through the final gate for the credits.

## Architecture

Everything is organized for maintainability. Game *content* is separate from the
*engine*, which is separate from *rendering*.

```
pokemon/
├── index.html            # canvas + module entry
├── styles/style.css
├── serve.mjs             # zero-dependency static dev server
├── src/
│   ├── game.js           # main loop + wiring + test hook (window.__shapemon)
│   ├── input.js          # keyboard → state-routed actions
│   ├── state.js          # shared mutable state
│   ├── core/             # screen geometry, RNG (injectable for tests)
│   ├── data/             # ← ALL CONFIG lives here (human-readable, editable)
│   │   ├── config.js         grid, screen, battle mechanics, animation
│   │   ├── types.js          type colors + Gen chart + phys/special split
│   │   ├── moves.js          move dex (power / accuracy / pp / type)
│   │   ├── species.js        creature dex (six-stat base lines + sprite)
│   │   ├── items.js          item dex + starting bag
│   │   ├── encounters.js     wild encounter tables per area
│   │   ├── maps.js           overworld builder + interiors + warps + NPCs
│   │   └── story.js          ALL dialog / script / credits text
│   ├── engine/           # stats, creature, damage, world, battle, party, save, dialog
│   └── render/           # sprites/tiles, hud + badges sidebar, world, battle, scenes
└── tests/
    ├── logic.test.mjs    # pure Node: formulas, data completeness, map reachability, sim battle
    └── ui.test.mjs       # Playwright: collision, building/cave entry, encounters, battles, full walkthrough
```

### Editing content

- **Add a creature:** copy an entry in `data/species.js` (base stats + sprite + moves).
- **Change stats/skills:** edit the numbers in `data/species.js` / `data/moves.js`.
- **Retune battle math:** edit `data/config.js` `battle` block (crit rate, STAB, random range…).
- **Rewrite the story / NPC lines:** edit `data/story.js`.
- **Reshape the world:** edit `data/maps.js`.

## Systems

| System | Notes |
|--------|-------|
| Map / camera | Large scrolling overworld (~4× a classic town/route), grid-based, camera clamps to bounds. |
| Collision | Trees, walls, water, rocks, and NPCs block movement; verified by tests. |
| Warps | Doors move between overworld and interiors (home, lab, heal center, gym, cave). |
| Dialog | Centralized script text; talk to villagers, professor, nurse, trainers, gym leader. |
| Battle | Authentic FireRed formulas + classic 2×2 menu + animation. Wild / trainer / gym, multi-creature parties, **status conditions** (burn/poison/paralysis/sleep), **stat stages**, PP. |
| Items / Shop | Bag with in-battle Potion/Full Heal/Revive/ball use; a **Mart** to buy items with prize money. |
| Catch / Party | Catch wild creatures with a ball; carry up to 6, **switch** mid-battle, extras go to storage. |
| Growth | EXP + level-up, **move learning**, and **evolution** (e.g. Emberling → Blazehound). |
| Save | Auto-save to `localStorage` (party/box/bag/money/badges) on map change; New Game / Continue. |
| World | Two towns (Willow + Tidewater), five+ interiors, badge-gated gates; sidebar tracks badges, money, party. |

## Testing

```bash
npm run test:logic   # Node, no browser — 100+ assertions
npm run test:ui      # Playwright — drives real key presses, writes screenshots/
npm test             # both
```

The UI suite plays the game end-to-end with **BFS pathfinding over real key
presses** and captures the screenshots in `screenshots/`.
