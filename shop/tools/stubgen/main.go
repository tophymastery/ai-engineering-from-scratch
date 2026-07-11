// Command stubgen reads a published OpenAPI contract and boots a stub HTTP server
// that answers every declared path+method with an example/schema-derived response.
//
// This is the S-T5 affordance that lets a slice develop against an UNBUILT
// neighbour: point the slice at `stubgen -spec contracts/openapi/order.v1.yaml`
// and it gets a running order service returning contract-shaped JSON, long before
// the real order service exists. In the shared E2E env (S-T8) the stub is swapped
// for the real implementation on merge with zero call-site changes.
//
// Response selection per operation: the lowest 2xx response; its
// content.application/json `example` if present, else a value synthesised from
// the (ref-resolved) response schema. Path templates (`{order_id}`) and the
// `:action` verb suffix (`/v1/orders/{order_id}:cancel`) are both supported via a
// regex router, so it stubs any spec that follows the 02 conventions.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

type route struct {
	method string
	path   string
	re     *regexp.Regexp
	status int
	body   any
}

func main() {
	spec := flag.String("spec", "", "path to an OpenAPI YAML file")
	port := flag.String("port", "9090", "port to serve the stub on")
	printOnly := flag.Bool("print", false, "print the generated routes and exit (do not serve)")
	idempotency := flag.Bool("idempotency", false, "replay-header behaviour: a repeat mutation with a seen Idempotency-Key gets Idempotency-Replayed: true (S-T8 topology default)")
	flag.Parse()
	if *spec == "" {
		fmt.Fprintln(os.Stderr, "usage: stubgen -spec <openapi.yaml> [-port N] [-print]")
		os.Exit(2)
	}

	doc, err := loadSpec(*spec)
	if err != nil {
		log.Fatalf("stubgen: %v", err)
	}
	routes, err := buildRoutes(doc)
	if err != nil {
		log.Fatalf("stubgen: %v", err)
	}
	title := "?"
	if info, ok := doc["info"].(map[string]any); ok {
		if t, ok := info["title"].(string); ok {
			title = t
		}
	}
	fmt.Printf("stubgen: %q — %d route(s) from %s\n", title, len(routes), *spec)
	for _, r := range routes {
		fmt.Printf("  %-4s %-40s -> %d\n", r.method, r.path, r.status)
	}
	if *printOnly {
		return
	}

	// Idempotency-replay ledger: when -idempotency is set, a mutation carrying an
	// Idempotency-Key that has been seen before is answered with the same status
	// plus an Idempotency-Replayed: true header — the contract-level "replay a
	// prior effect" signal every mutation obeys (02 §3). Stubs hold no real state,
	// so this proves the replay HEADER path the E2E smoke asserts; the durable
	// exactly-once effect lives in libs/idempotency once the real service merges.
	var idemMu sync.Mutex
	seenKeys := map[string]bool{}

	mux := http.NewServeMux()

	// Built-in health endpoint so e2e-up can healthcheck any stub uniformly. It is
	// intentionally OUTSIDE the contract (health is /healthz, unversioned) — this
	// is what lets stubgen boot 100% of the topology from /v1-only contracts.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Stub", title)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "service": title, "stub": true})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		for _, rt := range routes {
			if rt.method == r.Method && rt.re.MatchString(r.URL.Path) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Stub", title)
				if *idempotency && isMutation(r.Method) {
					if key := r.Header.Get("Idempotency-Key"); key != "" {
						idemMu.Lock()
						replay := seenKeys[key]
						seenKeys[key] = true
						idemMu.Unlock()
						if replay {
							w.Header().Set("Idempotency-Replayed", "true")
						}
					}
				}
				w.WriteHeader(rt.status)
				_ = json.NewEncoder(w).Encode(rt.body)
				return
			}
		}
		// Contract-shaped 404 (02 §2 error envelope) for unknown routes.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": "STUB_ROUTE_NOT_FOUND", "message": r.Method + " " + r.URL.Path + " not in contract",
			"trace_id": "stub", "retryable": false,
		}})
	})

	addr := ":" + *port
	log.Printf("stubgen serving %q on %s", title, addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("stubgen server exited: %v", err)
	}
}

func loadSpec(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: parse: %w", path, err)
	}
	return doc, nil
}

var methods = []string{"get", "post", "patch", "delete", "put"}

// isMutation reports whether a method is a state-changing verb that carries an
// Idempotency-Key (02 §3): POST, PATCH, PUT, DELETE.
func isMutation(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

func buildRoutes(doc map[string]any) ([]route, error) {
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("no paths in spec")
	}
	var out []route
	// deterministic ordering: longer/more-specific paths first so a :action route
	// is tried before the bare resource route.
	var keys []string
	for p := range paths {
		keys = append(keys, p)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })

	for _, p := range keys {
		item, _ := paths[p].(map[string]any)
		for _, m := range methods {
			op, ok := item[m].(map[string]any)
			if !ok {
				continue
			}
			status, body := exampleResponse(doc, op)
			out = append(out, route{
				method: strings.ToUpper(m),
				path:   p,
				re:     pathToRegex(p),
				status: status,
				body:   body,
			})
		}
	}
	return out, nil
}

// pathToRegex turns an OpenAPI path template into a full-match regex. {param}
// tokens match one segment excluding '/' and ':' (so a trailing :action stays
// literal); everything else is matched literally.
func pathToRegex(p string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	i := 0
	for i < len(p) {
		if p[i] == '{' {
			if j := strings.IndexByte(p[i:], '}'); j >= 0 {
				b.WriteString("[^/:]+")
				i += j + 1
				continue
			}
		}
		b.WriteString(regexp.QuoteMeta(string(p[i])))
		i++
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// exampleResponse returns the lowest-2xx status and a JSON body for it: the
// response `example` if present, else a value synthesised from its schema.
func exampleResponse(doc, op map[string]any) (int, any) {
	responses, _ := op["responses"].(map[string]any)
	status := 200
	var chosen map[string]any
	best := 999
	for code, r := range responses {
		n, err := strconv.Atoi(code)
		if err != nil || n < 200 || n >= 300 {
			continue
		}
		if n < best {
			best = n
			status = n
			chosen, _ = r.(map[string]any)
		}
	}
	if chosen == nil {
		return status, map[string]any{"stub": true}
	}
	content, _ := chosen["content"].(map[string]any)
	appjson, _ := content["application/json"].(map[string]any)
	if appjson == nil {
		return status, map[string]any{"stub": true}
	}
	if ex, ok := appjson["example"]; ok {
		return status, ex
	}
	if sch, ok := appjson["schema"].(map[string]any); ok {
		return status, synth(doc, sch, 0)
	}
	return status, map[string]any{"stub": true}
}

// synth builds an example value from a (possibly $ref) JSON schema node.
func synth(doc, sch map[string]any, depth int) any {
	if depth > 12 {
		return nil
	}
	if ref, ok := sch["$ref"].(string); ok {
		if r := resolveRef(doc, ref); r != nil {
			return synth(doc, r, depth+1)
		}
		return nil
	}
	if ex, ok := sch["example"]; ok {
		return ex
	}
	if enum, ok := sch["enum"].([]any); ok && len(enum) > 0 {
		return enum[0]
	}
	switch typeToken(sch["type"]) {
	case "object":
		obj := map[string]any{}
		if props, ok := sch["properties"].(map[string]any); ok {
			for name, sub := range props {
				if sm, ok := sub.(map[string]any); ok {
					obj[name] = synth(doc, sm, depth+1)
				}
			}
		}
		return obj
	case "array":
		if items, ok := sch["items"].(map[string]any); ok {
			return []any{synth(doc, items, depth+1)}
		}
		return []any{}
	case "integer":
		return 0
	case "number":
		return 0
	case "boolean":
		return false
	case "null":
		return nil
	default:
		return "string"
	}
}

func typeToken(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok && s != "null" {
				return s
			}
		}
	}
	return ""
}

func resolveRef(doc map[string]any, ref string) map[string]any {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	cur := any(doc)
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	if m, ok := cur.(map[string]any); ok {
		return m
	}
	return nil
}
