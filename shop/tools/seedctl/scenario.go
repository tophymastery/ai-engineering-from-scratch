package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Scenario is the declarative seed spec (03 §3). Its YAML shape is the golden
// contract shared by demos, E2E and load tests.
type Scenario struct {
	Seed      int64  `yaml:"seed"`
	Region    string `yaml:"region"`
	Merchants struct {
		Count     int `yaml:"count"`
		MenusEach int `yaml:"menus_each"`
	} `yaml:"merchants"`
	Customers struct {
		Count int `yaml:"count"`
	} `yaml:"customers"`
	Drivers struct {
		Count       int     `yaml:"count"`
		OnlineRatio float64 `yaml:"online_ratio"`
	} `yaml:"drivers"`
	Orders []OrderGroup `yaml:"orders"`

	// Name is derived from the file stem, not the YAML body.
	Name string `yaml:"-"`
}

// OrderGroup is a run of orders sharing a state (03 §3).
type OrderGroup struct {
	Count int    `yaml:"count"`
	State string `yaml:"state"`
}

// LoadScenario parses and validates a scenario file.
func LoadScenario(path, name string) (*Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Scenario
	if err := yaml.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.Name = name
	if s.Region == "" {
		s.Region = "bkk"
	}
	if s.Merchants.Count < 0 || s.Customers.Count < 0 || s.Drivers.Count < 0 {
		return nil, fmt.Errorf("%s: counts must be non-negative", path)
	}
	if s.Drivers.OnlineRatio < 0 || s.Drivers.OnlineRatio > 1 {
		return nil, fmt.Errorf("%s: drivers.online_ratio must be in [0,1]", path)
	}
	return &s, nil
}
