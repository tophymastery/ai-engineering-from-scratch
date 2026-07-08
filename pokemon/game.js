/* =============================================================================
 * Shapemon — Ember Quest
 * An original, Gen-1-style creature RPG. Grid movement, tile collision,
 * turn-based battles, a starter, a gym, and a credits screen.
 * All content (creatures, maps, story, names) is original. Sprites are drawn
 * from simple shapes; tile/collision sizes are grid-accurate.
 * ========================================================================== */

(() => {
  "use strict";

  const canvas = document.getElementById("game");
  const ctx = canvas.getContext("2d");

  // ---- Grid / viewport ------------------------------------------------------
  const TILE = 16;          // logical tile size
  const SCALE = 3;          // draw scale
  const TS = TILE * SCALE;  // on-screen tile size (48px)
  const VIEW_W = canvas.width / TS;   // 10 tiles wide
  const VIEW_H = canvas.height / TS;  // 9 tiles tall
  const MOVE_FRAMES = 12;   // frames to cross one tile
  const ENCOUNTER_RATE = 0.14;

  // ---- Colors ---------------------------------------------------------------
  const C = {
    grass: "#5bab53", grassDark: "#3f8a45",
    tall: "#2f6b3b", tallHi: "#3f8a45",
    path: "#d8c58c", pathEdge: "#c3ac6e",
    tree: "#1f5a2c", treeTrunk: "#5a3a1e",
    roof: "#b5423b", roofDark: "#8f302b", wall: "#e5d8b8",
    door: "#5a3a1e", water: "#3f6fd4",
    floor: "#c9b79a", floorLine: "#b7a382", iwall: "#6b5640",
    text: "#f4f4ec", box: "#101427", boxBorder: "#f4f4ec",
    hpGreen: "#57c34a", hpYellow: "#e6c327", hpRed: "#d63c3c",
    player: "#3553ff", playerHi: "#8fb6ff",
    npc: "#9a5cff",
  };

  // ---- Type effectiveness ---------------------------------------------------
  const TYPES = ["fire", "water", "grass", "normal"];
  const TYPE_COLOR = { fire: "#ff7a33", water: "#3f9fff", grass: "#5bd06a", normal: "#c9c9c9" };
  // eff[atk][def]
  const EFF = {
    fire:   { fire: 0.5, water: 0.5, grass: 2.0, normal: 1.0 },
    water:  { fire: 2.0, water: 0.5, grass: 0.5, normal: 1.0 },
    grass:  { fire: 0.5, water: 2.0, grass: 0.5, normal: 1.0 },
    normal: { fire: 1.0, water: 1.0, grass: 1.0, normal: 1.0 },
  };
  const typeEff = (atk, def) => (EFF[atk] && EFF[atk][def] != null) ? EFF[atk][def] : 1.0;

  // ---- Moves ----------------------------------------------------------------
  const MOVES = {
    scratch:  { name: "Scratch",   type: "normal", power: 40 },
    tackle:   { name: "Tackle",    type: "normal", power: 35 },
    ember:    { name: "Ember",     type: "fire",   power: 45 },
    vinewhip: { name: "Vine Whip", type: "grass",  power: 40 },
    watergun: { name: "Water Gun", type: "water",  power: 40 },
  };

  // ---- Species --------------------------------------------------------------
  const SPECIES = {
    emberling: { name: "Emberling", type: "fire",   color: "#ff7a33", shape: "flame",
                 base: { hp: 24, atk: 12, def: 10, spd: 12 }, moves: ["scratch", "ember"] },
    wormling:  { name: "Wormling",  type: "grass",  color: "#7bd06a", shape: "blob",
                 base: { hp: 18, atk: 8,  def: 8,  spd: 7  }, moves: ["tackle", "vinewhip"] },
    nibbit:    { name: "Nibbit",    type: "normal", color: "#c9a06a", shape: "round",
                 base: { hp: 17, atk: 9,  def: 7,  spd: 10 }, moves: ["tackle", "scratch"] },
    thornbud:  { name: "Thornbud",  type: "grass",  color: "#3fae5a", shape: "spike",
                 base: { hp: 26, atk: 11, def: 12, spd: 8  }, moves: ["tackle", "vinewhip"] },
  };

  // Build a battle-ready creature instance from a species id + level.
  function makeCreature(id, level) {
    const s = SPECIES[id];
    const maxhp = s.base.hp + level * 3;
    return {
      id, name: s.name, type: s.type, color: s.color, shape: s.shape,
      level,
      maxhp, hp: maxhp,
      atk: s.base.atk + level,
      def: s.base.def + level,
      spd: s.base.spd + level,
      moves: s.moves.map((m) => MOVES[m]),
    };
  }

  // ---- Maps -----------------------------------------------------------------
  // Legend:
  //  '.' grass  '_' path  ':' tall grass (encounters)  'T' tree
  //  '#' building wall  '=' interior wall  '~' water  'F' floor
  //  'H'/'L'/'G' overworld doors (home/lab/gym)  'D' interior exit door
  const RAW = {
    town: [
      "TTTTTTTTTTTTTTT",
      "T.....###.....T",
      "T.....#G#.....T",
      "T......_......T",
      "T......_......T",
      "T.....:_:.....T",
      "T......_......T",
      "T....::_::....T",
      "T......_......T",
      "T....::_::....T",
      "T......_......T",
      "T.....:_:.....T",
      "T......_......T",
      "T......_......T",
      "T......_......T",
      "T.###.._..###.T",
      "T.#H#.._..#L#.T",
      "T......_......T",
      "T......_......T",
      "TTTTTTTTTTTTTTT",
    ],
    home: [
      "=========",
      "=FFFFFFF=",
      "=FFFFFFF=",
      "=FFFFFFF=",
      "=FFFFFFF=",
      "=FFFFFFF=",
      "=FFFFFFF=",
      "====D====",
    ],
    lab: [
      "===========",
      "=FFFFFFFFF=",
      "=FFFFFFFFF=",
      "=FFFFFFFFF=",
      "=FFFFFFFFF=",
      "=FFFFFFFFF=",
      "=FFFFFFFFF=",
      "=FFFFFFFFF=",
      "=====D=====",
    ],
    gym: [
      "===========",
      "=FFFFFFFFF=",
      "=FFFFFFFFF=",
      "=FFFFFFFFF=",
      "=FFFFFFFFF=",
      "=FFFFFFFFF=",
      "=FFFFFFFFF=",
      "=FFFFFFFFF=",
      "=FFFFFFFFF=",
      "=FFFFFFFFF=",
      "=====D=====",
    ],
  };

  // Warp table: `${map}:${x},${y}` -> { map, x, y, dir }
  const WARPS = {
    "town:3,16":  { map: "home", x: 4, y: 6, dir: "up" },   // enter home
    "town:11,16": { map: "lab",  x: 5, y: 7, dir: "up" },   // enter lab
    "town:7,2":   { map: "gym",  x: 5, y: 9, dir: "up" },   // enter gym
    "home:4,7":   { map: "town", x: 3, y: 17, dir: "down" },
    "lab:5,8":    { map: "town", x: 11, y: 17, dir: "down" },
    "gym:5,10":   { map: "town", x: 7, y: 3, dir: "down" },
  };

  // NPCs per map: interact by facing them and pressing Confirm.
  const NPCS = {
    lab: [{ x: 5, y: 2, color: "#e0e0e0", shape: "prof", name: "Prof. Cedar", role: "prof" }],
    gym: [{ x: 5, y: 2, color: "#2f9e57", shape: "leader", name: "Leader Fern", role: "gym" }],
  };

  // Parse raw string maps into { name, grid, w, h }
  const MAPS = {};
  for (const name in RAW) {
    const grid = RAW[name].map((row) => row.split(""));
    const w = grid[0].length;
    for (const row of grid) {
      if (row.length !== w) console.error(`Map ${name} row width mismatch (${row.length} != ${w})`);
    }
    MAPS[name] = { name, grid, w, h: grid.length };
  }

  const WALKABLE = new Set([".", "_", ":", "F", "H", "L", "G", "D"]);
  function tileAt(map, x, y) {
    if (x < 0 || y < 0 || y >= map.h || x >= map.w) return "T";
    return map.grid[y][x];
  }
  function npcAt(mapName, x, y) {
    const list = NPCS[mapName] || [];
    return list.find((n) => n.x === x && n.y === y) || null;
  }
  function isBlocked(mapName, x, y) {
    const map = MAPS[mapName];
    if (!WALKABLE.has(tileAt(map, x, y))) return true;
    if (npcAt(mapName, x, y)) return true;
    return false;
  }

  // ---- Game state -----------------------------------------------------------
  const STATE = { TITLE: "title", WORLD: "world", DIALOG: "dialog", BATTLE: "battle", CREDITS: "credits" };
  const DIRV = { up: [0, -1], down: [0, 1], left: [-1, 0], right: [1, 0] };

  const flags = { hasStarter: false, gymBadge: false };

  const player = {
    map: "home", x: 4, y: 3, dir: "down",
    px: 4 * TILE, py: 3 * TILE, // pixel position (top-left of tile)
    moving: false, from: null, to: null, progress: 0,
    party: [],
  };

  const game = {
    state: STATE.TITLE,
    afterDialog: null,   // callback when dialog queue empties
    tick: 0,
  };

  // Dialog queue
  const dialog = { lines: [], index: 0, active: false };
  function say(lines, after) {
    dialog.lines = Array.isArray(lines) ? lines : [lines];
    dialog.index = 0;
    dialog.active = true;
    game.afterDialog = after || null;
    game.state = STATE.DIALOG;
  }
  function advanceDialog() {
    dialog.index++;
    if (dialog.index >= dialog.lines.length) {
      dialog.active = false;
      const cb = game.afterDialog;
      game.afterDialog = null;
      if (game.state === STATE.DIALOG) game.state = STATE.WORLD;
      if (cb) cb();
    }
  }

  // ---- Input ----------------------------------------------------------------
  const keys = new Set();
  const KEYMAP = {
    ArrowUp: "up", KeyW: "up", ArrowDown: "down", KeyS: "down",
    ArrowLeft: "left", KeyA: "left", ArrowRight: "right", KeyD: "right",
    KeyZ: "action", Enter: "action", Space: "action",
    KeyX: "cancel", Escape: "cancel", Backspace: "cancel",
  };
  window.addEventListener("keydown", (e) => {
    const a = KEYMAP[e.code];
    if (!a) return;
    e.preventDefault();
    if (["up", "down", "left", "right"].includes(a)) keys.add(a);
    if (!e.repeat) onPress(a);
  });
  window.addEventListener("keyup", (e) => {
    const a = KEYMAP[e.code];
    if (a && ["up", "down", "left", "right"].includes(a)) keys.delete(a);
  });

  function onPress(a) {
    switch (game.state) {
      case STATE.TITLE:
        if (a === "action") startGame();
        break;
      case STATE.DIALOG:
        if (a === "action" || a === "cancel") advanceDialog();
        break;
      case STATE.WORLD:
        if (a === "action") interact();
        break;
      case STATE.BATTLE:
        battleInput(a);
        break;
      case STATE.CREDITS:
        break;
    }
  }

  function startGame() {
    game.state = STATE.WORLD;
    say([
      "You wake up in your room in Willow Town.",
      "Today is the day you receive your first Shapemon!",
      "Head to Prof. Cedar's lab to the east, then travel",
      "north to challenge the Fernwood Gym.",
    ]);
  }

  // ---- Overworld movement ---------------------------------------------------
  function tryMove(dir) {
    player.dir = dir;
    const [dx, dy] = DIRV[dir];
    const nx = player.x + dx, ny = player.y + dy;
    if (isBlocked(player.map, nx, ny)) return; // turn only
    player.moving = true;
    player.from = { x: player.x, y: player.y };
    player.to = { x: nx, y: ny };
    player.progress = 0;
  }

  function finishMove() {
    player.x = player.to.x;
    player.y = player.to.y;
    player.px = player.x * TILE;
    player.py = player.y * TILE;
    player.moving = false;

    const key = `${player.map}:${player.x},${player.y}`;
    if (WARPS[key]) { doWarp(WARPS[key]); return; }

    if (tileAt(MAPS[player.map], player.x, player.y) === ":" && flags.hasStarter) {
      if (Math.random() < ENCOUNTER_RATE) startWildBattle();
    }
  }

  function doWarp(w) {
    player.map = w.map;
    player.x = w.x; player.y = w.y; player.dir = w.dir;
    player.px = w.x * TILE; player.py = w.y * TILE;
    player.moving = false;
  }

  function interact() {
    const [dx, dy] = DIRV[player.dir];
    const fx = player.x + dx, fy = player.y + dy;
    const npc = npcAt(player.map, fx, fy);
    if (!npc) return;

    if (npc.role === "prof") {
      if (!flags.hasStarter) {
        say([
          "Prof. Cedar: Ah, there you are!",
          "Three Shapemon rest on my table. As arranged,",
          "your partner is the Fire-type: Emberling!",
          "Emberling joined your party!",
          "Now go earn the Leaf Badge at Fernwood Gym, north of town.",
        ], () => {
          flags.hasStarter = true;
          player.party = [makeCreature("emberling", 5)];
        });
      } else {
        say(["Prof. Cedar: Emberling's flame looks strong. Go get that badge!"]);
      }
    } else if (npc.role === "gym") {
      if (!flags.hasStarter) {
        say(["Leader Fern: A trainer with no Shapemon? Come back when you're ready."]);
      } else if (!flags.gymBadge) {
        say([
          "Leader Fern: Welcome to Fernwood Gym!",
          "My Grass-types have deep roots. Show me your fire!",
        ], () => startGymBattle());
      } else {
        say(["Leader Fern: The Leaf Badge suits you. Well fought!"]);
      }
    }
  }

  // ---- Battle ---------------------------------------------------------------
  const battle = {
    kind: "wild",           // "wild" | "gym"
    enemy: null,
    ally: null,
    phase: "intro",         // intro | menu | moves | anim | result
    menuIndex: 0,
    moveIndex: 0,
    msg: [], msgIndex: 0,
    afterMsg: null,
    onWin: null,
  };

  function battleSay(lines, after) {
    battle.msg = Array.isArray(lines) ? lines : [lines];
    battle.msgIndex = 0;
    battle.afterMsg = after || null;
    battle.phase = "anim";
  }

  function startWildBattle() {
    const pool = ["wormling", "nibbit"];
    const id = pool[Math.floor(Math.random() * pool.length)];
    const lvl = 2 + Math.floor(Math.random() * 3);
    enterBattle("wild", makeCreature(id, lvl), null);
    battleSay([`A wild ${battle.enemy.name} appeared!`], () => { battle.phase = "menu"; });
  }

  function startGymBattle() {
    enterBattle("gym", makeCreature("thornbud", 6), () => {
      flags.gymBadge = true;
      game.state = STATE.CREDITS;
    });
    battleSay([
      "Leader Fern sent out Thornbud!",
      `Go, ${battle.ally.name}!`,
    ], () => { battle.phase = "menu"; });
  }

  function enterBattle(kind, enemy, onWin) {
    battle.kind = kind;
    battle.enemy = enemy;
    battle.ally = player.party[0];
    battle.onWin = onWin;
    battle.phase = "intro";
    battle.menuIndex = 0;
    battle.moveIndex = 0;
    game.state = STATE.BATTLE;
  }

  function battleInput(a) {
    if (battle.phase === "anim") {
      if (a === "action" || a === "cancel") {
        battle.msgIndex++;
        if (battle.msgIndex >= battle.msg.length) {
          const cb = battle.afterMsg;
          battle.afterMsg = null;
          if (cb) cb();
        }
      }
      return;
    }
    if (battle.phase === "menu") {
      const opts = battle.kind === "wild" ? 2 : 1; // FIGHT / RUN(wild only)
      if (a === "up" || a === "down") battle.menuIndex = (battle.menuIndex + 1) % opts;
      if (a === "action") {
        if (battle.menuIndex === 0) { battle.phase = "moves"; battle.moveIndex = 0; }
        else { tryRun(); }
      }
      return;
    }
    if (battle.phase === "moves") {
      const n = battle.ally.moves.length;
      if (a === "down") battle.moveIndex = (battle.moveIndex + 1) % n;
      if (a === "up") battle.moveIndex = (battle.moveIndex - 1 + n) % n;
      if (a === "cancel") { battle.phase = "menu"; return; }
      if (a === "action") doTurn(battle.ally.moves[battle.moveIndex]);
      return;
    }
  }

  function tryRun() {
    if (battle.kind !== "wild") return;
    battleSay(["Got away safely!"], () => { game.state = STATE.WORLD; });
  }

  function calcDamage(attacker, defender, move) {
    const base = Math.floor(((2 * attacker.level / 5 + 2) * move.power * (attacker.atk / defender.def)) / 50) + 2;
    const eff = typeEff(move.type, defender.type);
    const rand = 0.85 + Math.random() * 0.15;
    return { dmg: Math.max(1, Math.floor(base * eff * rand)), eff };
  }

  function effWord(eff) {
    if (eff >= 2) return "It's super effective!";
    if (eff <= 0.5) return "It's not very effective...";
    return null;
  }

  function doTurn(playerMove) {
    // Determine order by speed (player first on tie).
    const ally = battle.ally, enemy = battle.enemy;
    const enemyMove = enemy.moves[Math.floor(Math.random() * enemy.moves.length)];
    const allyFirst = ally.spd >= enemy.spd;

    const queue = [];
    const attack = (att, def, move, isAlly) => {
      const { dmg, eff } = calcDamage(att, def, move);
      def.hp = Math.max(0, def.hp - dmg);
      queue.push(`${att.name} used ${move.name}!`);
      const w = effWord(eff);
      if (w) queue.push(w);
    };

    const order = allyFirst
      ? [["ally", ally, enemy, playerMove], ["enemy", enemy, ally, enemyMove]]
      : [["enemy", enemy, ally, enemyMove], ["ally", ally, enemy, playerMove]];

    // Execute step by step, stopping if someone faints.
    for (const [who, att, def, move] of order) {
      if (att.hp <= 0) continue;
      attack(att, def, move, who === "ally");
      if (def.hp <= 0) {
        queue.push(`${def.name} fainted!`);
        break;
      }
    }

    battle.phase = "moves"; // will be overridden by battleSay
    battleSay(queue, resolveTurn);
  }

  function resolveTurn() {
    const ally = battle.ally, enemy = battle.enemy;
    if (enemy.hp <= 0) {
      const win = () => {
        // Reward: level up and full heal so the story stays winnable.
        ally.level += 1;
        const healed = makeCreature(ally.id, ally.level);
        ally.maxhp = healed.maxhp; ally.hp = healed.maxhp;
        ally.atk = healed.atk; ally.def = healed.def; ally.spd = healed.spd;
        if (battle.onWin) battle.onWin();
        else game.state = STATE.WORLD;
      };
      battleSay([`${ally.name} grew to Lv.${ally.level + 1}!`], win);
      return;
    }
    if (ally.hp <= 0) {
      battleSay([
        `${ally.name} fainted!`,
        "You scramble back home to recover...",
      ], () => {
        ally.hp = ally.maxhp;
        doWarp({ map: "home", x: 4, y: 3, dir: "down" });
        game.state = STATE.WORLD;
      });
      return;
    }
    battle.phase = "menu";
    battle.menuIndex = 0;
  }

  // ---- Update loop ----------------------------------------------------------
  function update() {
    game.tick++;
    if (game.state === STATE.WORLD && !player.moving && !dialog.active) {
      for (const d of ["up", "down", "left", "right"]) {
        if (keys.has(d)) { tryMove(d); break; }
      }
    }
    if (player.moving) {
      player.progress++;
      const t = player.progress / MOVE_FRAMES;
      player.px = (player.from.x + (player.to.x - player.from.x) * t) * TILE;
      player.py = (player.from.y + (player.to.y - player.from.y) * t) * TILE;
      if (player.progress >= MOVE_FRAMES) finishMove();
    }
  }

  // ---- Rendering: world -----------------------------------------------------
  function camera() {
    const map = MAPS[player.map];
    const mapPxW = map.w * TS, mapPxH = map.h * TS;
    let cx = player.px * SCALE + TS / 2 - canvas.width / 2;
    let cy = player.py * SCALE + TS / 2 - canvas.height / 2;
    if (mapPxW <= canvas.width) cx = (mapPxW - canvas.width) / 2;
    else cx = Math.max(0, Math.min(cx, mapPxW - canvas.width));
    if (mapPxH <= canvas.height) cy = (mapPxH - canvas.height) / 2;
    else cy = Math.max(0, Math.min(cy, mapPxH - canvas.height));
    return { cx, cy };
  }

  function drawTile(ch, sx, sy) {
    // base ground under everything
    ctx.fillStyle = C.grass;
    ctx.fillRect(sx, sy, TS, TS);
    switch (ch) {
      case "T":
        ctx.fillStyle = C.grassDark; ctx.fillRect(sx, sy, TS, TS);
        ctx.fillStyle = C.treeTrunk; ctx.fillRect(sx + TS * 0.42, sy + TS * 0.55, TS * 0.16, TS * 0.4);
        ctx.fillStyle = C.tree;
        ctx.beginPath(); ctx.arc(sx + TS / 2, sy + TS * 0.42, TS * 0.38, 0, Math.PI * 2); ctx.fill();
        break;
      case "_":
        ctx.fillStyle = C.pathEdge; ctx.fillRect(sx, sy, TS, TS);
        ctx.fillStyle = C.path; ctx.fillRect(sx + 2, sy + 2, TS - 4, TS - 4);
        break;
      case ":":
        ctx.fillStyle = C.tall; ctx.fillRect(sx, sy, TS, TS);
        ctx.fillStyle = C.tallHi;
        for (let i = 0; i < 4; i++)
          ctx.fillRect(sx + 4 + i * 10, sy + TS - 14, 4, 12);
        break;
      case "~":
        ctx.fillStyle = C.water; ctx.fillRect(sx, sy, TS, TS);
        break;
      case "#":
        ctx.fillStyle = C.roof; ctx.fillRect(sx, sy, TS, TS);
        ctx.fillStyle = C.roofDark; ctx.fillRect(sx, sy, TS, TS * 0.3);
        break;
      case "H": case "L": case "G":
        ctx.fillStyle = C.roofDark; ctx.fillRect(sx, sy, TS, TS);
        ctx.fillStyle = C.door; ctx.fillRect(sx + TS * 0.28, sy + TS * 0.2, TS * 0.44, TS * 0.8);
        ctx.fillStyle = "#ffd966"; ctx.fillRect(sx + TS * 0.6, sy + TS * 0.55, 4, 4);
        break;
      case "=":
        ctx.fillStyle = C.iwall; ctx.fillRect(sx, sy, TS, TS);
        ctx.fillStyle = "#7d6549"; ctx.fillRect(sx, sy, TS, TS * 0.25);
        break;
      case "F":
        ctx.fillStyle = C.floor; ctx.fillRect(sx, sy, TS, TS);
        ctx.strokeStyle = C.floorLine; ctx.strokeRect(sx + 0.5, sy + 0.5, TS - 1, TS - 1);
        break;
      case "D":
        ctx.fillStyle = C.floor; ctx.fillRect(sx, sy, TS, TS);
        ctx.fillStyle = C.door; ctx.fillRect(sx + TS * 0.2, sy, TS * 0.6, TS);
        break;
      default: // '.' grass
        ctx.fillStyle = C.grass; ctx.fillRect(sx, sy, TS, TS);
        ctx.fillStyle = C.grassDark; ctx.fillRect(sx + 6, sy + 10, 3, 3);
    }
  }

  function drawCreatureSprite(cr, cx, cy, r) {
    ctx.fillStyle = cr.color;
    switch (cr.shape) {
      case "flame":
        ctx.beginPath();
        ctx.moveTo(cx, cy - r);
        ctx.quadraticCurveTo(cx + r, cy, cx, cy + r);
        ctx.quadraticCurveTo(cx - r, cy, cx, cy - r);
        ctx.fill();
        ctx.fillStyle = "#ffd23f";
        ctx.beginPath(); ctx.arc(cx, cy + r * 0.2, r * 0.4, 0, Math.PI * 2); ctx.fill();
        break;
      case "blob":
        ctx.beginPath(); ctx.ellipse(cx, cy, r, r * 0.8, 0, 0, Math.PI * 2); ctx.fill();
        break;
      case "spike":
        ctx.beginPath();
        for (let i = 0; i < 8; i++) {
          const ang = (i / 8) * Math.PI * 2;
          const rad = i % 2 === 0 ? r : r * 0.6;
          const fn = i === 0 ? "moveTo" : "lineTo";
          ctx[fn](cx + Math.cos(ang) * rad, cy + Math.sin(ang) * rad);
        }
        ctx.closePath(); ctx.fill();
        break;
      case "round": default:
        ctx.beginPath(); ctx.arc(cx, cy, r, 0, Math.PI * 2); ctx.fill();
    }
    // eyes
    ctx.fillStyle = "#101427";
    ctx.beginPath(); ctx.arc(cx - r * 0.3, cy - r * 0.1, r * 0.12, 0, Math.PI * 2); ctx.fill();
    ctx.beginPath(); ctx.arc(cx + r * 0.3, cy - r * 0.1, r * 0.12, 0, Math.PI * 2); ctx.fill();
  }

  function drawPlayer(sx, sy) {
    // body (rounded rect) + head circle; facing marker
    ctx.fillStyle = C.player;
    ctx.fillRect(sx + TS * 0.22, sy + TS * 0.35, TS * 0.56, TS * 0.55);
    ctx.fillStyle = C.playerHi;
    ctx.beginPath(); ctx.arc(sx + TS / 2, sy + TS * 0.32, TS * 0.24, 0, Math.PI * 2); ctx.fill();
    // facing dot
    ctx.fillStyle = "#101427";
    const [dx, dy] = DIRV[player.dir];
    ctx.beginPath();
    ctx.arc(sx + TS / 2 + dx * TS * 0.16, sy + TS * 0.32 + dy * TS * 0.14, 3, 0, Math.PI * 2);
    ctx.fill();
  }

  function drawNPC(n, sx, sy) {
    ctx.fillStyle = n.color;
    ctx.fillRect(sx + TS * 0.22, sy + TS * 0.35, TS * 0.56, TS * 0.55);
    ctx.beginPath(); ctx.arc(sx + TS / 2, sy + TS * 0.3, TS * 0.22, 0, Math.PI * 2); ctx.fill();
    ctx.fillStyle = "#101427";
    ctx.beginPath(); ctx.arc(sx + TS * 0.44, sy + TS * 0.3, 2.5, 0, Math.PI * 2); ctx.fill();
    ctx.beginPath(); ctx.arc(sx + TS * 0.56, sy + TS * 0.3, 2.5, 0, Math.PI * 2); ctx.fill();
  }

  function renderWorld() {
    const map = MAPS[player.map];
    const { cx, cy } = camera();
    ctx.fillStyle = "#000"; ctx.fillRect(0, 0, canvas.width, canvas.height);

    const x0 = Math.floor(cx / TS), y0 = Math.floor(cy / TS);
    for (let ty = y0; ty <= y0 + VIEW_H + 1; ty++) {
      for (let tx = x0; tx <= x0 + VIEW_W + 1; tx++) {
        if (tx < 0 || ty < 0 || tx >= map.w || ty >= map.h) continue;
        drawTile(map.grid[ty][tx], tx * TS - cx, ty * TS - cy);
      }
    }
    for (const n of (NPCS[player.map] || [])) drawNPC(n, n.x * TS - cx, n.y * TS - cy);
    drawPlayer(player.px * SCALE - cx, player.py * SCALE - cy);
  }

  // ---- Rendering: text box --------------------------------------------------
  function wrap(text, maxW) {
    const words = text.split(" ");
    const lines = []; let cur = "";
    for (const w of words) {
      const test = cur ? cur + " " + w : w;
      if (ctx.measureText(test).width > maxW && cur) { lines.push(cur); cur = w; }
      else cur = test;
    }
    if (cur) lines.push(cur);
    return lines;
  }

  function drawBox(x, y, w, h) {
    ctx.fillStyle = C.box; ctx.fillRect(x, y, w, h);
    ctx.strokeStyle = C.boxBorder; ctx.lineWidth = 3;
    ctx.strokeRect(x + 2, y + 2, w - 4, h - 4);
  }

  function renderDialogBox(text) {
    const bx = 12, bh = 108, by = canvas.height - bh - 12, bw = canvas.width - 24;
    drawBox(bx, by, bw, bh);
    ctx.fillStyle = C.text;
    ctx.font = "18px 'Courier New', monospace";
    ctx.textBaseline = "top";
    const lines = wrap(text, bw - 40);
    lines.slice(0, 3).forEach((ln, i) => ctx.fillText(ln, bx + 20, by + 18 + i * 26));
    ctx.fillStyle = "#8fb6ff";
    ctx.font = "14px 'Courier New', monospace";
    ctx.fillText("▼ Z / Enter", bx + bw - 130, by + bh - 26);
  }

  // ---- Rendering: battle ----------------------------------------------------
  function drawHPBar(x, y, cr) {
    ctx.fillStyle = C.text;
    ctx.font = "16px 'Courier New', monospace";
    ctx.textBaseline = "top";
    ctx.fillText(`${cr.name}  Lv.${cr.level}`, x, y);
    const bw = 180, bh = 12, by = y + 22;
    ctx.fillStyle = "#2b2d4f"; ctx.fillRect(x, by, bw, bh);
    const frac = Math.max(0, cr.hp / cr.maxhp);
    ctx.fillStyle = frac > 0.5 ? C.hpGreen : frac > 0.2 ? C.hpYellow : C.hpRed;
    ctx.fillRect(x, by, bw * frac, bh);
    ctx.strokeStyle = C.text; ctx.lineWidth = 2; ctx.strokeRect(x, by, bw, bh);
    ctx.fillStyle = C.text; ctx.font = "13px 'Courier New', monospace";
    ctx.fillText(`${cr.hp}/${cr.maxhp}`, x + bw - 60, by + 16);
  }

  function renderBattle() {
    // background
    const g = ctx.createLinearGradient(0, 0, 0, canvas.height);
    g.addColorStop(0, "#cfe8ff"); g.addColorStop(1, "#9fd0a8");
    ctx.fillStyle = g; ctx.fillRect(0, 0, canvas.width, canvas.height);

    // enemy platform + sprite (top-right)
    const ex = canvas.width - 130, ey = 120;
    ctx.fillStyle = "rgba(0,0,0,0.12)";
    ctx.beginPath(); ctx.ellipse(ex, ey + 46, 70, 20, 0, 0, Math.PI * 2); ctx.fill();
    drawCreatureSprite(battle.enemy, ex, ey, 40);
    drawHPBar(24, 30, battle.enemy);

    // ally platform + sprite (bottom-left)
    const ax = 120, ay = 250;
    ctx.fillStyle = "rgba(0,0,0,0.12)";
    ctx.beginPath(); ctx.ellipse(ax, ay + 52, 80, 22, 0, 0, Math.PI * 2); ctx.fill();
    drawCreatureSprite(battle.ally, ax, ay, 50);
    drawHPBar(canvas.width - 230, 210, battle.ally);

    // bottom UI
    const bx = 12, bh = 120, by = canvas.height - bh - 12, bw = canvas.width - 24;
    drawBox(bx, by, bw, bh);
    ctx.fillStyle = C.text;
    ctx.font = "18px 'Courier New', monospace";
    ctx.textBaseline = "top";

    if (battle.phase === "anim" || battle.phase === "intro") {
      const line = battle.msg[Math.min(battle.msgIndex, battle.msg.length - 1)] || "";
      wrap(line, bw - 40).slice(0, 3).forEach((ln, i) => ctx.fillText(ln, bx + 20, by + 20 + i * 26));
      ctx.fillStyle = "#8fb6ff"; ctx.font = "14px 'Courier New', monospace";
      ctx.fillText("▼ Z", bx + bw - 60, by + bh - 26);
    } else if (battle.phase === "menu") {
      ctx.fillText("What will you do?", bx + 20, by + 16);
      const opts = battle.kind === "wild" ? ["FIGHT", "RUN"] : ["FIGHT"];
      opts.forEach((o, i) => {
        ctx.fillStyle = i === battle.menuIndex ? "#ffd23f" : C.text;
        ctx.fillText((i === battle.menuIndex ? "▶ " : "  ") + o, bx + 40, by + 52 + i * 28);
      });
    } else if (battle.phase === "moves") {
      battle.ally.moves.forEach((m, i) => {
        ctx.fillStyle = i === battle.moveIndex ? "#ffd23f" : C.text;
        ctx.fillText((i === battle.moveIndex ? "▶ " : "  ") + m.name, bx + 30, by + 18 + i * 28);
        ctx.fillStyle = TYPE_COLOR[m.type];
        ctx.font = "13px 'Courier New', monospace";
        ctx.fillText(m.type.toUpperCase(), bx + bw - 130, by + 22 + i * 28);
        ctx.font = "18px 'Courier New', monospace";
      });
      ctx.fillStyle = "#8fb6ff"; ctx.font = "14px 'Courier New', monospace";
      ctx.fillText("X = back", bx + bw - 110, by + bh - 26);
    }
  }

  // ---- Rendering: title / credits ------------------------------------------
  function renderTitle() {
    const g = ctx.createLinearGradient(0, 0, 0, canvas.height);
    g.addColorStop(0, "#1a1c3a"); g.addColorStop(1, "#3a1c2a");
    ctx.fillStyle = g; ctx.fillRect(0, 0, canvas.width, canvas.height);

    // decorative starter
    drawCreatureSprite(makeCreature("emberling", 5), canvas.width / 2, 150, 54);

    ctx.fillStyle = "#ff7a33";
    ctx.font = "bold 40px 'Courier New', monospace";
    ctx.textAlign = "center"; ctx.textBaseline = "middle";
    ctx.fillText("SHAPEMON", canvas.width / 2, 250);
    ctx.fillStyle = "#f4f4ec";
    ctx.font = "22px 'Courier New', monospace";
    ctx.fillText("Ember Quest", canvas.width / 2, 288);

    if (Math.floor(game.tick / 30) % 2 === 0) {
      ctx.fillStyle = "#8fb6ff";
      ctx.font = "18px 'Courier New', monospace";
      ctx.fillText("Press Z / Enter to start", canvas.width / 2, 360);
    }
    ctx.textAlign = "left";
  }

  const CREDITS = [
    "THANK YOU FOR PLAYING!",
    "",
    "You earned the Leaf Badge and",
    "cleared Fernwood Gym.",
    "",
    "Shapemon — Ember Quest",
    "An original Gen-1-style demo",
    "",
    "Design & Code .... you + Claude",
    "Engine .......... HTML5 Canvas",
    "Creatures ....... Emberling, Wormling,",
    "                  Nibbit, Thornbud",
    "",
    "~ The End ~",
  ];

  function renderCredits() {
    const g = ctx.createLinearGradient(0, 0, 0, canvas.height);
    g.addColorStop(0, "#101427"); g.addColorStop(1, "#2a1030");
    ctx.fillStyle = g; ctx.fillRect(0, 0, canvas.width, canvas.height);
    drawCreatureSprite(makeCreature("emberling", 6), canvas.width / 2, 70, 40);
    ctx.textAlign = "center"; ctx.textBaseline = "top";
    CREDITS.forEach((line, i) => {
      ctx.fillStyle = i === 0 ? "#ffd23f" : "#f4f4ec";
      ctx.font = (i === 0 ? "bold 20px" : "15px") + " 'Courier New', monospace";
      ctx.fillText(line, canvas.width / 2, 130 + i * 22);
    });
    ctx.textAlign = "left";
  }

  // ---- Main loop ------------------------------------------------------------
  function render() {
    if (game.state === STATE.TITLE) { renderTitle(); return; }
    if (game.state === STATE.CREDITS) { renderCredits(); return; }
    if (game.state === STATE.BATTLE) { renderBattle(); return; }
    renderWorld();
    if (game.state === STATE.DIALOG && dialog.active) {
      renderDialogBox(dialog.lines[dialog.index] || "");
    }
  }

  function frame() {
    update();
    render();
    requestAnimationFrame(frame);
  }

  // Expose a tiny hook so the screenshot harness can drive the game headlessly.
  window.__shapemon = {
    game, player, flags, battle, STATE, onPress, makeCreature,
    warpTo: doWarp,
    giveStarter: () => { flags.hasStarter = true; player.party = [makeCreature("emberling", 5)]; },
    startGymBattle, startWildBattle, doTurn,
  };

  requestAnimationFrame(frame);
})();
