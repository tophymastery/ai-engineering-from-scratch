// @shop/factories — TypeScript mirror of libs/factories (S-T7, 03 §3).
//
// Same entity shapes, same default values, same override-via-options ergonomics
// as the Go library, driven by an injected seeded RNG so BFF/Jest tests are
// reproducible. Zero runtime dependencies — plain TS. It compiles under `tsc`
// once BFF tooling lands (see README); until then it is source-of-truth-adjacent
// docs + code kept in lockstep with the Go factories.
//
// NOTE ON DETERMINISM: within TypeScript, `New(seed)` is byte-reproducible. It is
// NOT required to be byte-identical to the Go output — the Go `seedctl` is the
// single canonical dataset generator (03 §3). The shared contract here is the
// SHAPE and DEFAULTS, so a BFF written against these types matches services fed
// by the Go factories.

// ---- seeded RNG (mulberry32): deterministic per seed ----
export function rng(seed: number): () => number {
  let a = seed >>> 0;
  return function () {
    a |= 0;
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";

// ---- entity types (snake_case wire fields, mirroring 02 §1) ----
export interface Money { amount: number; currency: string; }
export interface User { id: string; name: string; email: string; phone: string; region: string; created_at: string; }
export interface Merchant { id: string; name: string; region: string; cuisine: string; rating_x10: number; online: boolean; created_at: string; }
export interface MenuItem { id: string; merchant_id: string; name: string; price: Money; available: boolean; created_at: string; }
export interface CartLine { menu_item_id: string; qty: number; }
export interface Cart { id: string; user_id: string; merchant_id: string; lines: CartLine[]; created_at: string; }
export interface Order { id: string; user_id: string; merchant_id: string; driver_id?: string; status: string; total: Money; region: string; created_at: string; }
export interface Driver { id: string; name: string; region: string; vehicle: string; online: boolean; created_at: string; }

const CUISINES = ["Thai", "Japanese", "Italian", "Indian", "Burger", "Vegan", "Dessert"];
const VEHICLES = ["MOTORCYCLE", "BICYCLE", "CAR"];

export interface FactoryOptions { region?: string; currency?: string; startAtMs?: number; }

// Factory mirrors the Go builder: one method per core entity, defaults + overrides.
export class Factory {
  private next: () => number;
  private n = 0;
  private tick = 0;
  private ms: number;
  private readonly region: string;
  private readonly currency: string;
  private readonly t0: number;

  constructor(seed: number, opts: FactoryOptions = {}) {
    this.next = rng(seed);
    this.region = opts.region ?? "bkk";
    this.currency = opts.currency ?? "THB";
    this.t0 = opts.startAtMs ?? Date.UTC(2026, 6, 11, 2, 15, 0);
    this.ms = this.t0;
  }

  private int(max: number): number { return Math.floor(this.next() * max); }

  private stamp(): string {
    const t = new Date(this.t0 + this.tick * 60_000);
    this.tick++;
    return t.toISOString().replace(/\.\d{3}Z$/, "Z");
  }

  // logicalShard mirrors libs/sharding: fmix64(fnv1a64(key)) % 256, done in BigInt.
  private logicalShard(key: string): number {
    const MASK = (1n << 64n) - 1n;
    let h = 1469598103934665603n; // FNV offset basis
    for (let i = 0; i < key.length; i++) {
      h = (h ^ BigInt(key.charCodeAt(i))) & MASK;
      h = (h * 1099511628211n) & MASK; // FNV prime
    }
    // murmur3 fmix64
    h = (h ^ (h >> 33n)) & MASK;
    h = (h * 0xff51afd7ed558ccdn) & MASK;
    h = (h ^ (h >> 33n)) & MASK;
    h = (h * 0xc4ceb9fe1a85ec53n) & MASK;
    h = (h ^ (h >> 33n)) & MASK;
    return Number(h % 256n);
  }

  private id(prefix: string): string {
    this.n++;
    const shard = this.logicalShard(`${prefix}:${this.n}`);
    const hh = shard.toString(16).padStart(2, "0");
    // 26-char pseudo-Crockford body from the seeded RNG (see determinism note).
    let body = "";
    for (let i = 0; i < 26; i++) body += CROCKFORD[this.int(32)];
    this.ms++;
    return `${prefix}_${hh}${body}`;
  }

  user(over: Partial<User> = {}): User {
    return { id: this.id("usr"), name: `Customer ${this.n}`, email: `user${this.n}@example.test`,
      phone: `+66${100000000 + this.int(899999999)}`, region: this.region, created_at: this.stamp(), ...over };
  }
  merchant(over: Partial<Merchant> = {}): Merchant {
    return { id: this.id("mer"), name: `Merchant ${this.n}`, region: this.region,
      cuisine: CUISINES[this.int(CUISINES.length)], rating_x10: 30 + this.int(21), online: true, created_at: this.stamp(), ...over };
  }
  menuItem(over: Partial<MenuItem> = {}): MenuItem {
    return { id: this.id("itm"), merchant_id: "", name: `Item ${this.n}`,
      price: { amount: 2000 + this.int(48000), currency: this.currency }, available: true, created_at: this.stamp(), ...over };
  }
  cart(over: Partial<Cart> = {}): Cart {
    return { id: this.id("crt"), user_id: "", merchant_id: "", lines: [], created_at: this.stamp(), ...over };
  }
  order(over: Partial<Order> = {}): Order {
    return { id: this.id("ord"), user_id: "", merchant_id: "", status: "PAYMENT_PENDING",
      total: { amount: 5000 + this.int(95000), currency: this.currency }, region: this.region, created_at: this.stamp(), ...over };
  }
  driver(over: Partial<Driver> = {}): Driver {
    return { id: this.id("drv"), name: `Driver ${this.n}`, region: this.region,
      vehicle: VEHICLES[this.int(VEHICLES.length)], online: true, created_at: this.stamp(), ...over };
  }
}

export function New(seed: number, opts: FactoryOptions = {}): Factory {
  return new Factory(seed, opts);
}
