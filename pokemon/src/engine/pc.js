/* Storage PC: move creatures between the active party and box storage. */
import { CONFIG } from "../data/config.js";
import { player } from "../state.js";

// Deposit a party member to the box. Keeps at least one creature in the party.
export function depositToBox(i) {
  if (player.party.length <= 1) return false;
  const c = player.party[i];
  if (!c) return false;
  player.party.splice(i, 1);
  player.box.push(c);
  return true;
}

// Withdraw a stored creature to the party (up to the party cap).
export function withdrawFromBox(i) {
  if (player.party.length >= CONFIG.party.max) return false;
  const c = player.box[i];
  if (!c) return false;
  player.box.splice(i, 1);
  player.party.push(c);
  return true;
}
