/* Main entry point: wires modules together, runs the update/render loop, and
 * exposes a test hook (window.__shapemon) used by the automated test suite. */
import { ctx, W, H } from "./core/screen.js";
import { setRng } from "./core/rng.js";
import { CONFIG } from "./data/config.js";
import { STATE, game, player, flags, battle, shop } from "./state.js";
import { MAPS, WARPS, NPCS, DOORS, NORTH_DOORS, EAST_DOORS, GATES, isBlocked, tileAt, isEncounterTile } from "./data/maps.js";
import { SPECIES } from "./data/species.js";
import { MOVES } from "./data/moves.js";
import { ITEMS, SHOP_STOCK } from "./data/items.js";
import { ENCOUNTERS } from "./data/encounters.js";
import { typeEffectiveness } from "./data/types.js";
import { calcStat, expForLevel, levelForExp, gainExp, expYield } from "./engine/stats.js";
import { calcDamage } from "./engine/damage.js";
import { makeCreature, makeMove, makeParty } from "./engine/creature.js";
import { addItem, itemQty, usableInBattle } from "./engine/bag.js";
import { buyItem } from "./engine/shop.js";
import { tryMove, updateMovement, doWarp } from "./engine/world.js";
import { startWildBattle, startGymBattle, startTrainerBattle, updateBattleAnim } from "./engine/battle.js";
import { healParty } from "./engine/party.js";
import { newGame, continueGame, hasSave, saveGame } from "./engine/save.js";
import { objective } from "./engine/objective.js";
import { renderWorld } from "./render/world_render.js";
import { renderBattle } from "./render/battle_render.js";
import { renderShop } from "./render/shop_render.js";
import { renderPc } from "./render/pc_render.js";
import { renderTitle, renderCredits } from "./render/scenes.js";
import { keys, onPress, initInput } from "./input.js";

function update() {
  game.tick++;
  if (game.state === STATE.WORLD && !player.moving) {
    for (const d of ["up", "down", "left", "right"]) if (keys.has(d)) { tryMove(d); break; }
  }
  updateMovement();
  if (game.state === STATE.BATTLE) updateBattleAnim();
}

// On-demand full-map overview (whole tilemap scaled to fit) for map snapshots.
const OVERVIEW_COLORS = {
  ".": "#5bab53", "_": "#d8c58c", ":": "#2f6b3b", T: "#1f5a2c", "~": "#3f6fd4",
  R: "#6d6f77", "#": "#b5423b", b: "#3f6fb5", F: "#c9b79a", "=": "#6b5640",
  H: "#5a3a1e", L: "#5a3a1e", G: "#8f302b", C: "#2c4f85", M: "#3f6fb5", V: "#101018", E: "#ffd23f", D: "#5a3a1e",
};
function renderOverview(name) {
  const m = MAPS[name];
  const cell = Math.floor(Math.min((W - 40) / m.w, (H - 60) / m.h));
  const ox = (W - m.w * cell) / 2, oy = (H - m.h * cell) / 2 + 8;
  ctx.fillStyle = "#0f1020"; ctx.fillRect(0, 0, W, H);
  for (let y = 0; y < m.h; y++) for (let x = 0; x < m.w; x++) {
    ctx.fillStyle = OVERVIEW_COLORS[m.grid[y][x]] || "#5bab53";
    ctx.fillRect(ox + x * cell, oy + y * cell, cell, cell);
  }
  for (const n of (NPCS[name] || [])) {
    ctx.fillStyle = n.role === "gym" ? "#ffef6b" : n.role === "trainer" ? "#ff7a3a" : "#ffffff";
    ctx.beginPath(); ctx.arc(ox + (n.x + 0.5) * cell, oy + (n.y + 0.5) * cell, Math.max(2, cell * 0.4), 0, 7); ctx.fill();
  }
  ctx.fillStyle = "#e9e9f2"; ctx.textAlign = "center"; ctx.textBaseline = "top";
  ctx.font = "bold 16px 'Courier New', monospace";
  ctx.fillText(name === "town" ? "WILLOW TOWN — full map" : name === "north" ? "TIDEWATER TOWN — full map" : name.toUpperCase(), W / 2, 10);
  ctx.textAlign = "left";
}

function render() {
  if (game.overview) return renderOverview(game.overview);
  if (game.state === STATE.TITLE) return renderTitle();
  if (game.state === STATE.CREDITS) return renderCredits();
  if (game.state === STATE.BATTLE) return renderBattle();
  if (game.state === STATE.SHOP) return renderShop();
  if (game.state === STATE.PC) return renderPc();
  renderWorld();
}

function frame() { update(); render(); requestAnimationFrame(frame); }

// ---- Test / automation hook ----
window.__shapemon = {
  STATE, game, player, flags, battle, shop,
  MAPS, WARPS, NPCS, DOORS, NORTH_DOORS, EAST_DOORS, GATES, SPECIES, MOVES, ITEMS, SHOP_STOCK, ENCOUNTERS,
  api: { calcStat, calcDamage, typeEffectiveness, expForLevel, levelForExp, gainExp, expYield, makeCreature, makeMove, makeParty },
  isBlocked, tileAt: (m, x, y) => tileAt(MAPS[m], x, y), isEncounterTile,
  setRng,
  setForceEncounter: (v) => { game.forceEncounter = !!v; },
  setNoEncounter: (v) => { game.noEncounter = !!v; },
  giveStarter: () => { flags.hasStarter = true; player.party = [makeCreature(CONFIG.starter.species, CONFIG.starter.level)]; },
  healParty, warpTo: doWarp,
  startWildBattle: (area) => startWildBattle(area || ENCOUNTERS.town),
  startGymBattle, startTrainerBattle,
  addItem, itemQty, usableInBattle, buyItem,
  newGame, continueGame, hasSave, saveGame, objective,
  setOverview: (name) => { game.overview = name || null; },
  press: onPress,
};

initInput();
requestAnimationFrame(frame);
