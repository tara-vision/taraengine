# Changelog

All notable changes to taraengine will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- `taraengine service install | uninstall | start | stop | restart | status` runs taraengine as a service: a LaunchAgent
  of the user on macOS, a systemd system service under its own `taraengine` user on Linux. A stop is remembered until
  a start, and `service status` ends with 3 when the service does not run.
- `serve --service`, which the service definitions run: the daemon fetches the llama.cpp build its binary pins when
  the build is missing, checked against the embedded lock before anything is unpacked, and tries again after 10, 30
  and 60 seconds, then every five minutes; it removes the builds it no longer pins, and keeps stderr to warnings and
  errors.
- `taraengine update` brings taraengine to the latest release through the channel it came from: Homebrew, a deb or rpm
  package, or the binary in place. `--check` only reports. It looks the release up only when it is run.
- The commands that talk to the engine wait for one that is starting, and on macOS the first of them starts the
  installed agent.
- `install.sh`, attached to every release and listed in `checksums.txt`, installs the binary and the service in one
  command.
- The Homebrew cask `tara-vision/taracode/taraengine`, for Apple Silicon Macs.
- The deb and rpm packages install, upgrade and remove the system service.

### Changed
- `serve` answers from the moment it binds: `GET /api/health` says `starting` and `GET /api/version` answers as it
  always does, while every other route answers 503 with `Retry-After` until the daemon is ready.
- `pkg/api`: `Health.Status` can be `starting`; `HealthOK`, `HealthDegraded` and `HealthStarting` name the values.
- `taraengine doctor` reports the runtime the daemon runs when a daemon answers, and it reports an engine that is
  starting without waiting for it.
- The macOS binary is signed with the Developer ID of Tara Vision, LLC and notarized by Apple.

## [0.2.0] - 2026-10-13

### Added
- `GET /api/loaded` gives each model a `status`, `loaded` or `sleeping`, and `taraengine ps` shows it in a STATUS
  column; `taraengine load` and `taraengine doctor` say when a model sleeps.
- An import from `hf://` or a local path takes its native context window from the file's header when it is pulled,
  and its default window is no larger; imports pulled by 0.1.0 get theirs when the daemon starts.
- An import keeps the capabilities it showed while it ran, across unloads and restarts.
- `pkg/client`: `Error.NotJSON` marks a refusal whose body is not the engine's error JSON; such a body is shown as
  one line of at most 300 bytes.

### Changed
- The memory figure and the capabilities of a running model are recorded whoever started it: a model that a `/v1`
  request brings up gets them from a later read of the model list. Reading a model's properties never makes
  llama-server load it.
- `POST /api/load` answers at once for a model that sleeps at the requested window, and waits for a model that is
  still loading instead of refusing the load.
- The end of llama-server's log that `GET /api/models/{name}` carries keeps line breaks and tabs and drops every other
  control character.
- A pre-release tag such as `v0.2.0-rc.1` is released with the `[Unreleased]` section of this file as its notes,
  unless it has a section of its own.
- The catalog runs a q8_0 KV cache (`cache-type-k` and `cache-type-v`) for gemma4:e4b, glm-4.7-flash, qwen3.6:27b,
  qwen3.8:27b and qwen3.6:35b, which holds less memory at the same window; the README's catalog table names each
  model's KV cache. A key set in `preset_global` wins over a catalog model's own preset for that key, and with
  flash attention turned off there a catalog model keeps an f16 V cache, which llama.cpp quantizes only with it.

## [0.1.0] - 2026-10-09

### Added
- `taraengine serve`: supervises one llama-server (upstream build b11514, pinned and checksum-verified) in
  router mode and serves the control API on 127.0.0.1:8272 with an OpenAI-compatible `/v1`.
- `taraengine runtime install | update | status`: the pinned build per platform, CUDA 12.8 or 13.4 by driver,
  the CPU build as the fallback, Metal on Apple Silicon.
- `taraengine pull | list | ps | load | unload | rm | doctor`: a curated registry of eight models pinned to
  Hugging Face artifacts with their projector and draft siblings, plus `hf://` and local escape hatches.
- Measured GPU memory per loaded model on NVIDIA, estimated resident memory on macOS.
- `.deb` and `.rpm` packages that install the binary under `/usr/bin` and carry LICENSE and NOTICE.

### Security
- The API refuses web pages of other origins: only pages served from localhost, 127.0.0.1 or [::1] and the
  `allowed_origins` of the configuration may use it. The daemon answers preflights itself, removes the CORS headers of
  llama-server, and requires `application/json` on every POST under `/api` and `/v1`.
- The supervised llama-server runs with an allowlisted environment, without the API key and the Hugging Face token,
  and with an empty download cache, so it serves only the models of the catalog.
- A key in the configuration file that this version does not know is an error that names the key.
- Release workflows pin every action to a commit; CodeQL, Dependabot and `goreleaser check` run in CI.
