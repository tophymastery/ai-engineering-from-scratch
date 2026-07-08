/* Bag / inventory + money helpers. */
import { player } from "../state.js";
import { ITEMS } from "../data/items.js";

export function addItem(id, qty = 1) {
  const e = player.bag.find((b) => b.item === id);
  if (e) e.qty += qty; else player.bag.push({ item: id, qty });
}
export function removeItem(id, qty = 1) {
  const e = player.bag.find((b) => b.item === id);
  if (!e) return false;
  e.qty -= qty;
  if (e.qty <= 0) player.bag = player.bag.filter((b) => b !== e);
  return true;
}
export const itemQty = (id) => { const e = player.bag.find((b) => b.item === id); return e ? e.qty : 0; };
export const usableInBattle = () => player.bag.filter((b) => b.qty > 0 && ITEMS[b.item].usableInBattle);

/* Apply a heal/status/revive item to a creature. Returns a result message or
 * null if the item can't be used on that target right now. */
export function applyItem(id, cr) {
  const it = ITEMS[id];
  if (it.kind === "heal") {
    if (cr.hp <= 0 || cr.hp >= cr.maxhp) return null;
    const before = cr.hp;
    cr.hp = Math.min(cr.maxhp, cr.hp + it.value);
    return `${cr.name} recovered ${cr.hp - before} HP!`;
  }
  if (it.kind === "status") {
    if (cr.status === "none") return null;
    cr.status = "none"; cr.sleepTurns = 0;
    return `${cr.name} is cured of its status!`;
  }
  if (it.kind === "revive") {
    if (cr.hp > 0) return null;
    cr.hp = Math.max(1, Math.floor(cr.maxhp * it.value)); cr.status = "none";
    return `${cr.name} was revived!`;
  }
  return null;
}
