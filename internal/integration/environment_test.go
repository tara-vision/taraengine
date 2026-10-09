//go:build integration

package integration

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// childMarker tells TestRequireEnvironmentChild that it runs as the child of the test below.
const childMarker = "INTEGRATION_CONTRACT_CHILD"

// TestRequireEnvironmentChild calls requireEnvironment and nothing else. On its own it does nothing: the test below
// runs it in a child process, with the environment it wants to see judged.
func TestRequireEnvironmentChild(t *testing.T) {
	if os.Getenv(childMarker) == "" {
		t.Skip("runs only as the child of TestRequireEnvironmentFailsInCIAndSkipsElsewhere")
	}
	requireEnvironment(t)
}

// TestRequireEnvironmentFailsInCIAndSkipsElsewhere pins the environment contract: without the variables that
// scripts/integration.sh sets, the test skips on a developer machine and fails where CI is set, so that a broken
// script cannot turn the integration job green by running nothing.
func TestRequireEnvironmentFailsInCIAndSkipsElsewhere(t *testing.T) {
	cases := []struct {
		name     string
		ci       bool
		wantExit int
		wantLine string
	}{
		{"CI set", true, 1, "--- FAIL: TestRequireEnvironmentChild"},
		{"CI not set", false, 0, "--- SKIP: TestRequireEnvironmentChild"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestRequireEnvironmentChild$", "-test.v")
			cmd.Env = append(childEnvironment(), childMarker+"=1")
			if tc.ci {
				cmd.Env = append(cmd.Env, "CI=true")
			}
			out, err := cmd.CombinedOutput()
			exit := 0
			var exited *exec.ExitError
			if errors.As(err, &exited) {
				exit = exited.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if exit != tc.wantExit || !strings.Contains(string(out), tc.wantLine) {
				t.Fatalf("exit %d, want %d, and %q in the output:\n%s", exit, tc.wantExit, tc.wantLine, out)
			}
		})
	}
}

// childEnvironment is this process's environment without CI and without the variables of the contract.
func childEnvironment() []string {
	var kept []string
	for _, kv := range environmentWithout(ownedEnvPrefix) {
		if !strings.HasPrefix(kv, "CI=") {
			kept = append(kept, kv)
		}
	}
	return kept
}
