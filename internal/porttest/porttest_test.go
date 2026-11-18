package porttest

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The span sits below the ephemeral range: 32768 is where Linux starts it by default, and the machine running the
// test says where its own range starts.
func TestTheSpanIsBelowTheEphemeralRange(t *testing.T) {
	if first+span > 32768 {
		t.Fatalf("[%d, %d) reaches into the default ephemeral range of Linux", first, first+span)
	}
	if low := ephemeralLow(t); first+span > low {
		t.Fatalf("[%d, %d) reaches into this machine's ephemeral range, which starts at %d", first, first+span, low)
	}
}

func TestFreeIsAFreeLoopbackPortInTheSpan(t *testing.T) {
	for range 20 {
		port := Free(t)
		if port < first || port >= first+span {
			t.Fatalf("port %d is outside [%d, %d)", port, first, first+span)
		}
		l, err := net.Listen("tcp", addr(port))
		if err != nil {
			t.Fatalf("port %d is free: %v", port, err)
		}
		_ = l.Close()
	}
}

func TestFreeTriesAnotherPortWhileTheListenIsRefused(t *testing.T) {
	var seen []string
	swapListen(t, recordListen(t, &seen, 3))
	swapDial(t, func(string) (net.Conn, error) { return nil, errors.New("connect: connection refused") })
	port := Free(t)
	checkProbes(t, seen, 4, port)
}

func TestFreeTriesAnotherPortWhileSomethingAnswers(t *testing.T) {
	var listened, dialled []string
	swapListen(t, recordListen(t, &listened, 0))
	swapDial(t, func(address string) (net.Conn, error) {
		dialled = append(dialled, address)
		if len(dialled) <= 3 {
			local, remote := net.Pipe()
			_ = remote.Close()
			return local, nil
		}
		return nil, errors.New("connect: connection refused")
	})
	port := Free(t)
	checkProbes(t, dialled, 4, port)
	if strings.Join(listened, " ") != strings.Join(dialled, " ") {
		t.Fatalf("a port is dialled after its listen opened: listened %v, dialled %v", listened, dialled)
	}
}

func TestFreeFailsTheTestWhenEveryTryIsTaken(t *testing.T) {
	calls := 0
	swapListen(t, func(string, string) (net.Listener, error) {
		calls++
		return nil, errors.New("bind: address already in use")
	})
	rec := &fatalRecorder{TB: t}
	if port := Free(rec); port != 0 {
		t.Fatalf("port %d after every try was taken", port)
	}
	if calls != tries {
		t.Fatalf("%d tries, want %d", calls, tries)
	}
	if !strings.Contains(rec.message, "no free loopback port") {
		t.Fatalf("message %q", rec.message)
	}
}

func TestFreeAddrIsAFreeLoopbackAddress(t *testing.T) {
	host, port, err := net.SplitHostPort(FreeAddr(t))
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("host %q: %v", host, err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < first || n >= first+span {
		t.Fatalf("port %q: %v", port, err)
	}
}

// checkProbes holds Free to the addresses it looked at: want of them, all on the loopback, not all the same port,
// and the port it returned is the last one it looked at.
func checkProbes(t *testing.T, seen []string, want, port int) {
	t.Helper()
	if len(seen) != want {
		t.Fatalf("%d probes %v, want %d", len(seen), seen, want)
	}
	distinct := map[string]bool{}
	for _, a := range seen {
		if host, _, err := net.SplitHostPort(a); err != nil || host != "127.0.0.1" {
			t.Fatalf("the probe of %q is not on the loopback", a)
		}
		distinct[a] = true
	}
	if len(distinct) == 1 {
		t.Fatalf("every try looked at the same port: %v", seen)
	}
	if last := seen[len(seen)-1]; last != addr(port) {
		t.Fatalf("Free returned %d, but the last port it looked at is %s", port, last)
	}
}

// recordListen is a listen that appends every address it is asked for to seen, refuses the first refuse of them,
// and opens a loopback listener on port 0 for the rest, so the test never depends on the port being free.
func recordListen(t *testing.T, seen *[]string, refuse int) func(string, string) (net.Listener, error) {
	t.Helper()
	return func(_, address string) (net.Listener, error) {
		*seen = append(*seen, address)
		if len(*seen) <= refuse {
			return nil, errors.New("bind: address already in use")
		}
		return net.Listen("tcp", "127.0.0.1:0")
	}
}

// swapListen replaces listen for the rest of the test.
func swapListen(t *testing.T, f func(network, address string) (net.Listener, error)) {
	t.Helper()
	previous := listen
	listen = f
	t.Cleanup(func() { listen = previous })
}

// swapDial replaces dial for the rest of the test.
func swapDial(t *testing.T, f func(address string) (net.Conn, error)) {
	t.Helper()
	previous := dial
	dial = f
	t.Cleanup(func() { dial = previous })
}

// ephemeralLow is the first port of the ephemeral range of the machine running the test.
func ephemeralLow(t *testing.T) int {
	t.Helper()
	var data []byte
	var err error
	switch runtime.GOOS {
	case "linux":
		data, err = os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	case "darwin":
		data, err = exec.Command("sysctl", "-n", "net.inet.ip.portrange.first").Output()
	default:
		t.Skipf("no known way to read the ephemeral range on %s", runtime.GOOS)
	}
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		t.Fatalf("no range in %q", data)
	}
	low, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("range %q: %v", data, err)
	}
	return low
}

// fatalRecorder keeps the message of a Fatalf instead of ending the test, so a test can look at a failure of Free.
type fatalRecorder struct {
	testing.TB
	message string
}

func (r *fatalRecorder) Helper() {}

func (r *fatalRecorder) Fatalf(format string, args ...any) { r.message = fmt.Sprintf(format, args...) }
