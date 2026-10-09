package llamaserver

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestSupervisorGivesTheChildOnlyItsOwnEnvironment(t *testing.T) {
	t.Setenv("TARAENGINE_API_KEY", "s3cret")
	s, _, logPath := helperSupervisor(t, "HELPER_PRINT_ENV=1", "CHOSEN=yes")
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	stopAndWait(t, s)
	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "env CHOSEN=yes") || strings.Contains(string(data), "s3cret") {
		t.Fatalf("the child sees the environment it was given and nothing of the daemon's:\n%s", data)
	}
}
