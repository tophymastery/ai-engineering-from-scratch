package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type pactFile struct {
	Consumer     struct{ Name string } `json:"consumer"`
	Provider     struct{ Name string } `json:"provider"`
	Interactions []struct {
		Description string `json:"description"`
		Request     struct {
			Method  string            `json:"method"`
			Path    string            `json:"path"`
			Headers map[string]string `json:"headers"`
			Body    json.RawMessage   `json:"body"`
		} `json:"request"`
		Response struct {
			Status int             `json:"status"`
			Body   json.RawMessage `json:"body"`
		} `json:"response"`
	} `json:"interactions"`
}

// cmdPactVerify replays every interaction in a file-based pact against a running
// provider and asserts status + response shape. This is the broker gate: an
// interaction the provider cannot satisfy fails the build (exit 1).
//
// File-based broker adaptation (no pact-broker binary available): the pact JSON
// is Pact-v2 shaped (interactions with request/response); "verification" is a
// live replay rather than a broker handshake. Shape rule: every key in the pact's
// response.body must be present in the provider's JSON response, and any scalar
// value pinned in the pact must match exactly.
func cmdPactVerify(pactPath, baseURL string) error {
	b, err := os.ReadFile(pactPath)
	if err != nil {
		return err
	}
	var p pactFile
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("%s: parse: %w", pactPath, err)
	}
	baseURL = strings.TrimRight(baseURL, "/")
	fmt.Printf("pact-verify: %s -> %s @ %s (%d interaction(s))\n",
		p.Consumer.Name, p.Provider.Name, baseURL, len(p.Interactions))

	client := &http.Client{Timeout: 5 * time.Second}
	var failures []string
	for _, it := range p.Interactions {
		if err := verifyInteraction(client, baseURL, it.Description,
			it.Request.Method, it.Request.Path, it.Request.Headers, it.Request.Body,
			it.Response.Status, it.Response.Body); err != nil {
			failures = append(failures, fmt.Sprintf("[%s] %v", it.Description, err))
			fmt.Printf("  FAIL: %s — %v\n", it.Description, err)
		} else {
			fmt.Printf("  PASS: %s\n", it.Description)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("pact NOT honoured by provider %q: %d/%d interaction(s) failed",
			p.Provider.Name, len(failures), len(p.Interactions))
	}
	fmt.Printf("pact-verify: OK — provider honours all %d interaction(s)\n", len(p.Interactions))
	return nil
}

func verifyInteraction(client *http.Client, baseURL, desc, method, path string,
	headers map[string]string, reqBody json.RawMessage, wantStatus int, wantBody json.RawMessage) error {

	var body io.Reader
	if len(reqBody) > 0 && string(reqBody) != "null" {
		body = bytes.NewReader(reqBody)
	}
	req, err := http.NewRequest(method, baseURL+path, body)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed (provider down or missing route?): %w", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != wantStatus {
		return fmt.Errorf("status: want %d, got %d (body=%s)", wantStatus, resp.StatusCode, truncate(got))
	}
	if len(wantBody) == 0 || string(wantBody) == "null" {
		return nil
	}
	var want, actual any
	if err := json.Unmarshal(wantBody, &want); err != nil {
		return fmt.Errorf("pact response.body is not JSON: %w", err)
	}
	if err := json.Unmarshal(got, &actual); err != nil {
		return fmt.Errorf("provider response is not JSON: %s", truncate(got))
	}
	if err := matchShape("$", want, actual); err != nil {
		return err
	}
	return nil
}

// matchShape asserts `want` is a shape-subset of `actual`: every key in a want
// object must exist in actual (recursively); pinned scalars must be equal.
func matchShape(path string, want, actual any) error {
	switch w := want.(type) {
	case map[string]any:
		a, ok := actual.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: want object, got %T", path, actual)
		}
		for k, wv := range w {
			av, ok := a[k]
			if !ok {
				return fmt.Errorf("%s.%s: key missing in provider response", path, k)
			}
			if err := matchShape(path+"."+k, wv, av); err != nil {
				return err
			}
		}
	case []any:
		a, ok := actual.([]any)
		if !ok {
			return fmt.Errorf("%s: want array, got %T", path, actual)
		}
		if len(a) < len(w) {
			return fmt.Errorf("%s: want >=%d elements, got %d", path, len(w), len(a))
		}
		for i := range w {
			if err := matchShape(fmt.Sprintf("%s[%d]", path, i), w[i], a[i]); err != nil {
				return err
			}
		}
	default:
		if fmt.Sprintf("%v", want) != fmt.Sprintf("%v", actual) {
			return fmt.Errorf("%s: want %v, got %v", path, want, actual)
		}
	}
	return nil
}

func truncate(b []byte) string {
	const max = 160
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
