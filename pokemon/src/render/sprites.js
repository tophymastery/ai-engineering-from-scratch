/* Tile + creature + character drawing. All sprites are simple shapes. */
import { ctx, TS, TILE, SCALE } from "../core/screen.js";
import { DIRV } from "../state.js";

export const COLORS = {
  grass: "#5bab53", grassDark: "#3f8a45", tall: "#2f6b3b", tallHi: "#3f8a45",
  path: "#d8c58c", pathEdge: "#c3ac6e", tree: "#1f5a2c", treeTrunk: "#5a3a1e",
  roof: "#b5423b", roofDark: "#8f302b", roofBlue: "#3f6fb5", roofBlueDark: "#2c4f85",
  rock: "#6d6f77", rockDark: "#4c4e55", door: "#5a3a1e", water: "#3f6fd4", waterHi: "#5b88e0",
  floor: "#c9b79a", floorLine: "#b7a382", iwall: "#6b5640",
  caveFloor: "#4a4b55", caveFloorLine: "#3a3b44",
  player: "#e23b3b", playerHi: "#ff8f6b",
};

export function drawTile(ch, sx, sy, theme) {
  ctx.fillStyle = theme === "cave" ? COLORS.caveFloor : COLORS.grass;
  ctx.fillRect(sx, sy, TS, TS);
  switch (ch) {
    case "T":
      ctx.fillStyle = COLORS.grassDark; ctx.fillRect(sx, sy, TS, TS);
      ctx.fillStyle = COLORS.treeTrunk; ctx.fillRect(sx + TS * 0.42, sy + TS * 0.55, TS * 0.16, TS * 0.4);
      ctx.fillStyle = COLORS.tree; ctx.beginPath(); ctx.arc(sx + TS / 2, sy + TS * 0.42, TS * 0.38, 0, 7); ctx.fill(); break;
    case "_":
      ctx.fillStyle = COLORS.pathEdge; ctx.fillRect(sx, sy, TS, TS);
      ctx.fillStyle = COLORS.path; ctx.fillRect(sx + 2, sy + 2, TS - 4, TS - 4); break;
    case ":":
      ctx.fillStyle = COLORS.tall; ctx.fillRect(sx, sy, TS, TS);
      ctx.fillStyle = COLORS.tallHi; for (let i = 0; i < 3; i++) ctx.fillRect(sx + 5 + i * 8, sy + TS - 11, 3, 9); break;
    case "~":
      ctx.fillStyle = COLORS.water; ctx.fillRect(sx, sy, TS, TS);
      ctx.fillStyle = COLORS.waterHi; ctx.fillRect(sx + 4, sy + 6, TS * 0.4, 2); break;
    case "#":
      ctx.fillStyle = COLORS.roof; ctx.fillRect(sx, sy, TS, TS);
      ctx.fillStyle = COLORS.roofDark; ctx.fillRect(sx, sy, TS, TS * 0.3); break;
    case "b":
      ctx.fillStyle = COLORS.roofBlue; ctx.fillRect(sx, sy, TS, TS);
      ctx.fillStyle = COLORS.roofBlueDark; ctx.fillRect(sx, sy, TS, TS * 0.3);
      ctx.fillStyle = "#fff"; ctx.fillRect(sx + TS * 0.44, sy + TS * 0.38, TS * 0.12, TS * 0.36);
      ctx.fillRect(sx + TS * 0.32, sy + TS * 0.5, TS * 0.36, TS * 0.12); break;
    case "R":
      ctx.fillStyle = COLORS.rock; ctx.fillRect(sx, sy, TS, TS);
      ctx.fillStyle = COLORS.rockDark; ctx.fillRect(sx, sy, TS, TS * 0.28); ctx.fillRect(sx, sy + TS * 0.72, TS, TS * 0.28); break;
    case "H": case "L": case "G": case "C": case "V":
      ctx.fillStyle = ch === "V" ? COLORS.rockDark : COLORS.roofDark; ctx.fillRect(sx, sy, TS, TS);
      ctx.fillStyle = ch === "V" ? "#0c0c12" : COLORS.door;
      ctx.fillRect(sx + TS * 0.24, sy + TS * 0.16, TS * 0.52, TS * 0.84);
      if (ch !== "V") { ctx.fillStyle = "#ffd966"; ctx.fillRect(sx + TS * 0.62, sy + TS * 0.55, 3, 3); } break;
    case "=":
      ctx.fillStyle = theme === "cave" ? COLORS.rock : COLORS.iwall; ctx.fillRect(sx, sy, TS, TS);
      ctx.fillStyle = theme === "cave" ? COLORS.rockDark : "#7d6549"; ctx.fillRect(sx, sy, TS, TS * 0.25); break;
    case "F":
      ctx.fillStyle = theme === "cave" ? COLORS.caveFloor : COLORS.floor; ctx.fillRect(sx, sy, TS, TS);
      ctx.strokeStyle = theme === "cave" ? COLORS.caveFloorLine : COLORS.floorLine;
      ctx.strokeRect(sx + 0.5, sy + 0.5, TS - 1, TS - 1); break;
    case "D":
      ctx.fillStyle = theme === "cave" ? COLORS.caveFloor : COLORS.floor; ctx.fillRect(sx, sy, TS, TS);
      ctx.fillStyle = COLORS.door; ctx.fillRect(sx + TS * 0.2, sy, TS * 0.6, TS); break;
    default:
      ctx.fillStyle = COLORS.grass; ctx.fillRect(sx, sy, TS, TS);
      ctx.fillStyle = COLORS.grassDark; ctx.fillRect(sx + 5, sy + 9, 2, 2);
  }
}

export function drawCreature(cr, cx, cy, r, opts = {}) {
  ctx.save();
  if (opts.alpha != null) ctx.globalAlpha = opts.alpha;
  ctx.fillStyle = opts.flash ? "#ffffff" : cr.sprite.color;
  const shape = cr.sprite.shape;
  if (shape === "flame") {
    ctx.beginPath(); ctx.moveTo(cx, cy - r);
    ctx.quadraticCurveTo(cx + r, cy, cx, cy + r);
    ctx.quadraticCurveTo(cx - r, cy, cx, cy - r); ctx.fill();
    if (!opts.flash) { ctx.fillStyle = "#ffd23f"; ctx.beginPath(); ctx.arc(cx, cy + r * 0.2, r * 0.4, 0, 7); ctx.fill(); }
  } else if (shape === "blob") {
    ctx.beginPath(); ctx.ellipse(cx, cy, r, r * 0.8, 0, 0, 7); ctx.fill();
  } else if (shape === "spike") {
    ctx.beginPath();
    for (let i = 0; i < 8; i++) {
      const ang = (i / 8) * Math.PI * 2, rad = i % 2 === 0 ? r : r * 0.6;
      ctx[i === 0 ? "moveTo" : "lineTo"](cx + Math.cos(ang) * rad, cy + Math.sin(ang) * rad);
    }
    ctx.closePath(); ctx.fill();
  } else {
    ctx.beginPath(); ctx.arc(cx, cy, r, 0, 7); ctx.fill();
  }
  if (!opts.flash) {
    ctx.fillStyle = "#101427";
    ctx.beginPath(); ctx.arc(cx - r * 0.3, cy - r * 0.1, r * 0.12, 0, 7); ctx.fill();
    ctx.beginPath(); ctx.arc(cx + r * 0.3, cy - r * 0.1, r * 0.12, 0, 7); ctx.fill();
  }
  ctx.restore();
}

export function drawPlayer(sx, sy, dir) {
  ctx.fillStyle = COLORS.player; ctx.fillRect(sx + TS * 0.22, sy + TS * 0.35, TS * 0.56, TS * 0.55);
  ctx.fillStyle = COLORS.playerHi; ctx.beginPath(); ctx.arc(sx + TS / 2, sy + TS * 0.32, TS * 0.24, 0, 7); ctx.fill();
  ctx.fillStyle = "#101427";
  const [dx, dy] = DIRV[dir];
  ctx.beginPath(); ctx.arc(sx + TS / 2 + dx * TS * 0.16, sy + TS * 0.32 + dy * TS * 0.14, 2.5, 0, 7); ctx.fill();
}

export function drawNPC(n, sx, sy) {
  ctx.fillStyle = n.color; ctx.fillRect(sx + TS * 0.22, sy + TS * 0.35, TS * 0.56, TS * 0.55);
  ctx.beginPath(); ctx.arc(sx + TS / 2, sy + TS * 0.3, TS * 0.22, 0, 7); ctx.fill();
  ctx.fillStyle = "#101427";
  ctx.beginPath(); ctx.arc(sx + TS * 0.44, sy + TS * 0.3, 2, 0, 7); ctx.fill();
  ctx.beginPath(); ctx.arc(sx + TS * 0.56, sy + TS * 0.3, 2, 0, 7); ctx.fill();
}
