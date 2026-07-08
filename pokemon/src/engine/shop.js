/* Mart purchasing. */
import { player } from "../state.js";
import { ITEMS } from "../data/items.js";
import { addItem } from "./bag.js";

export function canAfford(id) { return player.money >= ITEMS[id].price; }
export function buyItem(id) {
  if (!canAfford(id)) return false;
  player.money -= ITEMS[id].price;
  addItem(id, 1);
  return true;
}
