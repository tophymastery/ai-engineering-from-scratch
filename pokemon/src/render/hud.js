/* Reusable UI chrome: text boxes, HP boxes, and the overworld badges sidebar. */
import { ctx, W, H, SIDEBAR, MAP_VIEW_W } from "../core/screen.js";
import { CONFIG } from "../data/config.js";
import { player, flags } from "../state.js";
import { objective } from "../engine/objective.js";

const INK = "#20222f", PAPER = "#f7f7ef";

export function roundRect(x, y, w, h, r) {
  const rr = Math.min(r, w / 2, h / 2);
  ctx.beginPath();
  ctx.moveTo(x + rr, y);
  ctx.arcTo(x + w, y, x + w, y + h, rr);
  ctx.arcTo(x + w, y + h, x, y + h, rr);
  ctx.arcTo(x, y + h, x, y, rr);
  ctx.arcTo(x, y, x + w, y, rr);
  ctx.closePath();
}

export function drawBox(x, y, w, h, r = 8) {
  roundRect(x, y, w, h, r); ctx.fillStyle = PAPER; ctx.fill();
  ctx.lineWidth = 3; ctx.strokeStyle = INK; ctx.stroke();
  roundRect(x + 4, y + 4, w - 8, h - 8, Math.max(2, r - 4));
  ctx.lineWidth = 1; ctx.strokeStyle = "rgba(32,34,47,0.25)"; ctx.stroke();
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

function bar(x, y, w, h, frac, color, track) {
  roundRect(x, y, w, h, h / 2); ctx.fillStyle = track || "#2b2d3a"; ctx.fill();
  const fw = Math.max(0, Math.min(1, frac)) * w;
  if (fw > 1) { roundRect(x, y, Math.max(h, fw), h, h / 2); ctx.save(); ctx.clip(); ctx.fillStyle = color; ctx.fillRect(x, y, fw, h); ctx.restore(); }
}

// Framed HP box (GBA-style). opts: { num: show HP numbers, exp: 0..1 EXP fraction }
export function drawHPBox(x, y, cr, shownHp, opts = {}) {
  const showNum = opts.num, showExp = opts.exp != null;
  const bw = 224, bh = 44 + (showNum ? 16 : 0) + (showExp ? 20 : 0);
  drawBox(x, y, bw, bh, 10);
  ctx.fillStyle = INK; ctx.textBaseline = "top";
  ctx.font = "bold 15px 'Courier New', monospace"; ctx.fillText(cr.name.toUpperCase(), x + 14, y + 9);
  ctx.font = "14px 'Courier New', monospace"; ctx.fillText("Lv" + cr.level, x + bw - 46, y + 10);

  // "HP" pill + rounded bar
  const barY = y + 30, pillX = x + 14;
  roundRect(pillX, barY - 1, 26, 13, 6); ctx.fillStyle = "#e6b800"; ctx.fill();
  ctx.fillStyle = "#20222f"; ctx.font = "italic bold 11px 'Courier New', monospace"; ctx.fillText("HP", pillX + 5, barY + 1);
  const barX = pillX + 32, barW = bw - (barX - x) - 14;
  bar(barX, barY, barW, 9, shownHp / cr.maxhp, hpColor(shownHp / cr.maxhp));

  if (cr.status && cr.status !== "none") {
    ctx.fillStyle = STATUS_COLOR[cr.status]; roundRect(x + bw - 52, y + 8, 40, 15, 4); ctx.fill();
    ctx.fillStyle = "#fff"; ctx.font = "bold 11px 'Courier New', monospace"; ctx.fillText(STATUS_TAG[cr.status], x + bw - 47, y + 10);
  }
  let yy = barY + 14;
  if (showNum) {
    ctx.fillStyle = INK; ctx.textAlign = "right"; ctx.font = "13px 'Courier New', monospace";
    ctx.fillText(`${Math.round(shownHp)}/ ${cr.maxhp}`, x + bw - 14, yy); ctx.textAlign = "left"; yy += 16;
  }
  if (showExp) {
    ctx.fillStyle = INK; ctx.font = "italic bold 10px 'Courier New', monospace"; ctx.fillText("EXP", x + 14, yy + 1);
    bar(x + 44, yy + 2, bw - 58, 5, opts.exp, "#3f8fe0", "#2b2d3a");
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

  // GOAL: the current guided objective, wrapped at the panel bottom.
  const gy0 = H - 92;
  ctx.fillStyle = "#8fb6ff"; ctx.font = "bold 13px 'Courier New', monospace"; ctx.fillText("GOAL", x + SIDEBAR / 2, gy0);
  ctx.fillStyle = "#e9e9f2"; ctx.font = "11px 'Courier New', monospace"; ctx.textAlign = "center";
  wrap(objective(), SIDEBAR - 16).slice(0, 4).forEach((ln, i) => ctx.fillText(ln, x + SIDEBAR / 2, gy0 + 20 + i * 15));
  ctx.textAlign = "left";
}
