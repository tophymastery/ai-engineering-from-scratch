/* Keyboard input: held directions for walking + discrete presses routed by
 * game state (title menu, dialog, overworld, battle). */
import { STATE, game, shop } from "./state.js";
import { advanceDialog } from "./engine/dialog.js";
import { interact, tryMove } from "./engine/world.js";
import { battleInput } from "./engine/battle.js";
import { newGame, continueGame, saveGame } from "./engine/save.js";
import { buyItem } from "./engine/shop.js";
import { titleOptions } from "./render/scenes.js";

export const keys = new Set();

const KEYMAP = {
  ArrowUp: "up", KeyW: "up", ArrowDown: "down", KeyS: "down",
  ArrowLeft: "left", KeyA: "left", ArrowRight: "right", KeyD: "right",
  KeyZ: "action", Enter: "action", Space: "action",
  KeyX: "cancel", Escape: "cancel", Backspace: "cancel",
};
const DIRS = new Set(["up", "down", "left", "right"]);

export function onPress(a) {
  switch (game.state) {
    case STATE.TITLE: titleInput(a); break;
    case STATE.DIALOG: if (a === "action" || a === "cancel") advanceDialog(); break;
    case STATE.WORLD:
      if (a === "action") interact();
      else if (DIRS.has(a)) tryMove(a);   // single-step on tap (held keys also walk via the loop)
      break;
    case STATE.BATTLE: battleInput(a); break;
    case STATE.SHOP: shopInput(a); break;
    case STATE.CREDITS: break;
  }
}

function shopInput(a) {
  const n = shop.stock.length;
  if (a === "up") shop.index = (shop.index - 1 + n) % n;
  if (a === "down") shop.index = (shop.index + 1) % n;
  if (a === "action") buyItem(shop.stock[shop.index]);
  if (a === "cancel") { saveGame(); game.state = STATE.WORLD; }
}

function titleInput(a) {
  const opts = titleOptions();
  if (a === "up") game.titleIndex = (game.titleIndex - 1 + opts.length) % opts.length;
  if (a === "down") game.titleIndex = (game.titleIndex + 1) % opts.length;
  if (a === "action") {
    const choice = opts[Math.min(game.titleIndex, opts.length - 1)];
    if (choice === "Continue") continueGame(); else newGame();
  }
}

export function initInput() {
  window.addEventListener("keydown", (e) => {
    const a = KEYMAP[e.code]; if (!a) return; e.preventDefault();
    if (DIRS.has(a)) keys.add(a);
    if (!e.repeat) onPress(a);
  });
  window.addEventListener("keyup", (e) => {
    const a = KEYMAP[e.code]; if (a && DIRS.has(a)) keys.delete(a);
  });
}
