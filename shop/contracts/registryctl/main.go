// Command registryctl is the S-T5 contracts platform gate (implements D30).
//
// It is the single tool the CI `contract-validate` and `pact-verify` stages run
// against contracts/ — the monorepo's single integration surface. Subcommands:
//
//	registryctl validate <contracts-root>
//	    Parse every OpenAPI file (02 §1 conventions: /v1 paths, snake_case
//	    fields, error envelope referenced) AND every event topic schema
//	    (02 §4.3 envelope conformance + snake_case payload fields), and enforce
//	    the D30 dual-publish rule: a <topic>.v2 dir requires the base topic to
//	    carry a deprecation.yaml with a valid, not-yet-past deprecation_date.
//
//	registryctl diff <old.schema.json> <new.schema.json>
//	    Enforce D30 additive-only compatibility between two versions of the same
//	    topic schema: exit nonzero on any remove / rename / type-change /
//	    required-addition / enum-removal. Exit 0 when the change is purely
//	    additive (new optional fields).
//
//	registryctl pact-verify <pact.json> <provider-base-url>
//	    Replay each Pact interaction against a running provider and assert the
//	    response status + shape. Exit nonzero if any interaction is unsatisfied
//	    (the broker gate: "breaking a published pact => provider build red").
//
// Exit codes: 0 = green, 1 = a contract violation (the gate fired), 2 = usage /
// I/O error. Kept dependency-light (stdlib + yaml.v3, already vendored by
// tools/yamlcheck) to match the repo's build story.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "validate":
		if len(os.Args) != 3 {
			usage()
			os.Exit(2)
		}
		err = cmdValidate(os.Args[2])
	case "diff":
		if len(os.Args) != 4 {
			usage()
			os.Exit(2)
		}
		err = cmdDiff(os.Args[2], os.Args[3])
	case "pact-verify":
		if len(os.Args) != 4 {
			usage()
			os.Exit(2)
		}
		err = cmdPactVerify(os.Args[2], os.Args[3])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "registryctl: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  registryctl validate <contracts-root>
  registryctl diff <old.schema.json> <new.schema.json>
  registryctl pact-verify <pact.json> <provider-base-url>
`)
}
