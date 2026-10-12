package cli

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/tara-vision/taraengine/internal/config"
	"github.com/tara-vision/taraengine/pkg/client"
)

// keyHint is what a refused key needs: the ways this command line can give one. The key of the config file goes only
// with the address of the config file, which is why --host has to be left out for it.
const keyHint = "pass --api-key or set TARAENGINE_API_KEY, or leave out --host and put api_key in the config file"

// defaultDaemon is where a daemon listens when nothing says otherwise.
const defaultDaemon = "http://127.0.0.1:8272"

// resolved returns the flags with the address of the daemon and its key filled in. The address is --host, else the
// listen address of the config file. The key is --api-key, else TARAENGINE_API_KEY, else the api_key of the config
// file, and the file's key goes only with the file's address: the file describes the daemon it configures, and its
// key is not for another one that the command line names. So the file is read only when --host is absent, and a
// file that is needed but cannot be loaded is an error and not a client without a key.
func (f *globalFlags) resolved() (*globalFlags, error) {
	out := f.withEnvironment()
	if out.host != "" {
		return &out, nil
	}
	cfg, err := loadConfig(f)
	if err != nil {
		return nil, err
	}
	return out.withConfig(cfg), nil
}

// withEnvironment returns the flags with TARAENGINE_API_KEY as the key when --api-key gave none. The person who sets
// it speaks for every address they then name.
func (f globalFlags) withEnvironment() globalFlags {
	if f.apiKey == "" {
		f.apiKey = os.Getenv("TARAENGINE_API_KEY")
	}
	return f
}

// withConfig returns the flags with the listen address of cfg, when --host named none, and then with the key of cfg
// in place of a missing one. The key of cfg is the file's, or TARAENGINE_API_KEY, which config.Load lays over it.
// Flags that name an address get nothing from cfg.
func (f globalFlags) withConfig(cfg config.Config) *globalFlags {
	if f.host != "" {
		return &f
	}
	f.host = daemonURL(cfg.Listen)
	if f.apiKey == "" {
		f.apiKey = cfg.APIKey
	}
	return &f
}

// daemonURL is the address that reaches a daemon which listens on listen. A daemon that listens on every interface
// is reached on the loopback address, where it always answers.
func daemonURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return defaultDaemon // the configuration refuses a listen address that does not split
	}
	switch ip := net.ParseIP(host); {
	case host == "":
		host = "127.0.0.1"
	case ip != nil && ip.IsUnspecified() && ip.To4() != nil:
		host = "127.0.0.1"
	case ip != nil && ip.IsUnspecified():
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// client is the typed client of the daemon at host, with the key. Resolve the flags first.
func (f *globalFlags) client() *client.Client {
	return client.New(f.host, f.apiKey)
}

// explain turns a failure that the user can act on into the sentence they need; other errors pass through. Only a
// connection that could not be opened at all means that the engine is not running: the client reports no other
// failure as a connection error, so a stream cut in the middle of a pull is not mistaken for one. A key that the
// daemon refuses keeps the daemon's message but gets this command line's hint in place of the daemon's, which speaks
// of a Bearer header that no one types here.
func (f *globalFlags) explain(err error) error {
	var refused *client.Error
	switch {
	case err == nil:
		return nil
	case client.IsConnectionError(err):
		return fmt.Errorf("taraengine is not running at %s: start it with `taraengine serve` "+
			"(or `taraengine doctor` for details)", f.host)
	case errors.As(err, &refused) && refused.Status == http.StatusUnauthorized:
		keyed := *refused
		keyed.Hint = keyHint
		return &keyed
	}
	return err
}
