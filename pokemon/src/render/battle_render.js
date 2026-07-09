/* Battle renderer with animation: sprite lunge, hit-flash, HP-bar drain, faint.
 * GBA-style framed layout: foe HP box top-left, your HP box (with EXP bar)
 * bottom-right, grassy platforms, 2x2 command menu, 2x2 move panel + PP/TYPE. */
import { ctx, W, H } from "../core/screen.js";
import { battle, player } from "../state.js";
import { TYPE_COLORS } from "../data/types.js";
import { ITEMS } from "../data/items.js";
import { expForLevel } from "../engine/stats.js";
import { usableInBattle } from "../engine/bag.js";
import { drawCreature } from "./sprites.js";
import { drawBox, drawHPBox, roundRect, wrap } from "./hud.js";

function spriteState(who) {
  const a = battle.anim;
  return {
    lunge: a.lunge.who === who ? a.lunge.t : 0,
    flash: a.flash.who === who ? a.flash.t > 0 : false,
    faint: a.faint.who === who ? a.faint.t : 0,
  };
}

function platform(cx, cy, rw) {
  ctx.fillStyle = "rgba(0,0,0,0.10)"; ctx.beginPath(); ctx.ellipse(cx, cy + 6, rw, rw * 0.26, 0, 0, 7); ctx.fill();
  ctx.fillStyle = "#8fcf7e"; ctx.beginPath(); ctx.ellipse(cx, cy, rw, rw * 0.28, 0, 0, 7); ctx.fill();
  ctx.fillStyle = "#7bbf6a"; ctx.beginPath(); ctx.ellipse(cx, cy + 2, rw * 0.82, rw * 0.2, 0, 0, 7); ctx.fill();
}

const expFrac = (cr) => {
  const lo = expForLevel(cr.level), hi = expForLevel(cr.level + 1);
  return Math.max(0, Math.min(1, (cr.exp - lo) / Math.max(1, hi - lo)));
};

export function renderBattle() {
  const a = battle.anim;

  // background: sky band + ground band
  ctx.fillStyle = "#c7e8ff"; ctx.fillRect(0, 0, W, H);
  const grd = ctx.createLinearGradient(0, H * 0.42, 0, H);
  grd.addColorStop(0, "#a9dc9a"); grd.addColorStop(1, "#8ccf86");
  ctx.fillStyle = grd; ctx.fillRect(0, H * 0.42, W, H * 0.58);
  ctx.fillStyle = "rgba(255,255,255,0.18)"; ctx.fillRect(0, H * 0.42, W, 3);

  // foe (top-right sprite on platform, top-left HP box)
  const ex = W - 150, ey = 128;
  platform(ex, ey + 44, 66);
  if (a) {
    const es = spriteState("enemy");
    const lx = es.lunge ? -es.lunge : 0, alpha = es.faint ? Math.max(0, es.faint / 30) : 1, dy = es.faint ? (30 - es.faint) : 0;
    drawCreature(battle.enemy, ex + lx, ey + dy, 38, { flash: es.flash, alpha });
    drawHPBox(24, 24, battle.enemy, a.hpEnemy, {});
  }

  // ally (bottom-left sprite on platform, bottom-right HP box + EXP)
  const ax = 130, ay = 250;
  platform(ax, ay + 48, 80);
  if (a) {
    const as = spriteState("ally");
    const lx = as.lunge ? as.lunge : 0, alpha = as.faint ? Math.max(0, as.faint / 30) : 1, dy = as.faint ? (30 - as.faint) : 0;
    drawCreature(battle.ally, ax + lx, ay + dy, 46, { flash: as.flash, alpha });
    drawHPBox(W - 248, 196, battle.ally, a.hpAlly, { num: true, exp: expFrac(battle.ally) });
  }

  // bottom command / text box
  const bx = 12, bh = 132, by = H - bh - 12, bw = W - 24;
  drawBox(bx, by, bw, bh, 10);
  ctx.fillStyle = "#20222f"; ctx.textBaseline = "top"; ctx.font = "18px 'Courier New', monospace";
  const cursor = (on) => (on ? "▶ " : "  ");

  if (battle.phase === "anim" || battle.phase === "intro") {
    const line = battle.msg[Math.min(battle.msgIndex, battle.msg.length - 1)] || "";
    wrap(line, bw - 60).slice(0, 3).forEach((ln, i) => ctx.fillText(ln, bx + 22, by + 24 + i * 26));
    ctx.fillStyle = "#3553ff"; ctx.font = "14px 'Courier New', monospace"; ctx.fillText("▼ Z", bx + bw - 56, by + bh - 28);
  } else if (battle.phase === "menu") {
    ctx.fillText(`What will`, bx + 24, by + 26);
    ctx.fillText(`${battle.ally.name.toUpperCase()} do?`, bx + 24, by + 54);
    const cells = ["FIGHT", "PACK", "PKMN", "RUN"];
    const gx = bx + bw - 246, gy = by + 26, cw = 118, ch = 44;
    drawBox(gx - 12, by + 12, 252, bh - 24, 8);
    cells.forEach((c, i) => {
      const col = i % 2, row = i >> 1, px = gx + col * cw, py = gy + row * ch;
      ctx.fillStyle = i === battle.cmd ? "#d63c3c" : "#20222f";
      ctx.fillText(cursor(i === battle.cmd) + c, px, py);
    });
  } else if (battle.phase === "moves") {
    // left: 2x2 move grid ; right: PP + TYPE panel
    const moves = battle.ally.moves, gx = bx + 24, gy = by + 22, cw = 190, ch = 44;
    for (let i = 0; i < 4; i++) {
      const col = i % 2, row = i >> 1, px = gx + col * cw, py = gy + row * ch;
      const m = moves[i];
      ctx.fillStyle = i === battle.moveIndex ? "#d63c3c" : (m ? "#20222f" : "#b8b8c0");
      ctx.font = "17px 'Courier New', monospace";
      ctx.fillText(cursor(i === battle.moveIndex) + (m ? m.name : "-"), px, py);
    }
    const sel = moves[battle.moveIndex];
    const px = bx + bw - 224;
    drawBox(px, by + 12, 210, bh - 24, 8);
    ctx.fillStyle = "#20222f"; ctx.font = "16px 'Courier New', monospace";
    ctx.textAlign = "right"; ctx.fillText(`${sel.pp}/ ${sel.maxpp}`, px + 190, by + 34); ctx.textAlign = "left";
    ctx.fillText("PP", px + 20, by + 34);
    ctx.fillStyle = TYPE_COLORS[sel.type] || "#20222f";
    roundRect(px + 20, by + 66, 170, 24, 6); ctx.fill();
    ctx.fillStyle = "#fff"; ctx.font = "bold 15px 'Courier New', monospace"; ctx.fillText("TYPE/" + sel.type.toUpperCase(), px + 30, by + 71);
  } else if (battle.phase === "bag") {
    const items = usableInBattle();
    ctx.font = "16px 'Courier New', monospace";
    if (!items.length) ctx.fillText("Your bag is empty.", bx + 24, by + 22);
    items.slice(0, 4).forEach((b, i) => {
      ctx.fillStyle = i === battle.bagIndex ? "#d63c3c" : "#20222f";
      ctx.fillText(cursor(i === battle.bagIndex) + ITEMS[b.item].name + " x" + b.qty, bx + 24, by + 20 + i * 26);
    });
    ctx.fillStyle = "#3553ff"; ctx.font = "14px 'Courier New', monospace"; ctx.fillText("X=back", bx + bw - 100, by + bh - 28);
  } else if (battle.phase === "party") {
    ctx.font = "15px 'Courier New', monospace";
    ctx.fillText(battle.mustSwitch ? "Choose the next Shapemon:" : "Switch to:", bx + 24, by + 12);
    player.party.slice(0, 6).forEach((c, i) => {
      const active = c === battle.ally;
      ctx.fillStyle = i === battle.partyIndex ? "#d63c3c" : (c.hp > 0 ? "#20222f" : "#a0a0a8");
      const tag = active ? " (out)" : c.hp <= 0 ? " (FNT)" : "";
      ctx.fillText(cursor(i === battle.partyIndex) + `${c.name} Lv${c.level} ${c.hp}/${c.maxhp}${tag}`, bx + 24, by + 36 + i * 15);
    });
    if (!battle.mustSwitch) { ctx.fillStyle = "#3553ff"; ctx.font = "14px 'Courier New', monospace"; ctx.fillText("X=back", bx + bw - 100, by + bh - 26); }
  }
}
