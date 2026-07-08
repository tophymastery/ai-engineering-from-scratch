/* Canvas handle and derived screen geometry. Browser-only module. */
import { CONFIG } from "../data/config.js";

export const canvas = document.getElementById("game");
export const ctx = canvas.getContext("2d");
export const W = canvas.width;
export const H = canvas.height;

export const TILE = CONFIG.grid.tile;
export const SCALE = CONFIG.grid.scale;
export const TS = TILE * SCALE;                 // on-screen tile size

export const SIDEBAR = CONFIG.screen.sidebar;   // overworld right panel width
export const MAP_VIEW_W = W - SIDEBAR;          // pixels available for the map
export const MAP_VIEW_H = H;
export const VIEW_COLS = Math.floor(MAP_VIEW_W / TS);
export const VIEW_ROWS = Math.floor(MAP_VIEW_H / TS);
