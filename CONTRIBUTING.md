# Contributing

- One pull request per change, against `main`, with tests: every changed line is covered by the package's
  own tests.
- Run `make test lint vuln coverage-gate hygiene` before pushing. `make integration` runs the built binary against
  the real pinned runtime and a one-megabyte model; it needs network and takes about a minute.
- A new or changed model entry in `internal/registry/registry.yaml`: write `sha256: <pin>` and `size: 0`, run
  `go run ./tools/pinregistry internal/registry/registry.yaml`, and review what it filled in. The catalog table in
  the README follows the registry, so change it in the same pull request; `go test ./...` compares the two.
- A new llama.cpp build: `scripts/pin-runtime.sh <build> internal/runtime/runtime.lock.yaml`, in a pull request of its
  own. CI downloads the assets again and compares the checksums. In the same pull request:
  - raise `min_runtime` in `internal/registry/registry.yaml` for every model that needs the new build, so that
    `taraengine doctor` can tell a machine with an older runtime;
  - list the CUDA runtime bundles of the new build, one file for each `cudart` asset of the lock. The install
    expects every library under one top-level directory, with `libcudart.so.<major>` among them, and `go test ./...`
    fails until each asset has a list that shows that layout.

    ```sh
    curl -sL <asset url> | tar -tzf - > internal/runtime/testdata/cudart/<asset>.txt
    ```
- `make snapshot` builds the release artifacts into `dist/` without publishing or signing anything.
- Plain hyphens and straight quotes in code, comments and docs.
- Commit messages: `type: description` (feat, fix, refactor, docs, test, chore, ci).
