// Forge entry point: bind an ephemeral port, register it with the trial
// environment, serve until the environment reports done, then exit 0.
//
// Without MAKEFUN_ENV_URL the forge serves forever (local testing mode).
// The viewer identity is computed from the contest's public seeded scenario
// at startup, so no request path can ever name a different owner.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	envURL := strings.TrimSuffix(os.Getenv("MAKEFUN_ENV_URL"), "/")
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	nowBase := parseTrialNow(os.Getenv("MAKEFUN_TRIAL_NOW"))
	ident := scenarioIdentityOf()
	st := newStore(nowBase, ident.viewer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	done := make(chan struct{})
	go serve(ln, st, token, done)

	if envURL == "" {
		fmt.Fprintf(os.Stderr, "forge listening on 127.0.0.1:%d (serve-only, viewer %s)\n", port, ident.viewer.login)
		select {}
	}
	if err := register(envURL, port); err != nil {
		fatal(err)
	}
	pollDone(envURL, done)
}

// parseTrialNow converts MAKEFUN_TRIAL_NOW (RFC3339, unix seconds, or unix
// milliseconds) into unix seconds. Falls back to a fixed date.
func parseTrialNow(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 1735689600
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.Unix()
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n > 1000000000000 {
			return n / 1000
		}
		if n > 1000000000 {
			return n
		}
	}
	return 1735689600
}

// register POSTs {"port": N} to the environment. Retries transport errors.
func register(envURL string, port int) error {
	payload, _ := json.Marshal(map[string]int{"port": port})
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		resp, err := http.Post(envURL+"/register", "application/json", bytes.NewReader(payload))
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(100*(attempt+1)) * time.Millisecond)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == 200 || resp.StatusCode == 201 {
			return nil
		}
		if resp.StatusCode == 409 {
			return fmt.Errorf("register: port already registered (409)")
		}
		lastErr = fmt.Errorf("register: HTTP %d", resp.StatusCode)
		time.Sleep(500 * time.Millisecond)
	}
	return lastErr
}

// pollDone GETs /done with 250 ms -> 2000 ms backoff until done, then
// closes the done channel so the server shuts down and the process exits 0.
func pollDone(envURL string, done chan struct{}) {
	delay := 250 * time.Millisecond
	client := &http.Client{Timeout: 10 * time.Second}
	for {
		resp, err := client.Get(envURL + "/done")
		if err == nil {
			var body struct {
				Done bool `json:"done"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if body.Done {
				close(done)
				// Give the listener a moment to close, then exit.
				time.Sleep(100 * time.Millisecond)
				return
			}
		}
		time.Sleep(delay)
		if delay < 2000*time.Millisecond {
			delay *= 2
			if delay > 2000*time.Millisecond {
				delay = 2000 * time.Millisecond
			}
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "forge:", err)
	os.Exit(1)
}
