// Package porttest picks loopback ports for tests that give a port number to another process, or to code that
// listens on it later.
//
// A port read from a listener on port 0 and closed again comes from the kernel's ephemeral range, which every other
// listener on port 0 and every outgoing connection on the machine draws from as well, so one of them can take the
// port before the test binds it. Free picks below that range, which starts at 32768 on Linux and at 49152 on macOS:
// the kernel never hands out a port there by itself.
package porttest

import (
	"math/rand/v2"
	"net"
	"strconv"
	"testing"
	"time"
)

// Free picks from [first, first+span), and gives up after tries ports that are all taken.
const (
	first = 20000
	span  = 12000
	tries = 100
)

// listen and dial are net.Listen and a bounded net.DialTimeout; a test of this package replaces them.
var (
	listen = net.Listen
	dial   = func(address string) (net.Conn, error) { return net.DialTimeout("tcp", address, time.Second) }
)

// Free is a loopback port nobody listens on right now, from below the kernel's ephemeral range. The test fails when
// every port Free tried was taken.
func Free(t testing.TB) int {
	t.Helper()
	for range tries {
		port := first + rand.IntN(span) //nolint:gosec // G404: a test port needs spread, not secrecy
		if free(addr(port)) {
			return port
		}
	}
	t.Fatalf("porttest: no free loopback port in %d tries between %d and %d", tries, first, first+span-1)
	return 0
}

// free reports whether nobody listens on the loopback address: a listener on it opens, and a connection to it is
// refused. The connection finds a listener on the wildcard address, which on macOS does not keep a loopback listener
// from opening.
func free(address string) bool {
	l, err := listen("tcp", address)
	if err != nil {
		return false
	}
	_ = l.Close()
	c, err := dial(address)
	if err != nil {
		return true
	}
	_ = c.Close()
	return false
}

// FreeAddr is the loopback address of a port from Free.
func FreeAddr(t testing.TB) string {
	t.Helper()
	return addr(Free(t))
}

func addr(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }
