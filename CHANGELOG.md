# Changelog

All notable changes to taraengine will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
