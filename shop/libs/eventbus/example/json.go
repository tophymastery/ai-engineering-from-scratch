package main

import "encoding/json"

// jsonUnmarshal is a thin alias kept local so service.go reads cleanly.
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
