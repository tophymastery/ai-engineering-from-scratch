/* Overworld renderer: scrolling map viewport, player, NPCs, badges sidebar,
 * and the dialog box. */
import { ctx, W, H, TS, TILE, SCALE, MAP_VIEW_W, MAP_VIEW_H, VIEW_COLS, VIEW_ROWS } from "../core/screen.js";
import { MAPS, NPCS } from "../data/maps.js";
import { player, dialog } from "../state.js";
import { drawTile, drawPlayer, drawNPC } from "./sprites.js";
import { drawBox, drawSidebar, wrap } from "./hud.js";

function camera(map) {
  const pw = map.w * TS, ph = map.h * TS;
  let cx = player.px * SCALE + TS / 2 - MAP_VIEW_W / 2;
  let cy = player.py * SCALE + TS / 2 - MAP_VIEW_H / 2;
  cx = pw <= MAP_VIEW_W ? (pw - MAP_VIEW_W) / 2 : Math.max(0, Math.min(cx, pw - MAP_VIEW_W));
  cy = ph <= MAP_VIEW_H ? (ph - MAP_VIEW_H) / 2 : Math.max(0, Math.min(cy, ph - MAP_VIEW_H));
  return { cx, cy };
}

export function renderWorld() {
  const map = MAPS[player.map];
  const { cx, cy } = camera(map);

  // clip map drawing to the left viewport (sidebar sits to the right)
  ctx.save();
  ctx.beginPath(); ctx.rect(0, 0, MAP_VIEW_W, MAP_VIEW_H); ctx.clip();
  ctx.fillStyle = map.theme === "cave" ? "#1a1b24" : "#000"; ctx.fillRect(0, 0, MAP_VIEW_W, MAP_VIEW_H);

  const x0 = Math.floor(cx / TS), y0 = Math.floor(cy / TS);
  for (let ty = y0; ty <= y0 + VIEW_ROWS + 1; ty++)
    for (let tx = x0; tx <= x0 + VIEW_COLS + 1; tx++) {
      if (tx < 0 || ty < 0 || tx >= map.w || ty >= map.h) continue;
      drawTile(map.grid[ty][tx], tx * TS - cx, ty * TS - cy, map.theme);
    }
  for (const n of (NPCS[player.map] || [])) {
    if (n.role === "trainer" && n.defeated) { /* still show, greyed */ }
    drawNPC(n, n.x * TS - cx, n.y * TS - cy);
  }
  drawPlayer(player.px * SCALE - cx, player.py * SCALE - cy, player.dir);
  ctx.restore();

  drawSidebar();

  if (dialog.active) renderDialogBox(dialog.lines[dialog.index] || "");
}

function renderDialogBox(text) {
  const bx = 12, bh = 96, by = H - bh - 12, bw = MAP_VIEW_W - 24;
  drawBox(bx, by, bw, bh);
  ctx.fillStyle = "#20222f"; ctx.font = "17px 'Courier New', monospace"; ctx.textBaseline = "top";
  wrap(text, bw - 40).slice(0, 3).forEach((ln, i) => ctx.fillText(ln, bx + 20, by + 16 + i * 24));
  ctx.fillStyle = "#3553ff"; ctx.font = "13px 'Courier New', monospace";
  ctx.fillText("v Z/Enter", bx + bw - 110, by + bh - 24);
}
