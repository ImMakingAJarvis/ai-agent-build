// drain.go is the "drain" subcommand (card ELR-99, M4.1g): it asks the
// daemon to stop taking new work (POST /v1/drain), prints the ops still
// running, and with --wait polls GET /v1/state until the running set is
// empty or the time is up. It is the owner's side of the deploy guard:
// scripts/install_loopd.sh refuses to restart the daemon while
// running_ops is not empty, and this is how the ops get finished first.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// drainPoll is how often --wait re-asks the state endpoint.
var drainPoll = 250 * time.Millisecond

// drainState is the part of GET /v1/state the drain subcommand reads:
// the draining flag and the running job/op pairs.
type drainState struct {
	Draining   bool                `json:"draining"`
	RunningOps []map[string]string `json:"running_ops"`
}

// drain is the body of the "drain" subcommand: args starts with "drain",
// the rest are flags. client is the HTTP client (tests point it at an
// httptest server), out receives the report. It returns the process exit
// code: 0 when the drain was started and either --wait was not given or
// the running set drained in time; 1 when ops are still running at the
// --wait deadline, or the token read or any request failed.
func drain(args []string, client *http.Client, out io.Writer) int {
	if len(args) == 0 || args[0] != "drain" {
		fmt.Fprintln(out, "loopctl: expected the \"drain\" subcommand")
		return 1
	}

	fs := flag.NewFlagSet("drain", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprint(out, `usage: loopctl drain [--addr URL] [--token-file PATH] [--wait DURATION]

Asks loopd to stop taking new work (POST /v1/drain) and prints the ops
still running, one job/op pair per line. With --wait it keeps polling
GET /v1/state until the running set is empty (exit 0) or the duration
is up (exit 1, the ops still running are printed).

--addr        the daemon's API address (default http://127.0.0.1:9110)
--token-file  the bearer token file (default /run/loopd/token)
--wait        wait for the running ops to drain before returning (e.g. 5m)
`)
	}
	addr := fs.String("addr", "http://127.0.0.1:9110", "the daemon's API address")
	tokenFile := fs.String("token-file", "/run/loopd/token", "the bearer token file")
	wait := fs.Duration("wait", 0, "wait for the running ops to drain (e.g. 5m)")
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}

	raw, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintf(out, "loopctl: token: %v\n", err)
		return 1
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		fmt.Fprintf(out, "loopctl: token file %s is empty\n", *tokenFile)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	st, err := drainPost(ctx, client, *addr, token)
	if err != nil {
		fmt.Fprintf(out, "loopctl: drain: %v\n", err)
		return 1
	}
	fmt.Fprintln(out, "drain started")
	printOps(out, st.RunningOps)
	if *wait <= 0 {
		return 0
	}

	// The wait: poll the state until the running set is empty (exit 0)
	// or the duration is up (exit 1, naming what is still running).
	deadline := time.Now().Add(*wait)
	for {
		time.Sleep(drainPoll)
		st, err = stateGet(ctx, client, *addr, token)
		if err != nil {
			fmt.Fprintf(out, "loopctl: state: %v\n", err)
			return 1
		}
		if len(st.RunningOps) == 0 {
			fmt.Fprintln(out, "drained")
			return 0
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(out, "still running after the wait deadline:")
			printOps(out, st.RunningOps)
			return 1
		}
	}
}

// printOps writes the running job/op pairs, one per line, as job/op.
func printOps(out io.Writer, ops []map[string]string) {
	for _, op := range ops {
		fmt.Fprintf(out, "%s/%s\n", op["job"], op["op"])
	}
}

// drainPost POSTs /v1/drain and decodes the answer (the flag and the ops
// still running).
func drainPost(ctx context.Context, client *http.Client, addr, token string) (drainState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(addr, "/")+"/v1/drain", nil)
	if err != nil {
		return drainState{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return drainState{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return drainState{}, fmt.Errorf("POST /v1/drain: HTTP %d", resp.StatusCode)
	}
	var st drainState
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return drainState{}, fmt.Errorf("decode drain: %w", err)
	}
	return st, nil
}

// stateGet GETs /v1/state and decodes the answer.
func stateGet(ctx context.Context, client *http.Client, addr, token string) (drainState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(addr, "/")+"/v1/state", nil)
	if err != nil {
		return drainState{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return drainState{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return drainState{}, fmt.Errorf("GET /v1/state: HTTP %d", resp.StatusCode)
	}
	var st drainState
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return drainState{}, fmt.Errorf("decode state: %w", err)
	}
	return st, nil
}
