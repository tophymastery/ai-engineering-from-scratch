// Command yamlcheck validates that every YAML document in the given files parses
// cleanly. Used by `make render` to prove Kustomize output is well-formed YAML
// (the render-not-live-deploy check for S-T1; see VERIFICATION.md).
package main

import (
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: yamlcheck <file.yaml> [more.yaml...]")
		os.Exit(2)
	}
	total := 0
	for _, path := range os.Args[1:] {
		f, err := os.Open(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "yamlcheck: %v\n", err)
			os.Exit(1)
		}
		dec := yaml.NewDecoder(f)
		docs := 0
		for {
			var v any
			err := dec.Decode(&v)
			if err == io.EOF {
				break
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "yamlcheck: %s: parse error: %v\n", path, err)
				f.Close()
				os.Exit(1)
			}
			if v != nil {
				docs++
			}
		}
		f.Close()
		fmt.Printf("yamlcheck: %s OK (%d docs)\n", path, docs)
		total += docs
	}
	fmt.Printf("yamlcheck: %d document(s) parsed\n", total)
}
