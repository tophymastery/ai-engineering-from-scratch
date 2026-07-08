/* Battle engine — authentic FireRed sequence & layout, with animation.
 * Supports wild, trainer, and gym battles (multi-creature enemy parties).
 * The item system is intentionally omitted (PACK reports no items). */
import { CONFIG } from "../data/config.js";
import { STATE, game, player, flags, battle } from "../state.js";
import { calcDamage } from "./damage.js";
import { gainExp, expYield } from "./stats.js";
import { makeCreature, makeMove } from "./creature.js";
import { ENCOUNTERS } from "../data/encounters.js";
import { healParty } from "./party.js";
import { doWarp } from "./world.js";
import { rint, rand, rrange } from "../core/rng.js";

const A = CONFIG.animation;

function initAnim() {
  const ally = battle.ally, enemy = battle.enemy;
  battle.anim = {
    hpAlly: ally.hp, tgtAlly: ally.hp, hpEnemy: enemy.hp, tgtEnemy: enemy.hp,
    lunge: { who: null, t: 0 }, flash: { who: null, t: 0 }, faint: { who: null, t: 0 },
    lastMsg: -1,
  };
}

function battleSay(lines, after, fx) {
  battle.msg = Array.isArray(lines) ? lines : [lines];
  battle.fx = fx || [];
  battle.msgIndex = 0; battle.afterMsg = after || null; battle.phase = "anim";
  if (battle.anim) battle.anim.lastMsg = -1;
}

function enterBattle(kind, enemyParty, opts = {}) {
  battle.kind = kind;
  battle.enemyParty = enemyParty;
  battle.enemyIdx = 0;
  battle.enemy = enemyParty[0];
  battle.ally = player.party.find((c) => c.hp > 0) || player.party[0];
  battle.onWin = opts.onWin || null;
  battle.canRun = kind === "wild";
  battle.foeName = opts.foeName || null;
  battle.phase = "intro"; battle.cmd = 0; battle.moveIndex = 0;
  game.state = STATE.BATTLE;
  initAnim();
}

// ---- Entry points ----------------------------------------------------------
function weightedPick(table) {
  const total = table.reduce((s, e) => s + (e.weight || 1), 0);
  let r = rand() * total;
  for (const e of table) { r -= (e.weight || 1); if (r <= 0) return e; }
  return table[table.length - 1];
}

export function startWildBattle(area) {
  const pick = weightedPick(area.table);
  const enemy = makeCreature(pick.species, rrange(pick.min, pick.max));
  enterBattle("wild", [enemy]);
  battleSay([`Wild ${enemy.name} appeared!`, `Go! ${battle.ally.name}!`],
    () => { battle.phase = "menu"; });
}

export function startTrainerBattle(enemyParty, name, onWin) {
  enterBattle("trainer", enemyParty, { onWin, foeName: name });
  battleSay([`${name} wants to battle!`, `${name} sent out ${battle.enemy.name}!`, `Go! ${battle.ally.name}!`],
    () => { battle.phase = "menu"; });
}

export function startGymBattle() {
  const enemy = makeCreature("thornbud", 6);
  enterBattle("gym", [enemy], { foeName: "Leader Fern", onWin: () => { flags.gymBadge = true; game.state = STATE.WORLD; } });
  battleSay([`Leader Fern sent out ${enemy.name}!`, `Go! ${battle.ally.name}!`],
    () => { battle.phase = "menu"; });
}

// ---- Input -----------------------------------------------------------------
export function battleInput(a) {
  if (battle.phase === "anim") {
    if (a === "action" || a === "cancel") {
      battle.msgIndex++;
      if (battle.msgIndex >= battle.msg.length) {
        const cb = battle.afterMsg; battle.afterMsg = null; if (cb) cb();
      }
    }
    return;
  }
  if (battle.phase === "menu") {
    if (a === "left" || a === "right") battle.cmd ^= 1;
    if (a === "up" || a === "down") battle.cmd ^= 2;
    if (a === "action") chooseCommand(battle.cmd);
    return;
  }
  if (battle.phase === "moves") {
    const n = battle.ally.moves.length;
    if (a === "down") battle.moveIndex = (battle.moveIndex + 1) % n;
    if (a === "up") battle.moveIndex = (battle.moveIndex - 1 + n) % n;
    if (a === "cancel") { battle.phase = "menu"; return; }
    if (a === "action") {
      const mv = battle.ally.moves[battle.moveIndex];
      if (mv.pp <= 0) { battleSay(["No PP left for this move!"], () => { battle.phase = "moves"; }); return; }
      doTurn(mv);
    }
    return;
  }
  if (battle.phase === "party") { if (a === "cancel" || a === "action") battle.phase = "menu"; }
}

function chooseCommand(cmd) {
  if (cmd === 0) { battle.phase = "moves"; battle.moveIndex = 0; }
  else if (cmd === 1) battleSay(["You have no items!"], () => { battle.phase = "menu"; });
  else if (cmd === 2) battle.phase = "party";
  else if (cmd === 3) tryRun();
}

function tryRun() {
  if (!battle.canRun) { battleSay(["No! There's no running from this battle!"], () => { battle.phase = "menu"; }); return; }
  battleSay(["Got away safely!"], () => { game.state = STATE.WORLD; });
}

function pickEnemyMove(enemy) {
  const usable = enemy.moves.filter((m) => m.pp > 0);
  const pool = usable.length ? usable : [makeMove("struggle")];
  return pool[rint(pool.length)];
}

// ---- Turn resolution -------------------------------------------------------
export function doTurn(playerMove) {
  const ally = battle.ally, enemy = battle.enemy;
  const enemyMove = pickEnemyMove(enemy);
  const allyFirst = ally.speed === enemy.speed ? rand() < 0.5 : ally.speed > enemy.speed;

  const lines = [], fx = [];
  const strike = (att, def, move, foeSide) => {
    if (att.hp <= 0 || def.hp <= 0) return;
    if (move.pp != null) move.pp = Math.max(0, move.pp - 1);
    const res = calcDamage(att, def, move);
    def.hp = Math.max(0, def.hp - res.dmg);
    lines.push(`${foeSide ? "Foe " : ""}${att.name} used ${move.name}!`);
    fx.push({ act: foeSide ? "enemy" : "ally", hit: def === enemy ? "enemy" : "ally",
              hp: { who: def === enemy ? "enemy" : "ally", val: def.hp } });
    if (res.crit) { lines.push("A critical hit!"); fx.push(null); }
    if (res.eff >= 2) { lines.push("It's super effective!"); fx.push(null); }
    else if (res.eff <= 0.5) { lines.push("It's not very effective..."); fx.push(null); }
    if (def.hp <= 0) { lines.push(`${def === enemy ? "Foe " : ""}${def.name} fainted!`); fx.push({ faint: def === enemy ? "enemy" : "ally" }); }
  };

  const order = allyFirst
    ? [[ally, enemy, playerMove, false], [enemy, ally, enemyMove, true]]
    : [[enemy, ally, enemyMove, true], [ally, enemy, playerMove, false]];
  for (const [att, def, move, foeSide] of order) { if (def.hp <= 0) break; strike(att, def, move, foeSide); }

  battleSay(lines, resolveTurn, fx);
}

function resolveTurn() {
  const ally = battle.ally, enemy = battle.enemy;
  if (enemy.hp <= 0) {
    const gain = expYield(enemy);
    const levels = gainExp(ally, gain);
    const lines = [`${ally.name} gained ${gain} EXP. Points!`];
    for (const lv of levels) lines.push(`${ally.name} grew to level ${lv}!`);

    const hasNext = battle.enemyIdx + 1 < battle.enemyParty.length;
    battleSay(lines, () => {
      if (hasNext) {
        battle.enemyIdx++;
        battle.enemy = battle.enemyParty[battle.enemyIdx];
        battle.anim.hpEnemy = battle.anim.tgtEnemy = battle.enemy.hp;
        battle.anim.faint = { who: null, t: 0 };
        battleSay([`${battle.foeName} sent out ${battle.enemy.name}!`], () => { battle.phase = "menu"; });
      } else if (battle.onWin) battle.onWin();
      else game.state = STATE.WORLD;
    });
    return;
  }
  if (ally.hp <= 0) {
    battleSay([`${ally.name} fainted!`, "You have no Shapemon left!", "... You scurried back home."],
      () => { healParty(); doWarp({ map: "home", x: 5, y: 3, dir: "down" }); game.state = STATE.WORLD; });
    return;
  }
  battle.phase = "menu"; battle.cmd = 0;
}

// ---- Animation stepper (called every frame while in battle) ----------------
export function updateBattleAnim() {
  const a = battle.anim; if (!a) return;
  if (a.lastMsg !== battle.msgIndex) {
    a.lastMsg = battle.msgIndex;
    const fx = battle.fx[battle.msgIndex];
    if (fx) {
      if (fx.act) { a.lunge = { who: fx.act, t: A.lungeFrames }; a.flash = { who: fx.hit, t: A.flashFrames }; }
      if (fx.hp) { if (fx.hp.who === "ally") a.tgtAlly = fx.hp.val; else a.tgtEnemy = fx.hp.val; }
      if (fx.faint) a.faint = { who: fx.faint, t: A.faintFrames };
    }
  }
  const step = (cur, tgt, max) => {
    if (cur === tgt) return cur;
    const s = Math.max(1, Math.ceil(max / A.hpFrames));
    return cur < tgt ? Math.min(tgt, cur + s) : Math.max(tgt, cur - s);
  };
  a.hpAlly = step(a.hpAlly, a.tgtAlly, battle.ally.maxhp);
  a.hpEnemy = step(a.hpEnemy, a.tgtEnemy, battle.enemy.maxhp);
  if (a.lunge.t > 0) a.lunge.t--;
  if (a.flash.t > 0) a.flash.t--;
  if (a.faint.t > 0) a.faint.t--;
}
