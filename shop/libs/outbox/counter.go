package outbox

import "sync/atomic"

// int64Counter is a tiny concurrency-safe counter used for audit totals.
type int64Counter struct{ v atomic.Int64 }

func (c *int64Counter) add(n int64) { c.v.Add(n) }
func (c *int64Counter) get() int64  { return c.v.Load() }
