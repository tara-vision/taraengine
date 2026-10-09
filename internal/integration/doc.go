// Package integration holds the end-to-end tests of taraengine: the built binary against the real pinned llama.cpp
// runtime and the one-megabyte stories260K model, from serve to pull, load, chat, ps, unload, rm and stop. The test
// files carry the integration build tag, so that go test ./... leaves them alone; scripts/integration.sh, which
// make integration runs, installs the runtime and runs them.
package integration
