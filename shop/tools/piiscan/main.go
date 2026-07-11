// Command piiscan is the V-T2 PII scanner + data-inventory validator (D3). It is
// a real CI stage (ci/pii-scan.sh) with two jobs:
//
//	scan-traffic  — assert ZERO raw PII in golden-traffic events/logs. Goes RED
//	                the instant a name/phone/email/address leaks into an event or
//	                log line.
//	check-inventory — assert every PII-bearing column in the migrations is
//	                registered in data-inventory.yaml, and every class has a
//	                retention entry. Goes RED on an UNREGISTERED PII table/column
//	                (the "unregistered-table fixture ⇒ CI red" criterion).
//	validate      — assert both registers are internally well-formed.
//
// Exit code is the gate: 0 = clean, 1 = violations (prints them), 2 = usage/IO.
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "scan-traffic":
		cmdScanTraffic(os.Args[2:])
	case "check-inventory":
		cmdCheckInventory(os.Args[2:])
	case "validate":
		cmdValidate(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `piiscan <command>:
  scan-traffic [--known FILE] FILE...            scan events/logs for raw PII (RED on any hit)
  check-inventory INVENTORY RETENTION SQL...     every PII column registered (RED on unregistered)
  validate INVENTORY RETENTION                   registers well-formed`)
	os.Exit(2)
}

func cmdScanTraffic(args []string) {
	var known []string
	var files []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--known" {
			i++
			if i >= len(args) {
				usage()
			}
			b, err := os.ReadFile(args[i])
			if err != nil {
				fmt.Fprintf(os.Stderr, "piiscan: read known list: %v\n", err)
				os.Exit(2)
			}
			known = append(known, strings.Split(strings.TrimSpace(string(b)), "\n")...)
			continue
		}
		files = append(files, args[i])
	}
	if len(files) == 0 {
		usage()
	}
	findings, err := scanTraffic(files, known)
	if err != nil {
		fmt.Fprintf(os.Stderr, "piiscan: %v\n", err)
		os.Exit(2)
	}
	if len(findings) > 0 {
		fmt.Printf("piiscan scan-traffic: RED — %d raw-PII finding(s):\n", len(findings))
		for _, f := range findings {
			fmt.Printf("  %s\n", f)
		}
		os.Exit(1)
	}
	fmt.Printf("piiscan scan-traffic: GREEN — 0 raw PII across %d file(s) (%d known-PII strings checked)\n", len(files), countNonEmpty(known))
}

func cmdCheckInventory(args []string) {
	if len(args) < 3 {
		usage()
	}
	inv, err := loadInventory(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "piiscan: inventory: %v\n", err)
		os.Exit(2)
	}
	ret, err := loadRetention(args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "piiscan: retention: %v\n", err)
		os.Exit(2)
	}
	violations, err := checkInventory(args[2:], inv, ret)
	if err != nil {
		fmt.Fprintf(os.Stderr, "piiscan: %v\n", err)
		os.Exit(2)
	}
	if len(violations) > 0 {
		fmt.Printf("piiscan check-inventory: RED — %d violation(s):\n", len(violations))
		for _, v := range violations {
			fmt.Printf("  %s\n", v)
		}
		os.Exit(1)
	}
	fmt.Printf("piiscan check-inventory: GREEN — every PII column in %d migration file(s) is registered + has a retention class\n", len(args[2:]))
}

func cmdValidate(args []string) {
	if len(args) != 2 {
		usage()
	}
	inv, err := loadInventory(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "piiscan: inventory: %v\n", err)
		os.Exit(2)
	}
	ret, err := loadRetention(args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "piiscan: retention: %v\n", err)
		os.Exit(2)
	}
	if v := validateRegisters(inv, ret); len(v) > 0 {
		fmt.Printf("piiscan validate: RED — %d register problem(s):\n", len(v))
		for _, x := range v {
			fmt.Printf("  %s\n", x)
		}
		os.Exit(1)
	}
	fmt.Printf("piiscan validate: GREEN — data-inventory + retention registers well-formed (erasure=%s sla=%dh)\n", ret.Erasure.Mechanism, ret.Erasure.SLAHours)
}

func countNonEmpty(ss []string) int {
	n := 0
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			n++
		}
	}
	return n
}
