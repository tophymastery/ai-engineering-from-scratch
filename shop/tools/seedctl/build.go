package main

import (
	"encoding/json"

	"github.com/shop-platform/shop/libs/factories"
)

// Dataset is the fully-materialised, deterministically-ordered seed dataset. The
// field order here IS the canonical dump order; every slice is in creation order.
// Marshalling it yields byte-identical JSON for a given (seed, scenario).
type Dataset struct {
	SchemaVersion int                 `json:"schema_version"`
	ScenarioName  string              `json:"scenario_name"`
	Seed          int64               `json:"seed"`
	Region        string              `json:"region"`
	Counts        map[string]int      `json:"counts"`
	Users         []factories.User    `json:"users"`
	Merchants     []factories.Merchant `json:"merchants"`
	MenuItems     []factories.MenuItem `json:"menu_items"`
	Carts         []factories.Cart    `json:"carts"`
	Drivers       []factories.Driver  `json:"drivers"`
	Orders        []factories.Order   `json:"orders"`
}

// Build materialises a Scenario into a Dataset using the seeded factories. The
// construction order (merchants → menus → customers → drivers → orders+carts) is
// FIXED so the seeded RNG stream — and therefore every id, price and timestamp —
// is reproduced exactly on every run.
func Build(s *Scenario) *Dataset {
	f := factories.New(s.Seed, factories.Region(s.Region))
	ds := &Dataset{
		SchemaVersion: 1,
		ScenarioName:  s.Name,
		Seed:          s.Seed,
		Region:        s.Region,
		Users:         []factories.User{},
		Merchants:     []factories.Merchant{},
		MenuItems:     []factories.MenuItem{},
		Carts:         []factories.Cart{},
		Drivers:       []factories.Driver{},
		Orders:        []factories.Order{},
	}

	// Merchants + their menus.
	menusByMerchant := map[string][]factories.MenuItem{}
	for i := 0; i < s.Merchants.Count; i++ {
		m := f.Merchant()
		ds.Merchants = append(ds.Merchants, m)
		for j := 0; j < s.Merchants.MenusEach; j++ {
			mi := f.MenuItem(factories.WithMenuMerchant(m.ID))
			ds.MenuItems = append(ds.MenuItems, mi)
			menusByMerchant[m.ID] = append(menusByMerchant[m.ID], mi)
		}
	}

	// Customers.
	for i := 0; i < s.Customers.Count; i++ {
		ds.Users = append(ds.Users, f.User())
	}

	// Drivers: the first ceil(online_ratio*count) are online (deterministic).
	onlineN := int(float64(s.Drivers.Count)*s.Drivers.OnlineRatio + 0.5)
	for i := 0; i < s.Drivers.Count; i++ {
		ds.Drivers = append(ds.Drivers, f.Driver(factories.WithDriverOnline(i < onlineN)))
	}

	// Orders (+ the cart each came from). Round-robin assignment over the built
	// users/merchants/drivers keeps referential integrity fully deterministic.
	oi := 0
	for _, g := range s.Orders {
		for k := 0; k < g.Count; k++ {
			var userID, merchantID, driverID string
			if len(ds.Users) > 0 {
				userID = ds.Users[oi%len(ds.Users)].ID
			}
			var menu []factories.MenuItem
			if len(ds.Merchants) > 0 {
				m := ds.Merchants[oi%len(ds.Merchants)]
				merchantID = m.ID
				menu = menusByMerchant[m.ID]
			}
			if driverAssigned(g.State) && len(ds.Drivers) > 0 {
				driverID = ds.Drivers[oi%len(ds.Drivers)].ID
			}

			lines := []factories.CartLine{}
			if len(menu) > 0 {
				lines = append(lines, factories.CartLine{MenuItemID: menu[oi%len(menu)].ID, Qty: 1 + oi%3})
			}
			cart := f.Cart(
				factories.WithCartUser(userID),
				factories.WithCartMerchant(merchantID),
				factories.WithCartLines(lines...),
			)
			ds.Carts = append(ds.Carts, cart)

			order := f.Order(
				factories.WithUser(userID),
				factories.WithMerchant(merchantID),
				factories.WithStatus(g.State),
			)
			if driverID != "" {
				order.DriverID = driverID
			}
			ds.Orders = append(ds.Orders, order)
			oi++
		}
	}

	ds.Counts = map[string]int{
		"users":      len(ds.Users),
		"merchants":  len(ds.Merchants),
		"menu_items": len(ds.MenuItems),
		"carts":      len(ds.Carts),
		"drivers":    len(ds.Drivers),
		"orders":     len(ds.Orders),
	}
	return ds
}

// driverAssigned reports whether a given order state should carry a driver.
func driverAssigned(state string) bool {
	switch state {
	case "DISPATCHED", "DELIVERED", "PICKED_UP", "ARRIVED":
		return true
	default:
		return false
	}
}

// Canonical renders the dataset as deterministic, indented JSON (the artefact the
// byte-identity test hashes).
func (ds *Dataset) Canonical() []byte {
	b, _ := json.MarshalIndent(ds, "", "  ")
	return append(b, '\n')
}
