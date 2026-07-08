/* Reusable UI chrome: text boxes, HP boxes, and the overworld badges sidebar. */
import { ctx, W, H, SIDEBAR, MAP_VIEW_W } from "../core/screen.js";
import { CONFIG } from "../data/config.js";
import { player, flags } from "../state.js";

const INK = "#20222f", PAPER = "#f7f7ef";

export function drawBox(x, y, w, h) {
  ctx.fillStyle = PAPER; ctx.fillRect(x, y, w, h);
  ctx.strokeStyle = INK; ctx.lineWidth = 3; ctx.strokeRect(x + 2, y + 2, w - 4, h - 4);
}

export function wrap(text, maxW) {
  const words = text.split(" "), lines = []; let cur = "";
  for (const w of words) {
    const t = cur ? cur + " " + w : w;
    if (ctx.measureText(t).width > maxW && cur) { lines.push(cur); cur = w; } else cur = t;
  }
  if (cur) lines.push(cur); return lines;
}

export function hpColor(frac) {
  return frac > 0.5 ? "#57c34a" : frac > 0.2 ? "#e6c327" : "#d63c3c";
}

const STATUS_TAG = { burn: "BRN", poison: "PSN", paralysis: "PAR", sleep: "SLP" };
const STATUS_COLOR = { burn: "#e2531f", poison: "#a45bd0", paralysis: "#e6c327", sleep: "#7a8090" };

// shownHp animates; realMax used for the bar scale + optional numbers.
export function drawHPBox(x, y, cr, shownHp, showNum) {
  const bw = 216, bh = showNum ? 66 : 52;
  drawBox(x, y, bw, bh);
  ctx.fillStyle = INK; ctx.textBaseline = "top";
  ctx.font = "16px 'Courier New', monospace"; ctx.fillText(cr.name, x + 12, y + 8);
  ctx.font = "14px 'Courier New', monospace"; ctx.fillText("Lv" + cr.level, x + bw - 46, y + 9);
  if (cr.status && cr.status !== "none") {
    ctx.fillStyle = STATUS_COLOR[cr.status]; ctx.fillRect(x + 12, y + 26, 34, 15);
    ctx.fillStyle = "#fff"; ctx.font = "11px 'Courier New', monospace"; ctx.fillText(STATUS_TAG[cr.status], x + 15, y + 28);
  }
  ctx.fillStyle = INK; ctx.font = "13px 'Courier New', monospace"; ctx.fillText("HP", x + 54, y + 30);
  const barX = x + 80, barW = 120, barY = y + 32;
  ctx.fillStyle = "#20222f"; ctx.fillRect(barX - 1, barY - 1, barW + 2, 10);
  const frac = Math.max(0, shownHp / cr.maxhp);
  ctx.fillStyle = hpColor(frac); ctx.fillRect(barX, barY, barW * frac, 8);
  if (showNum) {
    ctx.fillStyle = INK; ctx.font = "13px 'Courier New', monospace";
    ctx.fillText(`${Math.round(shownHp)}/${cr.maxhp}`, x + bw - 86, y + 46);
  }
}

// Right-hand overworld panel: badges + lead creature summary.
export function drawSidebar() {
  const x = MAP_VIEW_W;
  ctx.fillStyle = "#20222f"; ctx.fillRect(x, 0, SIDEBAR, H);
  ctx.fillStyle = "#f4f4ec"; ctx.textAlign = "center"; ctx.textBaseline = "top";
  ctx.font = "bold 18px 'Courier New', monospace"; ctx.fillText("BADGES", x + SIDEBAR / 2, 14);

  const cols = 2, size = 40, gap = 12, gx = x + (SIDEBAR - (cols * size + (cols - 1) * gap)) / 2;
  for (let i = 0; i < CONFIG.badges.total; i++) {
    const c = i % cols, r = (i / cols) | 0;
    const bx = gx + c * (size + gap), by = 40 + r * (size + gap);
    const earned = !!flags.badges[i];
    ctx.fillStyle = earned ? "#57c34a" : "#3a3c50";
    ctx.beginPath(); ctx.arc(bx + size / 2, by + size / 2, size / 2, 0, 7); ctx.fill();
    ctx.strokeStyle = "#0d0e16"; ctx.lineWidth = 2; ctx.stroke();
    if (earned) { ctx.fillStyle = "#0d0e16"; ctx.font = "18px 'Courier New', monospace"; ctx.textBaseline = "middle"; ctx.fillText("*", bx + size / 2, by + size / 2 + 2); ctx.textBaseline = "top"; }
  }

  let py = 40 + Math.ceil(CONFIG.badges.total / cols) * (size + gap) + 10;
  ctx.fillStyle = "#ffd23f"; ctx.font = "13px 'Courier New', monospace"; ctx.fillText(`$ ${player.money}`, x + SIDEBAR / 2, py);
  py += 22;
  ctx.fillStyle = "#f4f4ec"; ctx.font = "bold 13px 'Courier New', monospace"; ctx.fillText(`PARTY (${player.party.length})`, x + SIDEBAR / 2, py);
  ctx.font = "12px 'Courier New', monospace";
  const shown = player.party.slice(0, 3);
  if (shown.length) {
    shown.forEach((c, i) => {
      ctx.fillStyle = c.hp > 0 ? "#f4f4ec" : "#8a8c9c";
      ctx.fillText(`${c.name} Lv${c.level}`, x + SIDEBAR / 2, py + 20 + i * 16);
    });
  } else {
    ctx.fillStyle = "#8a8c9c"; ctx.fillText("(empty)", x + SIDEBAR / 2, py + 20);
  }
  ctx.textAlign = "left";
}
