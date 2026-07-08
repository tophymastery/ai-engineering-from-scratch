/* Overworld: grid movement, collision, warps, encounters, NPC interaction. */
import { CONFIG } from "../data/config.js";
import { MAPS, WARPS, GATES, npcAt, isBlocked, tileAt, isEncounterTile } from "../data/maps.js";
import { ENCOUNTERS } from "../data/encounters.js";
import { STORY } from "../data/story.js";
import { STATE, DIRV, player, game, flags, shop } from "../state.js";
import { SHOP_STOCK } from "../data/items.js";
import { rand } from "../core/rng.js";
import { say } from "./dialog.js";
import { healParty } from "./party.js";
import { makeCreature, makeParty } from "./creature.js";
import { startWildBattle, startTrainerBattle, startGymBattle } from "./battle.js";
import { saveGame } from "./save.js";

const TILE = CONFIG.grid.tile;

export function startGame() {
  game.state = STATE.WORLD;
  say(STORY.intro);
}

export function tryMove(dir) {
  if (player.moving) return;   // ignore input mid-step
  player.dir = dir;
  const [dx, dy] = DIRV[dir];
  const nx = player.x + dx, ny = player.y + dy;
  if (isBlocked(player.map, nx, ny)) return;   // blocked -> just turn
  player.moving = true;
  player.from = { x: player.x, y: player.y };
  player.to = { x: nx, y: ny };
  player.progress = 0;
}

export function doWarp(w) {
  player.map = w.map; player.x = w.x; player.y = w.y; player.dir = w.dir;
  player.px = w.x * TILE; player.py = w.y * TILE; player.moving = false;
  if (flags.hasStarter) saveGame();   // autosave on every warp
}

export function finishMove() {
  player.x = player.to.x; player.y = player.to.y;
  player.px = player.x * TILE; player.py = player.y * TILE;
  player.moving = false;

  const warp = WARPS[`${player.map}:${player.x},${player.y}`];
  if (warp) { doWarp(warp); return; }

  const ch = tileAt(MAPS[player.map], player.x, player.y);

  // Badge-gated gate: warp onward (or roll credits) if the badge is earned.
  if (ch === "E") {
    const gate = GATES[player.map];
    if (gate && flags.badges[gate.need]) {
      if (gate.credits) game.state = STATE.CREDITS;
      else doWarp(gate.warp);
    } else {
      say(STORY[`gateLocked${gate ? gate.need : 0}`] || STORY.gateLocked0);
    }
    return;
  }

  if (flags.hasStarter && !game.noEncounter && isEncounterTile(player.map, ch)) {
    const area = ENCOUNTERS[player.map];
    if (area && (game.forceEncounter || rand() < area.rate)) startWildBattle(area);
  }
}

export function updateMovement() {
  if (game.state === STATE.WORLD && player.moving) {
    player.progress++;
    const t = player.progress / CONFIG.movement.moveFrames;
    player.px = (player.from.x + (player.to.x - player.from.x) * t) * TILE;
    player.py = (player.from.y + (player.to.y - player.from.y) * t) * TILE;
    if (player.progress >= CONFIG.movement.moveFrames) finishMove();
  }
}

export function interact() {
  const [dx, dy] = DIRV[player.dir];
  const npc = npcAt(player.map, player.x + dx, player.y + dy);
  if (!npc) return;

  switch (npc.role) {
    case "prof":
      if (!flags.hasStarter) say(STORY.profGive, () => {
        flags.hasStarter = true;
        player.party = [makeCreature(CONFIG.starter.species, CONFIG.starter.level)];
      });
      else say(STORY.profDone);
      break;
    case "nurse":
      say(STORY.nurse, () => { healParty(); if (flags.hasStarter) saveGame(); });
      break;
    case "villager":
      say(STORY.npc[npc.dialog] || ["..."]);
      break;
    case "shop":
      say(STORY.shopWelcome, () => { shop.stock = SHOP_STOCK.slice(); shop.index = 0; game.state = STATE.SHOP; });
      break;
    case "trainer":
      if (npc.defeated) { say([`${npc.name}: Great battle earlier!`]); break; }
      if (!flags.hasStarter) { say(["You need a Shapemon to battle!"]); break; }
      say(STORY.trainers[npc.dialog].intro, () =>
        startTrainerBattle(makeParty(npc.party), npc.name, () => {
          npc.defeated = true;
          say(STORY.trainers[npc.dialog].win);
        }));
      break;
    case "gym": {
      const b = npc.badge;
      if (!flags.hasStarter) say(STORY[npc.badge === 0 ? "gymNoStarter" : "gym2NoStarter"]);
      else if (!flags.badges[b]) say(STORY[npc.intro], () =>
        startGymBattle(npc.name, makeParty(npc.party), b, () => { flags.badges[b] = true; game.state = STATE.WORLD; }));
      else say(STORY[npc.done]);
      break;
    }
  }
}
