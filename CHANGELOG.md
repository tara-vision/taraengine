# Changelog

All notable changes to taraengine will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-MM-DD

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
