/* Battle engine — authentic FireRed sequence & layout, with animation.
 * Supports wild / trainer / gym battles, multi-creature parties, items, catching,
 * switching, status conditions, stat stages, move-learning, and evolution. */
import { CONFIG } from "../data/config.js";
import { STATE, game, player, battle, flags } from "../state.js";
import { SPECIES } from "../data/species.js";
import { ITEMS } from "../data/items.js";
import { ENCOUNTERS } from "../data/encounters.js";
import { calcDamage } from "./damage.js";
import { gainExp, expYield } from "./stats.js";
import { makeCreature, makeMove, resetStages, learnableAt, learnMove, evolveIfReady } from "./creature.js";
import { healParty, firstHealthy, partyWiped } from "./party.js";
import { removeItem, addItem, applyItem, usableInBattle } from "./bag.js";
import { doWarp } from "./world.js";
import { rint, rand, rrange } from "../core/rng.js";

const A = CONFIG.animation, B = CONFIG.battle;
const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));

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
  battle.enemyParty = enemyParty; battle.enemyIdx = 0; battle.enemy = enemyParty[0];
  battle.ally = firstHealthy() || player.party[0];
  battle.onWin = opts.onWin || null;
  battle.prize = opts.prize || 0;
  battle.canRun = kind === "wild";
  battle.foeName = opts.foeName || null;
  battle.mustSwitch = false;
  battle.phase = "intro"; battle.cmd = 0; battle.moveIndex = 0; battle.bagIndex = 0; battle.partyIndex = 0;
  resetStages(battle.ally); resetStages(battle.enemy);
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
  battleSay([`Wild ${enemy.name} appeared!`, `Go! ${battle.ally.name}!`], () => { battle.phase = "menu"; });
}
export function startTrainerBattle(enemyParty, name, onWin) {
  const prize = CONFIG.economy.trainerPrizePerLevel * enemyParty[enemyParty.length - 1].level;
  enterBattle("trainer", enemyParty, { onWin, foeName: name, prize });
  battleSay([`${name} wants to battle!`, `${name} sent out ${battle.enemy.name}!`, `Go! ${battle.ally.name}!`], () => { battle.phase = "menu"; });
}
export function startGymBattle(leaderName, party, badgeIndex, onWin) {
  enterBattle("gym", party, { foeName: leaderName, prize: CONFIG.economy.gymPrize, onWin });
  battleSay([`${leaderName} sent out ${battle.enemy.name}!`, `Go! ${battle.ally.name}!`], () => { battle.phase = "menu"; });
}

// ---- Input -----------------------------------------------------------------
export function battleInput(a) {
  const ph = battle.phase;
  if (ph === "anim") {
    if (a === "action" || a === "cancel") {
      battle.msgIndex++;
      if (battle.msgIndex >= battle.msg.length) { const cb = battle.afterMsg; battle.afterMsg = null; if (cb) cb(); }
    }
    return;
  }
  if (ph === "menu") {
    if (a === "left" || a === "right") battle.cmd ^= 1;
    if (a === "up" || a === "down") battle.cmd ^= 2;
    if (a === "action") chooseCommand(battle.cmd);
    return;
  }
  if (ph === "moves") {
    const n = battle.ally.moves.length;
    // 2x2 grid navigation, clamped to existing moves
    if (a === "left" || a === "right") { const t = battle.moveIndex ^ 1; if (t < n) battle.moveIndex = t; }
    if (a === "up" || a === "down") { const t = battle.moveIndex ^ 2; if (t < n) battle.moveIndex = t; }
    if (a === "cancel") { battle.phase = "menu"; return; }
    if (a === "action") {
      const mv = battle.ally.moves[battle.moveIndex];
      if (mv.pp <= 0) { battleSay(["No PP left for this move!"], () => { battle.phase = "moves"; }); return; }
      useMove(mv);
    }
    return;
  }
  if (ph === "bag") {
    const items = usableInBattle();
    if (!items.length) { battle.phase = "menu"; return; }
    if (a === "down") battle.bagIndex = (battle.bagIndex + 1) % items.length;
    if (a === "up") battle.bagIndex = (battle.bagIndex - 1 + items.length) % items.length;
    if (a === "cancel") { battle.phase = "menu"; return; }
    if (a === "action") useItem(items[Math.min(battle.bagIndex, items.length - 1)].item);
    return;
  }
  if (ph === "party") {
    const n = player.party.length;
    if (a === "down") battle.partyIndex = (battle.partyIndex + 1) % n;
    if (a === "up") battle.partyIndex = (battle.partyIndex - 1 + n) % n;
    if (a === "cancel" && !battle.mustSwitch) { battle.phase = "menu"; return; }
    if (a === "action") {
      const idx = battle.partyIndex, cand = player.party[idx];
      if (cand === battle.ally || cand.hp <= 0) return;   // invalid pick
      switchTo(idx, battle.mustSwitch);
    }
    return;
  }
}

function chooseCommand(cmd) {
  if (cmd === 0) { battle.phase = "moves"; battle.moveIndex = 0; }
  else if (cmd === 1) { battle.phase = "bag"; battle.bagIndex = 0; }
  else if (cmd === 2) { battle.phase = "party"; battle.partyIndex = player.party.indexOf(battle.ally); }
  else if (cmd === 3) tryRun();
}
function tryRun() {
  if (!battle.canRun) { battleSay(["No! There's no running from this battle!"], () => { battle.phase = "menu"; }); return; }
  battleSay(["Got away safely!"], () => { game.state = STATE.WORLD; });
}

// ---- Status helpers --------------------------------------------------------
function effectiveSpeed(cr) {
  const base = cr.speed * (cr.stages ? (cr.stages.speed >= 0 ? (2 + cr.stages.speed) / 2 : 2 / (2 - cr.stages.speed)) : 1);
  return cr.status === "paralysis" ? base * B.paralysisSpeed : base;
}
function applyStatus(target, status, q, fx) {
  if (target.status !== "none") return false;
  target.status = status;
  if (status === "sleep") target.sleepTurns = rrange(B.sleepMin, B.sleepMax);
  const word = { burn: "was burned", poison: "was poisoned", paralysis: "is paralyzed", sleep: "fell asleep" }[status];
  q.push(`${side(target)}${target.name} ${word}!`); fx.push(null);
  return true;
}
function changeStage(cr, stat, delta, q, fx) {
  const old = cr.stages[stat];
  cr.stages[stat] = clamp(old + delta, -6, 6);
  if (cr.stages[stat] === old) { q.push(`${side(cr)}${cr.name}'s ${stat.toUpperCase()} won't go higher!`); fx.push(null); return; }
  const dir = delta > 0 ? "rose" : "fell";
  const sharp = Math.abs(delta) >= 2 ? " sharply" : "";
  q.push(`${side(cr)}${cr.name}'s ${stat.toUpperCase()}${sharp} ${dir}!`); fx.push(null);
}
const side = (cr) => (cr === battle.enemy ? "Foe " : "");
const hpFx = (cr) => ({ hp: { who: cr === battle.enemy ? "enemy" : "ally", val: cr.hp } });

// Smarter AI: usually pick the highest-damage move vs the current ally.
function pickEnemyMove(enemy) {
  const usable = enemy.moves.filter((m) => m.pp > 0);
  if (!usable.length) return makeMove("struggle");
  if (rand() < 0.25) return usable[rint(usable.length)];   // a little unpredictability
  let best = usable[0], bestScore = -1;
  for (const m of usable) {
    const score = m.power > 0 ? calcDamage(enemy, battle.ally, m, { forceCrit: false, rand: 100 }).dmg : 1;
    if (score > bestScore) { bestScore = score; best = m; }
  }
  return best;
}
const faintMsg = (def, q, fx) => { q.push(`${side(def)}${def.name} fainted!`); fx.push({ faint: def === battle.enemy ? "enemy" : "ally" }); };

// One attacker's action within a turn; pushes messages + fx into q/fx.
function strike(att, def, move, q, fx) {
  if (att.hp <= 0 || def.hp <= 0) return;
  const actSide = att === battle.enemy ? "enemy" : "ally";
  // flinch (set by a faster foe's move earlier this turn)
  if (att.flinched) { att.flinched = false; q.push(`${side(att)}${att.name} flinched and couldn't move!`); fx.push(null); return; }
  // sleep / paralysis gating
  if (att.status === "sleep") {
    if (att.sleepTurns > 0) att.sleepTurns--;
    if (att.sleepTurns <= 0 && att.status === "sleep") { att.status = "none"; q.push(`${side(att)}${att.name} woke up!`); fx.push(null); }
    else { q.push(`${side(att)}${att.name} is fast asleep.`); fx.push(null); return; }
  }
  if (att.status === "paralysis" && rand() < B.paralysisSkipChance) {
    q.push(`${side(att)}${att.name} is paralyzed! It can't move!`); fx.push(null); return;
  }
  if (move.pp != null) move.pp = Math.max(0, move.pp - 1);

  // accuracy
  if (rand() * 100 >= move.accuracy) { q.push(`${side(att)}${att.name} used ${move.name}!`); fx.push({ act: actSide }); q.push("But it missed!"); fx.push(null); return; }

  if (move.power > 0) {
    const hits = move.multi || 1;
    let total = 0, res, hitCount = 0;
    for (let h = 0; h < hits; h++) { if (def.hp <= 0) break; res = calcDamage(att, def, move); def.hp = Math.max(0, def.hp - res.dmg); total += res.dmg; hitCount++; }
    q.push(`${side(att)}${att.name} used ${move.name}!`);
    fx.push({ act: actSide, hit: def === battle.enemy ? "enemy" : "ally", ...hpFx(def) });
    if (hitCount > 1) { q.push(`Hit ${hitCount} times!`); fx.push(null); }
    if (res.crit) { q.push("A critical hit!"); fx.push(null); }
    if (res.eff >= 2) { q.push("It's super effective!"); fx.push(null); }
    else if (res.eff <= 0.5) { q.push("It's not very effective..."); fx.push(null); }
    if (move.recoil && total > 0) {
      att.hp = Math.max(0, att.hp - Math.max(1, Math.floor(total * move.recoil)));
      q.push(`${side(att)}${att.name} is hit by recoil!`); fx.push(hpFx(att));
    }
    if (def.hp <= 0) { faintMsg(def, q, fx); return; }
    if (att.hp <= 0) { faintMsg(att, q, fx); return; }
    if (move.effect && move.effect.status && rand() < (move.effect.chance ?? 1)) applyStatus(def, move.effect.status, q, fx);
    if (move.effect && move.effect.flinch && rand() < move.effect.flinch) def.flinched = true;
  } else if (move.heal) {
    q.push(`${side(att)}${att.name} used ${move.name}!`); fx.push({ act: actSide });
    if (att.hp >= att.maxhp) { q.push("But its HP is already full!"); fx.push(null); }
    else { att.hp = Math.min(att.maxhp, att.hp + Math.floor(att.maxhp * move.heal)); q.push(`${side(att)}${att.name} regained health!`); fx.push(hpFx(att)); }
  } else if (move.effect) {
    q.push(`${side(att)}${att.name} used ${move.name}!`); fx.push({ act: actSide });
    if (move.effect.stat) changeStage(move.effect.target === "self" ? att : def, move.effect.stat, move.effect.stages, q, fx);
    else if (move.effect.status) { if (!applyStatus(def, move.effect.status, q, fx)) { q.push("But it failed!"); fx.push(null); } }
  }
}

function endOfTurnStatus(cr, q, fx) {
  if (cr.hp <= 0) return;
  if (cr.status === "burn" || cr.status === "poison") {
    const dmg = Math.max(1, Math.floor(cr.maxhp * (cr.status === "burn" ? B.burnDamage : B.poisonDamage)));
    cr.hp = Math.max(0, cr.hp - dmg);
    q.push(`${side(cr)}${cr.name} is hurt by its ${cr.status}!`); fx.push(hpFx(cr));
    if (cr.hp <= 0) { q.push(`${side(cr)}${cr.name} fainted!`); fx.push({ faint: cr === battle.enemy ? "enemy" : "ally" }); }
  }
}

// ---- Player actions --------------------------------------------------------
function useMove(playerMove) {
  const ally = battle.ally, enemy = battle.enemy;
  const enemyMove = pickEnemyMove(enemy);
  const q = [], fx = [];
  ally.flinched = false; enemy.flinched = false;   // flinch lasts only within a turn
  // higher move priority acts first, then Speed (ties random)
  const pr = (m) => m.priority || 0;
  let allyFirst;
  if (pr(playerMove) !== pr(enemyMove)) allyFirst = pr(playerMove) > pr(enemyMove);
  else allyFirst = effectiveSpeed(ally) === effectiveSpeed(enemy) ? rand() < 0.5 : effectiveSpeed(ally) > effectiveSpeed(enemy);
  const order = allyFirst ? [[ally, enemy, playerMove], [enemy, ally, enemyMove]] : [[enemy, ally, enemyMove], [ally, enemy, playerMove]];
  for (const [att, def, mv] of order) { if (att.hp > 0 && def.hp > 0) strike(att, def, mv, q, fx); }
  if (enemy.hp > 0 && ally.hp > 0) { endOfTurnStatus(ally, q, fx); endOfTurnStatus(enemy, q, fx); }
  battleSay(q, resolveTurn, fx);
}

function useItem(id) {
  const it = ITEMS[id];
  if (it.kind === "ball") { attemptCatch(id); return; }
  const target = it.kind === "revive" ? player.party.find((c) => c.hp <= 0) : battle.ally;
  const msg = target ? applyItem(id, target) : null;
  if (!msg) { battleSay(["It won't have any effect."], () => { battle.phase = "bag"; }); return; }
  removeItem(id);
  const q = [`You used ${it.name}.`, msg], fx = [null, target === battle.ally ? hpFx(battle.ally) : null];
  const enemyMove = pickEnemyMove(battle.enemy);
  if (battle.ally.hp > 0) strike(battle.enemy, battle.ally, enemyMove, q, fx);
  if (battle.enemy.hp > 0 && battle.ally.hp > 0) endOfTurnStatus(battle.ally, q, fx);
  battleSay(q, resolveTurn, fx);
}

function switchTo(idx, forced) {
  resetStages(battle.ally);
  battle.ally = player.party[idx];
  resetStages(battle.ally);
  battle.anim.hpAlly = battle.anim.tgtAlly = battle.ally.hp;
  const q = [`Go! ${battle.ally.name}!`], fx = [null];
  if (!forced) {   // a voluntary switch spends the turn; the foe attacks
    const enemyMove = pickEnemyMove(battle.enemy);
    if (battle.ally.hp > 0) strike(battle.enemy, battle.ally, enemyMove, q, fx);
    if (battle.enemy.hp > 0 && battle.ally.hp > 0) endOfTurnStatus(battle.ally, q, fx);
  }
  battle.mustSwitch = false;
  battleSay(q, resolveTurn, fx);
}

function catchChance(enemy, ball) {
  const bonus = ITEMS[ball].ballBonus;
  const hpFactor = (enemy.maxhp * 3 - enemy.hp * 2) / (enemy.maxhp * 3);
  const statusBonus = CONFIG.catch.statusBonus[enemy.status] ?? 1;
  return clamp(bonus * hpFactor * statusBonus, 0, 1);
}
function attemptCatch(ball) {
  removeItem(ball);
  const enemy = battle.enemy;
  const caught = rand() < catchChance(enemy, ball);
  if (!caught) {
    const q = [`You threw a ${ITEMS[ball].name}!`, "Oh no! It broke free!"], fx = [null, null];
    const enemyMove = pickEnemyMove(enemy);
    if (battle.ally.hp > 0) strike(enemy, battle.ally, enemyMove, q, fx);
    if (battle.ally.hp > 0) endOfTurnStatus(battle.ally, q, fx);
    battleSay(q, resolveTurn, fx);
    return;
  }
  enemy.status = "none"; resetStages(enemy);
  const toParty = player.party.length < CONFIG.party.max;
  if (toParty) player.party.push(enemy); else player.box.push(enemy);
  battleSay([`You threw a ${ITEMS[ball].name}!`, `Gotcha! ${enemy.name} was caught!`,
    toParty ? `${enemy.name} joined your party!` : `${enemy.name} was sent to storage.`],
    () => { game.state = STATE.WORLD; });
}

// ---- Turn resolution -------------------------------------------------------
function resolveTurn() {
  const ally = battle.ally, enemy = battle.enemy;

  if (enemy.hp <= 0) {
    const gain = expYield(enemy);
    const levelsBefore = ally.level;
    const levels = gainExp(ally, gain);
    const lines = [`${ally.name} gained ${gain} EXP. Points!`], after = [];
    for (const lv of levels) {
      lines.push(`${ally.name} grew to level ${lv}!`);
      for (const mv of learnableAt(ally, lv)) {
        if (learnMove(ally, mv) === "learned") lines.push(`${ally.name} learned ${mv.replace(/^\w/, (c) => c)}!`);
      }
    }
    if (levels.length) { const evolved = evolveIfReady(ally); if (evolved) { lines.push(`What? ${ally.name} is evolving!`); lines.push(`${ally.name} evolved into ${evolved}!`); } }

    const hasNext = battle.enemyIdx + 1 < battle.enemyParty.length;
    battleSay(lines, () => {
      if (hasNext) {
        battle.enemyIdx++; battle.enemy = battle.enemyParty[battle.enemyIdx]; resetStages(battle.enemy);
        battle.anim.hpEnemy = battle.anim.tgtEnemy = battle.enemy.hp; battle.anim.faint = { who: null, t: 0 };
        battleSay([`${battle.foeName} sent out ${battle.enemy.name}!`], () => { battle.phase = "menu"; });
      } else {
        finishWin();
      }
    });
    return;
  }

  if (ally.hp <= 0) {
    if (!partyWiped()) {
      battle.mustSwitch = true;
      battleSay([`${ally.name} fainted!`], () => { battle.phase = "party"; battle.partyIndex = player.party.findIndex((c) => c.hp > 0); });
    } else {
      battleSay([`${ally.name} fainted!`, "You have no Shapemon left!", "... You scurried back home."],
        () => { healParty(); doWarp({ map: "home", x: 5, y: 3, dir: "down" }); game.state = STATE.WORLD; });
    }
    return;
  }
  battle.phase = "menu"; battle.cmd = 0;
}

function finishWin() {
  if (battle.prize > 0) {
    player.money += battle.prize;
    battleSay([`You won ${battle.prize} coins!`], () => { if (battle.onWin) battle.onWin(); else game.state = STATE.WORLD; });
  } else if (battle.onWin) battle.onWin();
  else game.state = STATE.WORLD;
}

// ---- Animation stepper -----------------------------------------------------
export function updateBattleAnim() {
  const a = battle.anim; if (!a) return;
  if (a.lastMsg !== battle.msgIndex) {
    a.lastMsg = battle.msgIndex;
    const fx = battle.fx[battle.msgIndex];
    if (fx) {
      if (fx.act) { a.lunge = { who: fx.act, t: A.lungeFrames }; if (fx.hit) a.flash = { who: fx.hit, t: A.flashFrames }; }
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
