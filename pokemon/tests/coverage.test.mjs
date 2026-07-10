/* Exhaustive coverage suite (Playwright): drives the real game to assert
 *   - every WARP entry point (all building doors + interior exits) and every
 *     badge-gate warps to its declared target,
 *   - every item is usable and has its effect,
 *   - party switching changes the active creature,
 *   - every move can be used and produces its effect.
 * Every check asserts. */
import { chromium } from "playwright";
import path from "path";
import { fileURLToPath } from "url";
import { createServer } from "../serve.mjs";

const dir = path.dirname(fileURLToPath(import.meta.url));
const CHROME = "/opt/pw-browsers/chromium-1194/chrome-linux/chrome";
const CODE = { up: "ArrowUp", down: "ArrowDown", left: "ArrowLeft", right: "ArrowRight", z: "KeyZ", x: "KeyX" };

let pass = 0, fail = 0;
const ok = (n, c) => { if (c) pass++; else { fail++; console.error("  FAIL:", n); } };

const wait = (page, ms) => page.waitForTimeout(ms);
const tap = async (page, k) => { await page.keyboard.press(CODE[k]); await wait(page, 35); };
const settle = (page) => page.waitForFunction("!window.__shapemon.player.moving", { timeout: 4000 }).catch(() => {});
const stepDir = async (page, d) => { await page.keyboard.press(CODE[d]); await wait(page, 35); await settle(page); await wait(page, 25); };
const S = (page) => page.evaluate(() => {
  const s = window.__shapemon;
  return { map: s.player.map, x: s.player.x, y: s.player.y, gstate: s.game.state, phase: s.battle.phase, party: s.player.party.length };
});

// Stand next to (map,x,y) on a walkable non-door tile and step onto it.
async function stepOnto(page, map, x, y) {
  const spot = await page.evaluate(({ m, x, y }) => {
    const s = window.__shapemon;
    const dirs = { up: [0, -1], down: [0, 1], left: [-1, 0], right: [1, 0] };
    const opp = { up: "down", down: "up", left: "right", right: "left" };
    for (const d in dirs) {
      const nx = x + dirs[d][0], ny = y + dirs[d][1], ch = s.tileAt(m, nx, ny);
      if (!s.isBlocked(m, nx, ny) && !"HLGCVMDE".includes(ch)) return { nx, ny, step: opp[d] };
    }
    return null;
  }, { m: map, x, y });
  if (!spot) return null;
  await page.evaluate(({ m, x, y }) => { const s = window.__shapemon; s.warpTo({ map: m, x, y, dir: "down" }); s.game.state = s.STATE.WORLD; }, { m: map, x: spot.nx, y: spot.ny });
  await stepDir(page, spot.step);
  return S(page);
}

const toMenu = async (page) => { for (let i = 0; i < 30; i++) { const s = await S(page); if (s.gstate !== "battle" || s.phase === "menu") return; await tap(page, "z"); } };
const advanceAnim = async (page) => { for (let i = 0; i < 60; i++) { const s = await S(page); if (s.gstate !== "battle" || s.phase !== "anim") return; await tap(page, "z"); } };
const ensureCmd0 = async (page) => { const c = await page.evaluate(() => window.__shapemon.battle.cmd); if (c & 1) await tap(page, "left"); if (c & 2) await tap(page, "up"); };
const exitRun = async (page) => { if ((await S(page)).gstate !== "battle") return; await ensureCmd0(page); await tap(page, "right"); await tap(page, "down"); await tap(page, "z"); for (let i = 0; i < 8; i++) { if ((await S(page)).gstate !== "battle") break; await tap(page, "z"); } };
const clearDialog = async (page) => { for (let i = 0; i < 24; i++) { if ((await S(page)).gstate !== "dialog") break; await tap(page, "z"); } };
async function winBattle(page) {
  for (let i = 0; i < 600; i++) {
    const st = await S(page);
    if (st.gstate !== "battle") return;
    if (st.phase === "moves") {
      const info = await page.evaluate(() => { const s = window.__shapemon, b = s.battle; let bi = 0, best = -1; b.ally.moves.forEach((m, idx) => { const r = s.api.calcDamage(b.ally, b.enemy, m, { forceCrit: false, rand: 100 }).dmg; if (r > best) { best = r; bi = idx; } }); return { bi, cur: b.moveIndex, n: b.ally.moves.length }; });
      if ((info.cur ^ info.bi) & 1) await tap(page, "right");
      if ((info.cur ^ info.bi) & 2) await tap(page, "down");
      await tap(page, "z");
    } else await tap(page, "z");
  }
}
// Stand next to an NPC facing it, then interact.
async function faceInteract(page, map, nx, ny) {
  const spot = await page.evaluate(({ m, x, y }) => {
    const s = window.__shapemon, dirs = { up: [0, -1], down: [0, 1], left: [-1, 0], right: [1, 0] }, opp = { up: "down", down: "up", left: "right", right: "left" };
    for (const d in dirs) { const ax = x + dirs[d][0], ay = y + dirs[d][1]; if (!s.isBlocked(m, ax, ay)) return { ax, ay, face: opp[d] }; }
    return null;
  }, { m: map, x: nx, y: ny });
  if (!spot) return null;
  await page.evaluate(({ m, x, y, f }) => { const s = window.__shapemon; s.warpTo({ map: m, x, y, dir: f }); s.game.state = s.STATE.WORLD; }, { m: map, x: spot.ax, y: spot.ay, f: spot.face });
  await tap(page, "z");
  await wait(page, 60);
  return S(page);
}

// Set up a controlled wild battle with a chosen ally + a tanky, tackle-only foe.
async function setup(page, allySpecies = "blazehound", allyLevel = 50) {
  await page.evaluate(({ es, el }) => {
    const s = window.__shapemon; s.setRng(() => 0.5);
    const a = s.api.makeCreature(es, el);
    s.player.party = [a]; s.startWildBattle(); s.battle.ally = a;
    s.battle.enemy = s.api.makeCreature("cavvit", 3);
    s.battle.enemy.maxhp = 400; s.battle.enemy.hp = 400; s.battle.enemy.moves = [s.api.makeMove("tackle")];
    const an = s.battle.anim; an.hpEnemy = an.tgtEnemy = 400; an.hpAlly = an.tgtAlly = a.hp;
  }, { es: allySpecies, el: allyLevel });
  await toMenu(page);
}

async function useBagItem(page, id) {
  await ensureCmd0(page); await tap(page, "right"); await tap(page, "z");   // FIGHT->PACK->open bag
  const idx = await page.evaluate((id) => window.__shapemon.usableInBattle().findIndex((b) => b.item === id), id);
  if (idx < 0) { await tap(page, "x"); return false; }
  for (let i = 0; i < idx; i++) await tap(page, "down");
  await tap(page, "z");
  return true;
}

(async () => {
  const server = createServer().listen(0);
  const port = server.address().port;
  const browser = await chromium.launch({ executablePath: CHROME, args: ["--no-sandbox"] });
  const page = await browser.newPage({ viewport: { width: 820, height: 620 } });
  const errors = [];
  page.on("pageerror", (e) => errors.push(String(e)));
  page.on("console", (m) => { if (m.type() === "error" && !/Failed to load resource/.test(m.text())) errors.push(m.text()); });

  await page.goto(`http://localhost:${port}/index.html`);
  await page.waitForFunction("window.__shapemon !== undefined");
  await page.evaluate(() => {
    const s = window.__shapemon;
    try { localStorage.clear(); } catch {}
    s.newGame(); s.giveStarter(); s.setNoEncounter(true);
    s.flags.badges = [true, true, true, true, true, true, true, true];   // open every gate
    s.game.state = s.STATE.WORLD;
  });
  await wait(page, 200);

  // ---------------------------------------------------------------- WARP entry points
  console.log("# entry points (warps)");
  const warps = await page.evaluate(() => Object.entries(window.__shapemon.WARPS).map(([k, v]) => ({ k, map: v.map })));
  ok("warp table non-empty", warps.length > 20);
  for (const { k, map } of warps) {
    const [srcMap, coord] = k.split(":");
    const [sx, sy] = coord.split(",").map(Number);
    const res = await stepOnto(page, srcMap, sx, sy);
    ok(`entry ${k} -> ${map}`, res && res.map === map);
  }

  console.log("# entry points (badge gates)");
  const gates = await page.evaluate(() => Object.entries(window.__shapemon.GATES).map(([k, v]) => ({ k, to: v.warp ? v.warp.map : "credits" })));
  for (const { k, to } of gates) {
    const pos = await page.evaluate((m) => { const g = window.__shapemon.MAPS[m].grid; for (let y = 0; y < g.length; y++) for (let x = 0; x < g[0].length; x++) if (g[y][x] === "E") return { x, y }; return null; }, k);
    ok(`gate tile exists on ${k}`, !!pos);
    if (!pos) continue;
    const res = await stepOnto(page, k, pos.x, pos.y);
    if (to === "credits") ok(`gate ${k} -> credits`, res && res.gstate === "credits");
    else ok(`gate ${k} -> ${to}`, res && res.map === to);
    await page.evaluate(() => { window.__shapemon.game.state = window.__shapemon.STATE.WORLD; });
  }

  // ---------------------------------------------------------------- items
  console.log("# items");
  const giveAll = () => page.evaluate(() => { const s = window.__shapemon; ["potion", "superpotion", "hyperpotion", "fullheal", "revive", "snarebell", "greatbell"].forEach((i) => s.addItem(i, 5)); });
  for (const id of ["potion", "superpotion", "hyperpotion"]) {
    await setup(page); await giveAll();
    await page.evaluate(() => { window.__shapemon.battle.ally.hp = 1; });
    const q0 = await page.evaluate((id) => window.__shapemon.itemQty(id), id);
    await useBagItem(page, id); await advanceAnim(page);
    const hp = await page.evaluate(() => window.__shapemon.battle.ally.hp);
    const q1 = await page.evaluate((id) => window.__shapemon.itemQty(id), id);
    ok(`item ${id} heals`, hp > 1 && q1 === q0 - 1);
    await exitRun(page);
  }
  { // full heal cures status
    await setup(page); await giveAll();
    await page.evaluate(() => { window.__shapemon.battle.ally.status = "burn"; });
    await useBagItem(page, "fullheal"); await advanceAnim(page);
    ok("item fullheal cures status", await page.evaluate(() => window.__shapemon.battle.ally.status === "none"));
    await exitRun(page);
  }
  { // revive a fainted party member
    await page.evaluate(() => {
      const s = window.__shapemon; s.setRng(() => 0.5);
      const a = s.api.makeCreature("blazehound", 50), b = s.api.makeCreature("nibbit", 5);
      b.hp = 0; s.player.party = [a, b]; s.startWildBattle(); s.battle.ally = a;
      s.battle.enemy = s.api.makeCreature("cavvit", 3); s.battle.enemy.maxhp = 400; s.battle.enemy.hp = 400; s.battle.enemy.moves = [s.api.makeMove("tackle")];
      const an = s.battle.anim; an.hpEnemy = an.tgtEnemy = 400; an.hpAlly = an.tgtAlly = a.hp;
    });
    await toMenu(page); await giveAll();
    await useBagItem(page, "revive"); await advanceAnim(page);
    ok("item revive restores a fainted member", await page.evaluate(() => window.__shapemon.player.party[1].hp > 0));
    await exitRun(page);
  }
  for (const ball of ["snarebell", "greatbell"]) { // balls catch
    await page.evaluate((ball) => { const s = window.__shapemon; s.setRng(() => 0.01); s.player.party = [s.api.makeCreature("blazehound", 50)]; s.startWildBattle(); s.battle.enemy.hp = 1; s.addItem(ball, 3); }, ball);
    await toMenu(page);
    const p0 = (await S(page)).party, q0 = await page.evaluate((b) => window.__shapemon.itemQty(b), ball);
    await useBagItem(page, ball);
    for (let i = 0; i < 14; i++) { if ((await S(page)).gstate !== "battle") break; await tap(page, "z"); }
    const q1 = await page.evaluate((b) => window.__shapemon.itemQty(b), ball);
    ok(`item ${ball} catches`, (await S(page)).party === p0 + 1 && q1 === q0 - 1);
  }

  // ---------------------------------------------------------------- switch party
  console.log("# switch (change Shapemon)");
  await page.evaluate(() => {
    const s = window.__shapemon; s.setRng(() => 0.5);
    s.player.party = [s.api.makeCreature("emberling", 20), s.api.makeCreature("wormling", 20), s.api.makeCreature("dribblet", 20)];
    s.startWildBattle(); s.battle.enemy.maxhp = 400; s.battle.enemy.hp = 400; s.battle.enemy.moves = [s.api.makeMove("tackle")];
    s.battle.anim.hpEnemy = s.battle.anim.tgtEnemy = 400;
  });
  await toMenu(page);
  for (const target of [1, 2]) {
    const before = await page.evaluate(() => window.__shapemon.battle.ally.speciesId);
    await ensureCmd0(page); await tap(page, "down"); await tap(page, "z");   // PKMN -> party
    await page.evaluate((t) => { window.__shapemon.battle.partyIndex = t; }, target);
    await tap(page, "z");
    await toMenu(page);
    const after = await page.evaluate(() => window.__shapemon.battle.ally.speciesId);
    ok(`switch to party slot ${target} changed active`, after !== before);
  }
  await exitRun(page);

  // ---------------------------------------------------------------- every move
  console.log("# every skill");
  const moveIds = await page.evaluate(() => Object.keys(window.__shapemon.MOVES).filter((m) => m !== "struggle"));
  for (const mid of moveIds) {
    await setup(page);
    await page.evaluate((mid) => { const s = window.__shapemon; s.battle.ally.moves = [s.api.makeMove(mid)]; s.battle.ally.hp = Math.floor(s.battle.ally.maxhp * 0.5); }, mid);
    const move = await page.evaluate((mid) => window.__shapemon.MOVES[mid], mid);
    const eHp0 = await page.evaluate(() => window.__shapemon.battle.enemy.hp);
    const aHp0 = await page.evaluate(() => window.__shapemon.battle.ally.hp);
    await ensureCmd0(page); await tap(page, "z"); await tap(page, "z");   // FIGHT -> only move
    const msgs = await page.evaluate(() => window.__shapemon.battle.msg.slice());
    const usedIt = msgs.some((l) => l.includes(`used ${move.name}!`));
    let effectOk;
    if (move.power > 0) effectOk = (await page.evaluate(() => window.__shapemon.battle.enemy.hp)) < eHp0;
    else if (move.heal) effectOk = (await page.evaluate(() => window.__shapemon.battle.ally.hp)) > aHp0;
    else if (move.effect && move.effect.stat) effectOk = await page.evaluate((k) => window.__shapemon.battle.enemy.stages[k] < 0 || window.__shapemon.battle.ally.stages[k] !== 0, move.effect.stat);
    else if (move.effect && move.effect.status) effectOk = await page.evaluate((st) => window.__shapemon.battle.enemy.status === st, move.effect.status);
    else effectOk = usedIt;
    ok(`skill ${mid}: used + effect`, usedIt && effectOk);
    await advanceAnim(page); await exitRun(page);
  }

  // ---------------------------------------------------------------- NPC roles
  console.log("# npc roles (dialog / nurse / shop / pc / prof / gym-done)");
  await page.evaluate(() => { const s = window.__shapemon; s.player.party = [s.api.makeCreature("blazehound", 60)]; s.flags.hasStarter = true; s.flags.badges = [true, true, true, true, true, true, true, true]; s.setRng(() => 0.5); s.setNoEncounter(true); });
  const allNpcs = await page.evaluate(() => { const out = [], N = window.__shapemon.NPCS; for (const map in N) for (const n of N[map]) out.push({ map, x: n.x, y: n.y, role: n.role, name: n.name }); return out; });
  let villagers = 0, nurses = 0, shops = 0, pcs = 0, gymsDone = 0;
  for (const n of allNpcs) {
    if (n.role === "villager") { const st = await faceInteract(page, n.map, n.x, n.y); ok(`villager ${n.name}@${n.map} talks`, st && st.gstate === "dialog"); await clearDialog(page); villagers++; }
    else if (n.role === "nurse") { await page.evaluate(() => { window.__shapemon.player.party[0].hp = 1; }); const st = await faceInteract(page, n.map, n.x, n.y); ok(`nurse@${n.map} dialog`, st && st.gstate === "dialog"); await clearDialog(page); ok(`nurse@${n.map} heals`, await page.evaluate(() => { const c = window.__shapemon.player.party[0]; return c.hp === c.maxhp; })); nurses++; }
    else if (n.role === "shop") { await faceInteract(page, n.map, n.x, n.y); await clearDialog(page); ok(`shop@${n.map} opens`, (await S(page)).gstate === "shop"); await tap(page, "x"); shops++; }
    else if (n.role === "pc") { await faceInteract(page, n.map, n.x, n.y); await clearDialog(page); ok(`pc@${n.map} opens`, (await S(page)).gstate === "pc"); await tap(page, "x"); pcs++; }
    else if (n.role === "prof") { const st = await faceInteract(page, n.map, n.x, n.y); ok(`prof@${n.map} dialog`, st && st.gstate === "dialog"); await clearDialog(page); }
    else if (n.role === "gym") { const st = await faceInteract(page, n.map, n.x, n.y); ok(`gym-done ${n.name} dialog`, st && st.gstate === "dialog"); await clearDialog(page); gymsDone++; }
  }
  ok("all heal centers covered", nurses >= 5 && pcs >= 5);
  ok("mart + villagers + all gyms covered", shops >= 1 && villagers >= 4 && gymsDone === 8);

  // ---------------------------------------------------------------- trainer battles (all)
  console.log("# trainer battles (all)");
  const trainerNames = allNpcs.filter((n) => n.role === "trainer");
  for (const t of trainerNames) {
    await page.evaluate((tt) => { const s = window.__shapemon; s.player.party = [s.api.makeCreature("blazehound", 60)]; s.player.money = 0; s.setRng(() => 0.5); s.setNoEncounter(true); for (const n of s.NPCS[tt.map]) if (n.role === "trainer" && n.x === tt.x && n.y === tt.y) n.defeated = false; }, t);
    const st = await faceInteract(page, t.map, t.x, t.y);
    ok(`trainer ${t.name}@${t.map} intro`, st && st.gstate === "dialog");
    await clearDialog(page);
    ok(`trainer ${t.name}@${t.map} battle starts`, (await S(page)).gstate === "battle");
    await winBattle(page); await clearDialog(page);
    const done = await page.evaluate((tt) => { for (const n of window.__shapemon.NPCS[tt.map]) if (n.role === "trainer" && n.x === tt.x && n.y === tt.y) return n.defeated; }, t);
    ok(`trainer ${t.name}@${t.map} defeated + prize`, done === true && (await page.evaluate(() => window.__shapemon.player.money)) > 0);
  }

  // ---------------------------------------------------------------- gym battles (all 8)
  console.log("# gym battles (all 8)");
  const GB = await page.evaluate(() => window.__shapemon.GYM_BADGES);
  for (const g of GB) {
    await page.evaluate((b) => { const s = window.__shapemon; s.player.party = [s.api.makeCreature("blazehound", 60)]; s.flags.badges = [true, true, true, true, true, true, true, true]; s.flags.badges[b] = false; s.setRng(() => 0.5); s.setNoEncounter(true); }, g.badge);
    await page.evaluate((gm) => { const s = window.__shapemon; s.warpTo({ map: gm, x: 6, y: 3, dir: "up" }); s.game.state = s.STATE.WORLD; }, g.gymMap);
    await tap(page, "z"); await wait(page, 60);
    ok(`gym ${g.badge} intro`, (await S(page)).gstate === "dialog");
    await clearDialog(page);
    ok(`gym ${g.badge} battle starts`, (await S(page)).gstate === "battle");
    await winBattle(page);
    ok(`gym ${g.badge} badge earned`, await page.evaluate((b) => !!window.__shapemon.flags.badges[b], g.badge));
    await clearDialog(page);
  }

  // ---------------------------------------------------------------- status conditions
  console.log("# status conditions");
  for (const [st, needle] of [["poison", "poison"], ["burn", "burn"]]) {
    await setup(page);
    await page.evaluate((s) => { window.__shapemon.battle.enemy.status = s; }, st);
    await page.evaluate(() => { window.__shapemon.battle.ally.moves = [window.__shapemon.api.makeMove("growl")]; });
    await ensureCmd0(page); await tap(page, "z"); await tap(page, "z");
    ok(`status ${st} end-of-turn tick`, (await page.evaluate(() => window.__shapemon.battle.msg.slice())).some((l) => l.toLowerCase().includes(`hurt by its ${needle}`)));
    await advanceAnim(page); await exitRun(page);
  }
  await setup(page);   // paralysis skip
  await page.evaluate(() => { const s = window.__shapemon; s.setRng(() => 0.0); s.battle.ally.status = "paralysis"; s.battle.ally.moves = [s.api.makeMove("scratch")]; });
  await ensureCmd0(page); await tap(page, "z"); await tap(page, "z");
  ok("status paralysis can skip a turn", (await page.evaluate(() => window.__shapemon.battle.msg.slice())).some((l) => l.includes("paralyzed")));
  await page.evaluate(() => window.__shapemon.setRng(() => 0.5)); await advanceAnim(page); await exitRun(page);
  await setup(page);   // sleep skip
  await page.evaluate(() => { const s = window.__shapemon; s.battle.ally.status = "sleep"; s.battle.ally.sleepTurns = 2; s.battle.ally.moves = [s.api.makeMove("scratch")]; });
  await ensureCmd0(page); await tap(page, "z"); await tap(page, "z");
  ok("status sleep skips a turn", (await page.evaluate(() => window.__shapemon.battle.msg.slice())).some((l) => l.toLowerCase().includes("fast asleep")));
  await advanceAnim(page); await exitRun(page);

  // ---------------------------------------------------------------- level-up + evolution
  console.log("# level-up + evolution");
  await page.evaluate(() => { const s = window.__shapemon; s.setRng(() => 0.9); const a = s.api.makeCreature("emberling", 8); a.exp = s.api.expForLevel(9) - 1; s.player.party = [a]; s.startWildBattle(); s.battle.ally = a; s.battle.enemy = s.api.makeCreature("nibbit", 3); s.battle.enemy.hp = 1; const an = s.battle.anim; an.hpEnemy = an.tgtEnemy = 1; an.hpAlly = an.tgtAlly = a.hp; });
  await toMenu(page); await winBattle(page);
  ok("battle win grants a level-up", await page.evaluate(() => window.__shapemon.player.party[0].level >= 9));
  await page.evaluate(() => { const s = window.__shapemon; s.setRng(() => 0.9); const a = s.api.makeCreature("emberling", 15); a.exp = s.api.expForLevel(16) - 1; s.player.party = [a]; s.startWildBattle(); s.battle.ally = a; s.battle.enemy = s.api.makeCreature("nibbit", 3); s.battle.enemy.hp = 1; const an = s.battle.anim; an.hpEnemy = an.tgtEnemy = 1; an.hpAlly = an.tgtAlly = a.hp; });
  await toMenu(page); await winBattle(page);
  ok("Emberling evolves into Blazehound", await page.evaluate(() => window.__shapemon.player.party[0].speciesId === "blazehound"));

  // ---------------------------------------------------------------- run + blackout
  console.log("# run + blackout");
  await setup(page);
  await ensureCmd0(page); await tap(page, "right"); await tap(page, "down"); await tap(page, "z");
  for (let i = 0; i < 6; i++) { if ((await S(page)).gstate !== "battle") break; await tap(page, "z"); }
  ok("run from a wild battle escapes", (await S(page)).gstate === "world");
  await page.evaluate(() => { const s = window.__shapemon; s.setRng(() => 0.5); const a = s.api.makeCreature("nibbit", 2); a.hp = 1; s.player.party = [a]; s.startWildBattle(); s.battle.enemy = s.api.makeCreature("cavvit", 60); s.battle.enemy.moves = [s.api.makeMove("tackle")]; const an = s.battle.anim; an.hpEnemy = an.tgtEnemy = s.battle.enemy.hp; an.hpAlly = an.tgtAlly = 1; });
  await toMenu(page); await ensureCmd0(page); await tap(page, "z"); await tap(page, "z");
  for (let i = 0; i < 24; i++) { if ((await S(page)).gstate === "world") break; await tap(page, "z"); }
  ok("blackout warps home after a party wipe", (await S(page)).map === "home");

  // ---------------------------------------------------------------- save / continue
  console.log("# save / continue");
  await page.evaluate(() => { const s = window.__shapemon; s.player.money = 777; s.flags.badges = [true, false, true, false, false, false, false, false]; s.player.party = [s.api.makeCreature("emberling", 9)]; s.player.box = [s.api.makeCreature("wormling", 6)]; s.player.map = "town"; s.player.x = 7; s.player.y = 17; s.saveGame(); s.player.money = 0; s.flags.badges = []; s.player.party = []; s.player.box = []; s.continueGame(); });
  ok("save/continue restores money", await page.evaluate(() => window.__shapemon.player.money === 777));
  ok("save/continue restores badges", await page.evaluate(() => window.__shapemon.flags.badges[0] === true && window.__shapemon.flags.badges[2] === true));
  ok("save/continue restores party + box", await page.evaluate(() => window.__shapemon.player.party[0]?.speciesId === "emberling" && window.__shapemon.player.box[0]?.speciesId === "wormling"));

  await browser.close();
  server.close();
  ok("no JS errors during coverage", errors.length === 0);
  if (errors.length) console.error("  errors:", errors.slice(0, 5));
  console.log(`\n=== coverage: ${pass} passed, ${fail} failed ===`);
  process.exit(fail ? 1 : 0);
})();
