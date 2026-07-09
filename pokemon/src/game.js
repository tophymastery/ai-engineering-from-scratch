/* Main entry point: wires modules together, runs the update/render loop, and
 * exposes a test hook (window.__shapemon) used by the automated test suite. */
import { ctx } from "./core/screen.js";
import { setRng } from "./core/rng.js";
import { CONFIG } from "./data/config.js";
import { STATE, game, player, flags, battle, shop } from "./state.js";
import { MAPS, WARPS, NPCS, DOORS, NORTH_DOORS, GATES, isBlocked, tileAt, isEncounterTile } from "./data/maps.js";
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

function render() {
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
  MAPS, WARPS, NPCS, DOORS, NORTH_DOORS, GATES, SPECIES, MOVES, ITEMS, SHOP_STOCK, ENCOUNTERS,
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
  press: onPress,
};

initInput();
requestAnimationFrame(frame);
