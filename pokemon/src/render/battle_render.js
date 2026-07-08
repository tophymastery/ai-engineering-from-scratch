/* Battle renderer with animation: sprite lunge, hit-flash, HP-bar drain, faint.
 * Classic layout: foe HP box top-left, your HP box bottom-right, 2x2 command
 * menu (FIGHT / PACK / PKMN / RUN). */
import { ctx, W, H } from "../core/screen.js";
import { battle } from "../state.js";
import { TYPE_COLORS } from "../data/types.js";
import { drawCreature } from "./sprites.js";
import { drawBox, drawHPBox, wrap } from "./hud.js";

function spriteState(who) {
  const a = battle.anim;
  const lunge = a.lunge.who === who ? a.lunge.t : 0;
  const flash = a.flash.who === who ? a.flash.t > 0 : false;
  const faint = a.faint.who === who ? a.faint.t : 0;
  return { lunge, flash, faint };
}

export function renderBattle() {
  const a = battle.anim;
  const g = ctx.createLinearGradient(0, 0, 0, H);
  g.addColorStop(0, "#cfe8ff"); g.addColorStop(1, "#9fd0a8");
  ctx.fillStyle = g; ctx.fillRect(0, 0, W, H);

  // foe (top-right sprite, top-left HP box)
  const ex = W - 150, ey = 120;
  const es = spriteState("enemy");
  ctx.fillStyle = "rgba(0,0,0,0.12)"; ctx.beginPath(); ctx.ellipse(ex, ey + 44, 60, 16, 0, 0, 7); ctx.fill();
  if (a) {
    const lx = es.lunge ? -es.lunge : 0;                       // lunge toward player (down-left)
    const alpha = es.faint ? Math.max(0, es.faint / 30) : 1;
    const dy = es.faint ? (30 - es.faint) : 0;
    drawCreature(battle.enemy, ex + lx, ey + dy, 38, { flash: es.flash, alpha });
    drawHPBox(24, 26, battle.enemy, a.hpEnemy, false);
  }

  // ally (bottom-left sprite, bottom-right HP box)
  const ax = 130, ay = 250;
  const as = spriteState("ally");
  ctx.fillStyle = "rgba(0,0,0,0.12)"; ctx.beginPath(); ctx.ellipse(ax, ay + 48, 70, 18, 0, 0, 7); ctx.fill();
  if (a) {
    const lx = as.lunge ? as.lunge : 0;
    const alpha = as.faint ? Math.max(0, as.faint / 30) : 1;
    const dy = as.faint ? (30 - as.faint) : 0;
    drawCreature(battle.ally, ax + lx, ay + dy, 46, { flash: as.flash, alpha });
    drawHPBox(W - 240, 206, battle.ally, a.hpAlly, true);
  }

  // bottom command / text box
  const bx = 12, bh = 132, by = H - bh - 12, bw = W - 24;
  drawBox(bx, by, bw, bh);
  ctx.fillStyle = "#20222f"; ctx.textBaseline = "top"; ctx.font = "18px 'Courier New', monospace";

  if (battle.phase === "anim" || battle.phase === "intro") {
    const line = battle.msg[Math.min(battle.msgIndex, battle.msg.length - 1)] || "";
    wrap(line, bw - 60).slice(0, 3).forEach((ln, i) => ctx.fillText(ln, bx + 20, by + 22 + i * 26));
    ctx.fillStyle = "#3553ff"; ctx.font = "14px 'Courier New', monospace"; ctx.fillText("v Z", bx + bw - 56, by + bh - 26);
  } else if (battle.phase === "menu") {
    ctx.fillText(`What will ${battle.ally.name} do?`, bx + 20, by + 22);
    const cells = ["FIGHT", "PACK", "PKMN", "RUN"];
    const gx = bx + bw - 250, gy = by + 22;
    cells.forEach((c, i) => {
      const col = i % 2, row = i >> 1;
      ctx.fillStyle = i === battle.cmd ? "#d63c3c" : "#20222f";
      ctx.fillText((i === battle.cmd ? "> " : "  ") + c, gx + col * 130, gy + row * 42);
    });
  } else if (battle.phase === "moves") {
    battle.ally.moves.forEach((m, i) => {
      ctx.fillStyle = i === battle.moveIndex ? "#d63c3c" : "#20222f";
      ctx.fillText((i === battle.moveIndex ? "> " : "  ") + m.name, bx + 24, by + 20 + i * 28);
    });
    const sel = battle.ally.moves[battle.moveIndex];
    ctx.fillStyle = "#20222f"; ctx.font = "15px 'Courier New', monospace";
    ctx.fillText(`PP ${sel.pp}/${sel.maxpp}`, bx + bw - 210, by + 24);
    ctx.fillStyle = TYPE_COLORS[sel.type] || "#20222f";
    ctx.fillText("TYPE/" + sel.type.toUpperCase(), bx + bw - 210, by + 52);
    ctx.fillStyle = "#3553ff"; ctx.font = "14px 'Courier New', monospace"; ctx.fillText("X=back", bx + bw - 100, by + bh - 26);
  } else if (battle.phase === "party") {
    const c = battle.ally;
    ctx.fillText(`${c.name}  Lv${c.level}`, bx + 20, by + 16);
    ctx.font = "15px 'Courier New', monospace";
    ctx.fillText(`HP ${c.hp}/${c.maxhp}   TYPE ${c.type.toUpperCase()}`, bx + 20, by + 46);
    ctx.fillText(`ATK ${c.atk}  DEF ${c.def}  SP.ATK ${c.spAtk}`, bx + 20, by + 72);
    ctx.fillText(`SP.DEF ${c.spDef}  SPD ${c.speed}`, bx + 20, by + 96);
    ctx.fillStyle = "#3553ff"; ctx.font = "14px 'Courier New', monospace"; ctx.fillText("X=back", bx + bw - 100, by + bh - 26);
  }
}
