package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// scan.go — the raw-PII detectors run over golden-traffic events + logs (D3:
// "zero raw PII in golden-traffic events/logs"). Detectors are chosen to be
// PRECISE on token-only traffic: they never match prefixed ULIDs, RFC3339
// timestamps, ports or schema versions, so a clean run is genuinely clean and a
// leak is genuinely a leak.

var (
	// email is the strongest structural PII signal; token-only events have no '@'.
	reEmail = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	// intlPhone: an E.164-ish number starting with '+'. Timestamps/ULIDs never
	// start with '+', so this is false-positive-free on our traffic.
	reIntlPhone = regexp.MustCompile(`\+\d[\d\-\s]{6,}\d`)
	// card: 13–19 digit runs (optionally space/dash grouped). Only reported when
	// Luhn-valid, which excludes timestamp/version digit runs.
	reCardish = regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`)
)

type finding struct {
	File string
	Line int
	Kind string
	Hit  string
}

func (f finding) String() string {
	return fmt.Sprintf("%s:%d [%s] %s", f.File, f.Line, f.Kind, f.Hit)
}

// scanTraffic scans each file for raw PII: structural detectors (email / intl
// phone / Luhn-valid card) plus, when provided, exact matches of a known-PII
// wordlist (the strongest seed-driven proof that the exact inputs did not leak).
func scanTraffic(files, known []string) ([]finding, error) {
	knownClean := make([]string, 0, len(known))
	for _, k := range known {
		if k = strings.TrimSpace(k); k != "" {
			knownClean = append(knownClean, k)
		}
	}
	var out []finding
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
		ln := 0
		for sc.Scan() {
			ln++
			line := sc.Text()
			for _, m := range reEmail.FindAllString(line, -1) {
				out = append(out, finding{path, ln, "email", m})
			}
			for _, m := range reIntlPhone.FindAllString(line, -1) {
				out = append(out, finding{path, ln, "phone", m})
			}
			for _, m := range reCardish.FindAllString(line, -1) {
				if luhn(m) {
					out = append(out, finding{path, ln, "card", m})
				}
			}
			for _, k := range knownClean {
				if strings.Contains(line, k) {
					out = append(out, finding{path, ln, "known-pii", k})
				}
			}
		}
		f.Close()
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func luhn(s string) bool {
	digits := make([]int, 0, len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits = append(digits, int(r-'0'))
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	dbl := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if dbl {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		dbl = !dbl
	}
	return sum%10 == 0
}
