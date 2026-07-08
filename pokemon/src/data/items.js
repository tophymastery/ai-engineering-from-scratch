/* Item dex + starting bag (config). Fully data-driven and easy to extend.
 *   kind:
 *     "heal"   restore HP (value = amount, 9999 = full)
 *     "revive" revive a fainted creature (value = fraction of max HP)
 *     "status" cure status conditions (value ignored)
 *     "ball"   catch a wild creature (ballBonus scales catch chance)
 *   price : shop cost (buying); items are sold in the Mart
 *   usableInBattle / usableInField : where the item can be used */
export const ITEMS = {
  potion:      { id: "potion",      name: "Potion",       kind: "heal",   value: 20,   price: 300,  desc: "Restores 20 HP.",  usableInBattle: true,  usableInField: true },
  superpotion: { id: "superpotion", name: "Super Potion", kind: "heal",   value: 50,   price: 700,  desc: "Restores 50 HP.",  usableInBattle: true,  usableInField: true },
  hyperpotion: { id: "hyperpotion", name: "Hyper Potion", kind: "heal",   value: 200,  price: 1200, desc: "Restores 200 HP.", usableInBattle: true,  usableInField: true },
  fullheal:    { id: "fullheal",    name: "Full Heal",    kind: "status", value: 0,    price: 600,  desc: "Cures any status.", usableInBattle: true, usableInField: true },
  revive:      { id: "revive",      name: "Revive",       kind: "revive", value: 0.5,  price: 1500, desc: "Revives a fainted creature.", usableInBattle: true, usableInField: true },
  snarebell:   { id: "snarebell",   name: "Snarebell",    kind: "ball",   ballBonus: 1.0, price: 200, desc: "A device for catching wild Shapemon.", usableInBattle: true, usableInField: false },
  greatbell:   { id: "greatbell",   name: "Great Bell",   kind: "ball",   ballBonus: 1.5, price: 600, desc: "A better catching device.",           usableInBattle: true, usableInField: false },
};

// What the player starts with (item id + quantity).
export const STARTING_BAG = [
  { item: "potion", qty: 3 },
  { item: "snarebell", qty: 5 },
];

// What the Mart sells.
export const SHOP_STOCK = ["potion", "superpotion", "fullheal", "revive", "snarebell", "greatbell"];
