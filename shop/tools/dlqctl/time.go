package main

import "time"

// mustTime returns a fixed occurred_at for deterministic seeded demo events.
func mustTime() time.Time { return time.Date(2026, 7, 11, 2, 15, 0, 0, time.UTC) }
