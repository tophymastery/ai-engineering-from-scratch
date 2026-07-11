package main

import (
	"encoding/base64"
	"os"
)

// loadKeyring builds the master KEK. In prod PROFILE_KEK is a 32-byte
// base64-encoded key sourced from KMS; when unset (dev/test/e2e) a per-process
// random KEK is used. The KEK only ever wraps per-user DEKs — never PII.
func loadKeyring() (*keyring, error) {
	if v := os.Getenv("PROFILE_KEK"); v != "" {
		raw, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, err
		}
		return newKeyring(raw)
	}
	return newKeyring(randomKey(dekLen))
}
