/* Save / load via localStorage, plus New Game reset. Human-readable JSON. */
import { CONFIG } from "../data/config.js";
import { STATE, game, player, flags, battle } from "../state.js";
import { NPCS } from "../data/maps.js";
import { STORY } from "../data/story.js";
import { makeCreature } from "./creature.js";
import { say } from "./dialog.js";

const KEY = "shapemon-save-v1";
const TILE = CONFIG.grid.tile;

export function hasSave() {
  try { return !!localStorage.getItem(KEY); } catch { return false; }
}

export function saveGame() {
  const data = {
    flags: { ...flags },
    player: {
      map: player.map, x: player.x, y: player.y, dir: player.dir,
      party: player.party.map((c) => ({
        id: c.speciesId, level: c.level, exp: c.exp, hp: c.hp,
        moves: c.moves.map((m) => ({ id: m.id, pp: m.pp })),
      })),
    },
    trainers: (NPCS.town || []).filter((n) => n.role === "trainer" && n.defeated).map((n) => n.name),
  };
  try { localStorage.setItem(KEY, JSON.stringify(data)); } catch { /* ignore */ }
}

export function continueGame() {
  let data; try { data = JSON.parse(localStorage.getItem(KEY)); } catch { return false; }
  if (!data) return false;

  Object.assign(flags, data.flags);
  player.map = data.player.map; player.x = data.player.x; player.y = data.player.y;
  player.dir = data.player.dir; player.px = player.x * TILE; player.py = player.y * TILE;
  player.moving = false;
  player.party = data.player.party.map((s) => {
    const c = makeCreature(s.id, s.level);
    c.exp = s.exp; c.hp = s.hp;
    for (const sm of s.moves) { const m = c.moves.find((mm) => mm.id === sm.id); if (m) m.pp = sm.pp; }
    return c;
  });
  const won = new Set(data.trainers || []);
  for (const n of (NPCS.town || [])) if (n.role === "trainer") n.defeated = won.has(n.name);

  game.state = STATE.WORLD;
  return true;
}

export function newGame() {
  Object.assign(flags, { hasStarter: false, gymBadge: false });
  Object.assign(player, {
    map: "home", x: 5, y: 3, dir: "down", px: 5 * TILE, py: 3 * TILE,
    moving: false, from: null, to: null, progress: 0, party: [], bag: [],
  });
  for (const n of (NPCS.town || [])) if (n.role === "trainer") n.defeated = false;
  battle.anim = null;
  try { localStorage.removeItem(KEY); } catch { /* ignore */ }
  game.state = STATE.WORLD;
  say(STORY.intro);
}
