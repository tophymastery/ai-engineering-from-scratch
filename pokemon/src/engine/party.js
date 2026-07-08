/* Party helpers. */
import { player } from "../state.js";

export function healParty() {
  for (const c of player.party) {
    c.hp = c.maxhp;
    for (const m of c.moves) m.pp = m.maxpp;
  }
}

export const firstHealthy = () => player.party.find((c) => c.hp > 0) || null;
export const partyWiped = () => player.party.every((c) => c.hp <= 0);
