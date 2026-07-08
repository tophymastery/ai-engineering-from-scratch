/* Factories that turn dex data into live battle instances. Pure module. */
import { SPECIES } from "../data/species.js";
import { MOVES } from "../data/moves.js";
import { recomputeStats, expForLevel } from "./stats.js";

export const makeMove = (id) => {
  const m = MOVES[id];
  return { ...m, pp: m.pp, maxpp: m.pp };
};

export function makeCreature(id, level) {
  const s = SPECIES[id];
  if (!s) throw new Error(`Unknown species: ${id}`);
  const cr = {
    speciesId: id, name: s.name, type: s.type,
    sprite: s.sprite, base: s.base, expYield: s.expYield,
    level, exp: expForLevel(level),
    moves: s.moves.map(makeMove),
  };
  recomputeStats(cr, false);
  cr.hp = cr.maxhp;
  return cr;
}

// Build a party (array of creatures) from a [{species, level}] list.
export const makeParty = (list) => list.map((e) => makeCreature(e.species, e.level));
