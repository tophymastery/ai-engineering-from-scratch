/* Mutable game state, shared across engine + render modules. */
import { CONFIG } from "./data/config.js";

export const STATE = {
  TITLE: "title", WORLD: "world", DIALOG: "dialog", BATTLE: "battle", SHOP: "shop", CREDITS: "credits",
};

export const DIRV = { up: [0, -1], down: [0, 1], left: [-1, 0], right: [1, 0] };

const T = CONFIG.grid.tile;

// badges[i] === true once the i-th gym is cleared.
export const flags = { hasStarter: false, badges: [] };

export const player = {
  map: "home", x: 5, y: 3, dir: "down",
  px: 5 * T, py: 3 * T,
  moving: false, from: null, to: null, progress: 0,
  party: [], bag: [], box: [], money: 0,
};

export const shop = { stock: [], index: 0 };

export const game = {
  state: STATE.TITLE, afterDialog: null, tick: 0,
  forceEncounter: false, noEncounter: false, titleIndex: 0,
};

export const dialog = { lines: [], index: 0, active: false, speaker: null };

export const battle = {
  kind: "wild", enemy: null, ally: null, enemyParty: [], enemyIdx: 0,
  phase: "intro", cmd: 0, moveIndex: 0, bagIndex: 0, partyIndex: 0,
  mustSwitch: false, foeName: null, canRun: true,
  msg: [], fx: [], msgIndex: 0, afterMsg: null, onWin: null,
  anim: null,
};
