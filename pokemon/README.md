# Shapemon — Ember Quest

A small, web-based, Gen-1-style **creature-battle RPG** built with vanilla
HTML5 Canvas. Grid movement, tile-accurate collision, a starter creature,
turn-based battles, a gym, and a credits screen.

> **Original work.** This is *inspired by* the classic monster-RPG formula but
> shares no assets, names, maps, characters, or story with any existing game.
> All creatures, moves, and towns here are original. Sprites are drawn from
> simple geometric shapes (circles, flames, spiked stars) sized to the tile grid.

## Play

Open `index.html` in any modern browser — no build step, no server required.

- **Move:** Arrow keys / WASD
- **Confirm:** Z / Enter
- **Back:** X / Esc

## The game loop

1. **Title screen** → press Z/Enter.
2. You wake in your room in **Willow Town** and step outside.
3. Visit **Prof. Cedar's lab** to receive your starter — the Fire-type
   **Emberling** (hardcoded first partner).
4. Walk **north** along the route; tall grass triggers wild battles that level
   you up.
5. Enter **Fernwood Gym** and battle **Leader Fern** (Grass-type — your fire has
   the advantage).
6. Win → a **thank-you / credits** screen.

## Systems

| System        | Notes |
|---------------|-------|
| Map / tiles   | String-defined tilemaps parsed into a grid; camera follows the player and clamps to map bounds. |
| Collision     | Trees, walls, water, and NPCs block movement; grid-accurate per tile. |
| Warps         | Door tiles teleport between the overworld and interiors (home, lab, gym). |
| Battle        | Turn-based, speed-ordered, with a 4-type effectiveness chart (fire/water/grass/normal). |
| Creatures     | Emberling (fire), Wormling (grass), Nibbit (normal), Thornbud (grass, gym). |

## Test / screenshots

A Playwright harness verifies the game boots without errors, checks collision,
drives a full battle to confirm the win → credits transition, and captures the
screenshots in `screenshots/`.

```bash
cd pokemon
PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 npm install playwright
node test.mjs
```
