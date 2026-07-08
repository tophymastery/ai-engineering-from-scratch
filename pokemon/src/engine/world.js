/* Overworld: grid movement, collision, warps, encounters, NPC interaction. */
import { CONFIG } from "../data/config.js";
import { MAPS, WARPS, npcAt, isBlocked, tileAt, isEncounterTile } from "../data/maps.js";
import { ENCOUNTERS } from "../data/encounters.js";
import { STORY } from "../data/story.js";
import { STATE, DIRV, player, game, flags } from "../state.js";
import { rand } from "../core/rng.js";
import { say } from "./dialog.js";
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

  // Victory Gate: the ending. Opens once the Leaf Badge is earned.
  if (ch === "E") {
    if (flags.gymBadge) { game.state = STATE.CREDITS; }
    else say(STORY.gateLocked);
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
      say(STORY.nurse, () => { for (const c of player.party) { c.hp = c.maxhp; c.moves.forEach((m) => (m.pp = m.maxpp)); } });
      break;
    case "villager":
      say(STORY.npc[npc.dialog] || ["..."]);
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
    case "gym":
      if (!flags.hasStarter) say(STORY.gymNoStarter);
      else if (!flags.gymBadge) say(STORY.gymIntro, () => startGymBattle());
      else say(STORY.gymDone);
      break;
  }
}
