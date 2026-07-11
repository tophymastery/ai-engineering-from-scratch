// Package factories is the S-T7 typed test-data builder library (03 §3): one
// factory per core entity, sensible defaults with functional overrides, so tests
// and seedctl never hand-roll JSON. Every ID is a platform shard-hint ULID
// (usr_, mer_, itm_, crt_, ord_, drv_) minted deterministically from an injected
// seeded RNG, so the SAME SEED PRODUCES BYTE-IDENTICAL ENTITIES on every run and
// in every process — the repeatability contract of 03 §4.
//
// A TypeScript mirror lives at bffs/factories-ts/ with the identical defaults.
//
// Usage:
//
//	f := factories.New(42, factories.Region("bkk"))
//	u := f.User()
//	o := f.Order(factories.WithStatus("DELIVERED"), factories.WithMerchant(m.ID))
package factories

import (
	"fmt"
	"math/rand"
	"time"
)

// Factory is a deterministic entity builder. NOT safe for concurrent use: seeded
// determinism requires a single, ordered sequence of draws. Create one per
// goroutine/scenario.
type Factory struct {
	rnd      *rand.Rand
	t0       time.Time
	ms       uint64 // monotonic ULID millisecond counter
	n        int    // entity counter (shard-key source)
	region   string
	currency string
	step     time.Duration
	tick     int64
}

// Option configures a Factory.
type Option func(*Factory)

// Region sets the default region for built entities (default "bkk").
func Region(r string) Option { return func(f *Factory) { f.region = r } }

// Currency sets the default money currency (default "THB").
func Currency(c string) Option { return func(f *Factory) { f.currency = c } }

// StartAt sets the deterministic clock origin (default 2026-07-11T02:15:00Z).
func StartAt(t time.Time) Option { return func(f *Factory) { f.t0 = t } }

// New builds a Factory seeded by seed. Identical seed + option set ⇒ identical
// entity stream.
func New(seed int64, opts ...Option) *Factory {
	f := &Factory{
		rnd:      rand.New(rand.NewSource(seed)),
		t0:       time.Date(2026, 7, 11, 2, 15, 0, 0, time.UTC),
		region:   "bkk",
		currency: "THB",
		step:     time.Minute,
	}
	for _, o := range opts {
		o(f)
	}
	f.ms = uint64(f.t0.UnixMilli())
	return f
}

// stamp returns the next deterministic RFC 3339 UTC timestamp.
func (f *Factory) stamp() string {
	t := f.t0.Add(time.Duration(f.tick) * f.step)
	f.tick++
	return t.UTC().Format(time.RFC3339)
}

// --- entity types (snake_case wire fields per 02 §1) ---

// Money is the 02 §1 integer-minor-unit money value.
type Money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// User is a customer/account.
type User struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
	Region    string `json:"region"`
	CreatedAt string `json:"created_at"`
}

// Merchant is a store/restaurant.
type Merchant struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Region    string `json:"region"`
	Cuisine   string `json:"cuisine"`
	RatingX10 int    `json:"rating_x10"` // integer tenths, e.g. 45 = 4.5 stars
	Online    bool   `json:"online"`
	CreatedAt string `json:"created_at"`
}

// MenuItem is a purchasable item on a merchant's menu.
type MenuItem struct {
	ID         string `json:"id"`
	MerchantID string `json:"merchant_id"`
	Name       string `json:"name"`
	Price      Money  `json:"price"`
	Available  bool   `json:"available"`
	CreatedAt  string `json:"created_at"`
}

// CartLine is one item + quantity in a cart.
type CartLine struct {
	MenuItemID string `json:"menu_item_id"`
	Qty        int    `json:"qty"`
}

// Cart is an in-progress basket.
type Cart struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	MerchantID string     `json:"merchant_id"`
	Lines      []CartLine `json:"lines"`
	CreatedAt  string     `json:"created_at"`
}

// Order is a placed order.
type Order struct {
	ID         string `json:"id"`
	UserID     string `json:"user_id"`
	MerchantID string `json:"merchant_id"`
	DriverID   string `json:"driver_id,omitempty"`
	Status     string `json:"status"`
	Total      Money  `json:"total"`
	Region     string `json:"region"`
	CreatedAt  string `json:"created_at"`
}

// Driver is a delivery driver.
type Driver struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Region    string `json:"region"`
	Vehicle   string `json:"vehicle"`
	Online    bool   `json:"online"`
	CreatedAt string `json:"created_at"`
}

// --- per-entity option types + builders ---

// UserOpt overrides a User default.
type UserOpt func(*User)

// WithUserRegion overrides a user's region.
func WithUserRegion(r string) UserOpt { return func(u *User) { u.Region = r } }

// WithUserName overrides a user's display name.
func WithUserName(n string) UserOpt { return func(u *User) { u.Name = n } }

// User builds a customer.
func (f *Factory) User(opts ...UserOpt) User {
	id := f.newID("usr")
	u := User{
		ID:        id,
		Name:      fmt.Sprintf("Customer %d", f.n),
		Email:     fmt.Sprintf("user%d@example.test", f.n),
		Phone:     fmt.Sprintf("+66%09d", 100000000+f.rnd.Intn(899999999)),
		Region:    f.region,
		CreatedAt: f.stamp(),
	}
	for _, o := range opts {
		o(&u)
	}
	return u
}

// MerchantOpt overrides a Merchant default.
type MerchantOpt func(*Merchant)

// WithMerchantRegion overrides a merchant's region.
func WithMerchantRegion(r string) MerchantOpt { return func(m *Merchant) { m.Region = r } }

// WithMerchantOnline sets a merchant's online flag.
func WithMerchantOnline(v bool) MerchantOpt { return func(m *Merchant) { m.Online = v } }

var cuisines = []string{"Thai", "Japanese", "Italian", "Indian", "Burger", "Vegan", "Dessert"}

// Merchant builds a store.
func (f *Factory) Merchant(opts ...MerchantOpt) Merchant {
	m := Merchant{
		ID:        f.newID("mer"),
		Name:      fmt.Sprintf("Merchant %d", f.n),
		Region:    f.region,
		Cuisine:   cuisines[f.rnd.Intn(len(cuisines))],
		RatingX10: 30 + f.rnd.Intn(21), // 3.0 .. 5.0
		Online:    true,
		CreatedAt: f.stamp(),
	}
	for _, o := range opts {
		o(&m)
	}
	return m
}

// MenuItemOpt overrides a MenuItem default.
type MenuItemOpt func(*MenuItem)

// WithMenuMerchant sets the owning merchant id.
func WithMenuMerchant(id string) MenuItemOpt { return func(mi *MenuItem) { mi.MerchantID = id } }

// WithMenuPrice overrides the price (minor units).
func WithMenuPrice(minor int64) MenuItemOpt {
	return func(mi *MenuItem) { mi.Price.Amount = minor }
}

// MenuItem builds a menu item.
func (f *Factory) MenuItem(opts ...MenuItemOpt) MenuItem {
	mi := MenuItem{
		ID:        f.newID("itm"),
		Name:      fmt.Sprintf("Item %d", f.n),
		Price:     Money{Amount: int64(2000 + f.rnd.Intn(48000)), Currency: f.currency}, // 20..500 THB
		Available: true,
		CreatedAt: f.stamp(),
	}
	for _, o := range opts {
		o(&mi)
	}
	return mi
}

// CartOpt overrides a Cart default.
type CartOpt func(*Cart)

// WithCartUser sets the owning user id.
func WithCartUser(id string) CartOpt { return func(c *Cart) { c.UserID = id } }

// WithCartMerchant sets the merchant id.
func WithCartMerchant(id string) CartOpt { return func(c *Cart) { c.MerchantID = id } }

// WithCartLines sets the cart lines.
func WithCartLines(lines ...CartLine) CartOpt { return func(c *Cart) { c.Lines = lines } }

// Cart builds a basket.
func (f *Factory) Cart(opts ...CartOpt) Cart {
	c := Cart{
		ID:        f.newID("crt"),
		Lines:     []CartLine{},
		CreatedAt: f.stamp(),
	}
	for _, o := range opts {
		o(&c)
	}
	return c
}

// OrderOpt overrides an Order default.
type OrderOpt func(*Order)

// WithStatus overrides an order's status (03 §3 example).
func WithStatus(s string) OrderOpt { return func(o *Order) { o.Status = s } }

// WithRegion overrides an order's region (03 §3 example).
func WithRegion(r string) OrderOpt { return func(o *Order) { o.Region = r } }

// WithMerchant sets an order's merchant id.
func WithMerchant(id string) OrderOpt { return func(o *Order) { o.MerchantID = id } }

// WithUser sets an order's user id.
func WithUser(id string) OrderOpt { return func(o *Order) { o.UserID = id } }

// WithDriver sets an order's assigned driver id.
func WithDriver(id string) OrderOpt { return func(o *Order) { o.DriverID = id } }

// WithTotal overrides an order's total (minor units).
func WithTotal(minor int64) OrderOpt { return func(o *Order) { o.Total.Amount = minor } }

// Order builds an order.
func (f *Factory) Order(opts ...OrderOpt) Order {
	o := Order{
		ID:        f.newID("ord"),
		Status:    "PAYMENT_PENDING",
		Total:     Money{Amount: int64(5000 + f.rnd.Intn(95000)), Currency: f.currency}, // 50..1000 THB
		Region:    f.region,
		CreatedAt: f.stamp(),
	}
	for _, o2 := range opts {
		o2(&o)
	}
	return o
}

// DriverOpt overrides a Driver default.
type DriverOpt func(*Driver)

// WithDriverOnline sets a driver's online flag.
func WithDriverOnline(v bool) DriverOpt { return func(d *Driver) { d.Online = v } }

// WithDriverRegion overrides a driver's region.
func WithDriverRegion(r string) DriverOpt { return func(d *Driver) { d.Region = r } }

var vehicles = []string{"MOTORCYCLE", "BICYCLE", "CAR"}

// Driver builds a delivery driver.
func (f *Factory) Driver(opts ...DriverOpt) Driver {
	d := Driver{
		ID:        f.newID("drv"),
		Name:      fmt.Sprintf("Driver %d", f.n),
		Region:    f.region,
		Vehicle:   vehicles[f.rnd.Intn(len(vehicles))],
		Online:    true,
		CreatedAt: f.stamp(),
	}
	for _, o := range opts {
		o(&d)
	}
	return d
}
