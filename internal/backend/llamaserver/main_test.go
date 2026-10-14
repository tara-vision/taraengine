package llamaserver

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestMain turns the test binary into a fake llama-server when TARAENGINE_HELPER is set, or when it
// runs under the name llama-server: the backend gives its child an environment of its own, so no
// variable of the test reaches a child the backend starts. The helper prints one line and serves
// /health on --port. Its environment shapes it:
//
//	HELPER_FAIL_START     writes this message to stderr and exits with code 127 at once, as llama-server does
//	                      when the loader cannot find a library it needs
//	HELPER_NO_HEALTH=1    never listens
//	HELPER_IGNORE_TERM=1  ignores SIGTERM, so only SIGKILL ends it
//	HELPER_EXIT_AFTER_MS  exits by itself after that long with HELPER_EXIT_CODE; the clock starts
//	                      once the listener is up, so a prober gets the whole span
//	HELPER_PRINT_ENV=1    prints its environment, one "env NAME=value" line per variable
//	HELPER_GRANDCHILD=1   starts a helper of its own in its process group, as the router starts a
//	                      model process, and prints "grandchild <pid>"; with
//	                      HELPER_GRANDCHILD_IGNORES_TERM=1 that grandchild ignores SIGTERM
//
// Otherwise it exits with status 0 on SIGTERM.
func TestMain(m *testing.M) {
	if os.Getenv("TARAENGINE_HELPER") != "1" && filepath.Base(os.Args[0]) != "llama-server" {
		os.Exit(m.Run())
	}
	runHelper()
}

func runHelper() {
	if msg := os.Getenv("HELPER_FAIL_START"); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(127)
	}
	port := "0"
	for i, a := range os.Args {
		if a == "--port" && i+1 < len(os.Args) {
			port = os.Args[i+1]
		}
	}
	switch {
	case os.Getenv("HELPER_IGNORE_TERM") == "1":
		signal.Ignore(syscall.SIGTERM)
	case os.Getenv("HELPER_NO_HEALTH") != "1":
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGTERM)
		go func() { <-sigs; os.Exit(0) }()
	}
	fmt.Println("helper started") // the signal disposition is in place once this line is out
	if os.Getenv("HELPER_PRINT_ENV") == "1" {
		for _, kv := range os.Environ() {
			fmt.Println("env " + kv)
		}
	}
	if os.Getenv("HELPER_GRANDCHILD") == "1" {
		startGrandchild()
	}
	if os.Getenv("HELPER_NO_HEALTH") == "1" {
		exitLater()
		// A bare select {} would trip the runtime deadlock detector and exit with code 2 at once; a
		// pending timer keeps the helper alive, never listening, until the supervisor kills it.
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	l, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		os.Exit(2)
	}
	exitLater()
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"status":"ok"}`)) })
	if err := http.Serve(l, nil); err != nil {
		os.Exit(2)
	}
}

// startGrandchild starts one more helper in this helper's process group. It never listens, writes to the
// helper's own output, so it keeps the log pipe open after the helper is gone, dies of SIGTERM, and ends
// by itself after 30 seconds at the latest.
func startGrandchild() {
	cmd := exec.Command(os.Args[0])
	cmd.Env = []string{"TARAENGINE_HELPER=1", "HELPER_NO_HEALTH=1", "HELPER_EXIT_AFTER_MS=30000"}
	if os.Getenv("HELPER_GRANDCHILD_IGNORES_TERM") == "1" {
		cmd.Env = append(cmd.Env, "HELPER_IGNORE_TERM=1")
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		os.Exit(3)
	}
	fmt.Printf("grandchild %d\n", cmd.Process.Pid)
}

// exitLater makes the helper exit by itself when HELPER_EXIT_AFTER_MS is set.
func exitLater() {
	ms := os.Getenv("HELPER_EXIT_AFTER_MS")
	if ms == "" {
		return
	}
	after, _ := strconv.Atoi(ms)
	code, _ := strconv.Atoi(os.Getenv("HELPER_EXIT_CODE"))
	go func() {
		time.Sleep(time.Duration(after) * time.Millisecond)
		os.Exit(code)
	}()
}
