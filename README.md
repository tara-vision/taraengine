# taraengine

The Tara Vision inference engine: a local, privacy-first, OpenAI-compatible server for Gemma, GLM and Qwen
on NVIDIA Linux machines and Apple Silicon Macs. Built on llama.cpp by ggml-org.

taraengine owns the catalog, the pull, the presets, the pinned runtime, the process supervision, the API and
the measurement. The math is done by ggml through llama.cpp's server, pinned to one upstream build per release
and checksum-verified on install. No telemetry, ever.

## Status

This release serves: the pinned runtime, the catalog, the control API and the OpenAI-compatible passthrough.
taracode's next release runs on it. Service installers and the Homebrew cask come in a later release.

## Quick start

```sh
taraengine runtime install        # the pinned llama.cpp build for this machine
taraengine serve                  # listens on http://127.0.0.1:8272
taraengine pull gemma4:12b        # in a second terminal
curl http://127.0.0.1:8272/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"gemma4:12b","messages":[{"role":"user","content":"hello"}]}'
```

The chat request loads the model on first use. `taraengine doctor` shows the machine, the runtime and the daemon, and
recommends the catalog's model for the RAM it finds: for the machine of the daemon when one answers, and for this
machine when none does.

## Install

Download the binary for your machine from the [releases page](https://github.com/tara-vision/taraengine/releases):
`taraengine-darwin-arm64` for Apple Silicon, `taraengine-linux-amd64` or `taraengine-linux-arm64` for Linux. Every
release also carries `checksums.txt`, a keyless cosign signature of it as a Sigstore bundle
(`checksums.txt.sigstore.json`) and SLSA provenance.

```sh
BASE=https://github.com/tara-vision/taraengine/releases/latest/download
BIN=taraengine-darwin-arm64     # or taraengine-linux-amd64, or taraengine-linux-arm64
curl -fsSLO $BASE/$BIN -O $BASE/checksums.txt -O $BASE/checksums.txt.sigstore.json
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp 'https://github.com/tara-vision/taraengine/.github/workflows/release.yml@refs/tags/v.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
sha256sum --ignore-missing -c checksums.txt     # on macOS: shasum -a 256 --ignore-missing -c checksums.txt
chmod +x $BIN
sudo mv $BIN /usr/local/bin/taraengine
```

`taraengine version` prints the version you installed. The signature covers `checksums.txt`, and the checksums cover
every binary and package of the release, so verifying the one verifies the rest. The provenance can be checked with
[slsa-verifier](https://github.com/slsa-framework/slsa-verifier). If macOS blocks a binary that you downloaded with
a browser, remove the quarantine mark with `xattr -d com.apple.quarantine taraengine-darwin-arm64`.

On Linux the release also has a `.deb` and an `.rpm` for amd64 and arm64:

```sh
sudo dpkg -i taraengine_<version>_linux_amd64.deb
sudo rpm -i taraengine_<version>_linux_amd64.rpm
```

A package installs the binary as `/usr/bin/taraengine` and puts LICENSE and NOTICE under
`/usr/share/doc/taraengine/`. The Homebrew cask and an installer script come in a later release.

To build from source, clone the repository and run `make build`. `CONTRIBUTING.md` lists the checks that every
change has to pass.

## Runtime

taraengine does not carry llama.cpp inside its binary. `taraengine runtime install` fetches it once from the
llama.cpp releases on GitHub, and each taraengine release pins exactly one upstream build.

- **Pinned.** The binary embeds `internal/runtime/runtime.lock.yaml`: the build number and the sha256 of every
  release archive it may install, per platform.
- **Verified.** The download is hashed and compared with the lock before anything is unpacked. The unpacking refuses
  any entry that would land outside the target directory, and any link that does not point at a sibling file. The
  sha256 of the installed `llama-server` goes into a `manifest.json` next to it, and `taraengine serve` checks the
  binary against that file when it starts. It refuses to start when the runtime is missing or does not match, and it
  never downloads anything itself.
- **Kept whole.** The directory holds `llama-server`, its libraries and upstream's `LICENSE`. A failed install leaves
  the previous one as it was.
- **Moved on request.** `taraengine runtime update` moves to the build that the running taraengine pins, never to
  another one, and says `already at <build>` when there is nothing to do. `taraengine runtime status` says whether
  the runtime that the daemon would run on is verified, which is the `runtime_dir` of the configuration when it is set
  and the pinned build under the data directory otherwise, and `--dir-only` prints just that directory.

The build is chosen for the machine:

| Machine | Build |
|---------|-------|
| Apple Silicon | Metal |
| Linux with an NVIDIA driver of 580 or newer | CUDA 13.4 |
| Linux amd64 with an NVIDIA driver of 570 or newer | CUDA 12.8 |
| Any other Linux machine | CPU |

The driver version comes from `nvidia-smi`. The Linux arm64 builds are pinned too, but have not been tested on a
machine. Other systems, such as Intel Macs, are not supported in this release. A CUDA build needs the CUDA runtime
libraries of its major version. When `ldconfig` does not list them, `runtime install` also fetches upstream's CUDA
runtime bundle, a few hundred megabytes; `--cudart` fetches it always and `--no-cudart` never. `taraengine doctor`
shows the GPU, the RAM tier and the runtime it found, and names a model that needs a newer runtime than the pinned one.

The runtime lives in `<data dir>/runtime/<build>/`. The data directory is `~/Library/Application Support/taraengine`
on macOS, and `$XDG_DATA_HOME/taraengine` (default `~/.local/share/taraengine`) on Linux.

### Supervision

`taraengine serve` runs in the foreground and starts one `llama-server` in router mode on a loopback port of its
own. It writes the process id of that server to `child.pid` in the data directory each time the server starts, and
removes the file when it stops the server cleanly. If the server exits, taraengine ends the model processes that it
left behind and starts it again after a pause of 1, 2, 4 and then 8 seconds. The fifth exit within ten minutes ends
the restarts and marks the backend unhealthy until `serve` is started again.

When a daemon dies without stopping its server (it was killed, or the machine ran out of memory), the server and its
model processes keep running and keep their GPU memory. The next `serve` looks at `child.pid` before it starts its own
server. If that process is still there and runs the `llama-server` of the runtime in use, taraengine logs it and ends
its process group, with SIGTERM and, ten seconds later, SIGKILL, so that a stale server cannot take the memory that a
new load needs. A server of another runtime directory, or any other process, is left alone.

One daemon runs per data directory. `serve` takes a lock on the file `lock` in the data directory and writes its own
process id into it, and a second `serve` on the same data directory, whatever its listen address, stops at once with an
error that names the directory and that process id. The lock ends with the daemon, however it ends, and the file stays.

A model whose load through `/api/load` fails twice in a row is marked faulty and refused, on `/api/load` and on `/v1`,
until `taraengine load <model> --force`; `taraengine doctor` shows its exit code and the end of the server log. A load
that `/v1` starts by itself, and a server that dies in the middle of a chat, are not counted yet.

SIGINT or SIGTERM stops the daemon: requests in flight get five seconds, and `llama-server` gets SIGTERM and, ten
seconds later, SIGKILL.

## Models

The catalog is built into the binary. Each entry pins a Hugging Face file by size and sha256, together with the
projector that lets a model read images and the draft model that speeds it up (speculative decoding), when the
model has them. A pull fetches all of them.

| Name          | Tier            | Size    | Capabilities            |
|---------------|-----------------|---------|-------------------------|
| gemma4:e4b    | small (default) | 4.6 GB  | tools, thinking, vision |
| gemma4:12b    | 16 (default)    | 7.2 GB  | tools, thinking, vision |
| gemma4:26b    | 32              | 14.6 GB | tools, thinking, vision |
| gemma4:31b    | 48              | 18.0 GB | tools, thinking, vision |
| glm-4.7-flash | 32 (default)    | 18.2 GB | tools, thinking         |
| qwen3.6:27b   | 32              | 19.1 GB | tools, thinking, vision |
| qwen3.8:27b   | 32              | 19.0 GB | tools, thinking, vision |
| qwen3.6:35b   | 48 (default)    | 20.4 GB | tools, thinking, vision |

Size is the main file, as `taraengine list` shows it; the projector and the draft file come on top. A tier is the
RAM class a model is meant for. `small` suits any machine, and a machine with at least 14, 30 or 46 GiB of RAM counts
as `16`, `32` or `48`. `(default)` marks the model that `taraengine doctor` recommends for a tier. Each model keeps
the license of its authors; read it on the model's Hugging Face page before you use it.

```
$ taraengine list
NAME           STATUS     SIZE     TIER   CONTEXT  CAPABILITIES
gemma4:e4b     available  4.6 GB   small  32768    tools,thinking,vision
gemma4:12b     available  7.2 GB   16     32768    tools,thinking,vision
gemma4:26b     available  14.6 GB  32     32768    tools,thinking,vision
...
```

`STATUS` is `available` for a model in the catalog that is not on disk, `pulled` once it is, and `loading`, `loaded`
or `sleeping` while the engine runs it; `failed` and `faulty` mark a load that did not work. `CONTEXT` is the context
window in tokens that the model runs at, or would run at. Sizes below 1 GB, such as a small imported model, are shown in
MB with one decimal, and from 1 GB in GB.

### Pull, load, unload, remove

```sh
taraengine pull gemma4:12b                                                # a catalog model
taraengine pull hf://ggml-org/models-moved/tinyllamas/stories260K.gguf    # any GGUF file on Hugging Face
taraengine pull /srv/models/my-model.gguf                                 # a GGUF file on this machine
```

A pull resolves each file with Hugging Face, refuses it when the sha256 that Hugging Face reports is not the one the
catalog pins, checks the free disk space, and downloads into a `.part` file, so that running the same pull again
resumes an interrupted one. The complete file is hashed before it is moved into place; a file that fails the check is
deleted and never becomes a model. The command prints a line for every five percent of a download, in MB below 1 GB
and in GB from there.

```
$ taraengine pull gemma4:12b
resolving gemma-4-12B-it-Q4_0.gguf
downloading gemma-4-12B-it-Q4_0.gguf 361.5 MB / 7.2 GB
...
done gemma4:12b
```

A model from `hf://` or from a local path is named after its file without `.gguf`, has no tier and no promise of a
tuned preset, and its capabilities are marked `(inferred)`: taraengine reads them from the chat template while the
model runs and keeps them, so `taraengine list` still shows them after the model stops. Its native context window
comes from the file's header when it is pulled, and its default window is never larger; a load that asks for another
window gets that one. A local file is linked, not copied, and `taraengine rm` removes the link and never the file. A
name that the catalog owns is refused with a 409, and so is one that llama-server would serve under the same id as
another model's name (`gemma4:12b` and `gemma4-12b` are one id), and a different file under a name that an import
already holds.

A gated or private repository needs `hf_token` in the configuration, or `TARAENGINE_HF_TOKEN` in the environment of
`serve`. To use a catalog of your own, put a `registry.yaml` in the data directory; it replaces the built-in one. It
is checked like the built-in one: a key that taraengine does not know, a `preset` key that does not tune how a model
runs (the error lists the keys that do), two names that llama-server would serve under one id, and a second default
for a tier are refused with an error that names them. The engine itself sets where a model is, its context window, its
projector and its draft.

```sh
taraengine load gemma4:12b                  # loads it, and waits until it is ready
taraengine load gemma4:12b --context 16384  # loads it at another window, which the daemon remembers for the model
taraengine ps                               # what is loaded, and what it holds in memory
taraengine unload gemma4:12b
taraengine rm gemma4:12b                    # deletes the model and its projector and draft file from disk
```

```
$ taraengine load gemma4:12b
loaded gemma4:12b at 32768 tokens of context, 10.0 GiB measured
$ taraengine ps
NAME        STATUS  CONTEXT  MEMORY
gemma4:12b  loaded  32768    10.0 GiB measured
```

The context window defaults to 32768 tokens, or to the model's native maximum when that is smaller. `context` in the
configuration changes the default, and `context_by_tier` sets it per RAM tier. The memory of a loaded model is
measured on NVIDIA, as the GPU memory that the driver reports for the model's process. On a Mac the GPU has no
per-process figure, so the number is the process's resident memory and is labeled `estimated`. A dash means that
there is no figure.
A model that a request through `/v1` brings up on its own gets its figure from a later read of the model list
(`taraengine list`, `taraengine ps` or the API) that finds no load, unload or catalog change under way. A model left
idle for `sleep_idle_seconds` sleeps: `ps` shows `sleeping` with the figure of its last run, the next request wakes
it, and `taraengine load` of a sleeping model at its window answers at once.

## API

The control API is under `/api` on the listen address, `http://127.0.0.1:8272` by default. Bodies are JSON, and an
error is `{"error":{"message":"...","hint":"..."}}`.

| Route | What it does | Errors |
|-------|--------------|--------|
| `GET /api/health` | `{"status":"ok"}` or `"degraded"`, with the backend's name. Open without a key; `HEAD` works too. | |
| `GET /api/version` | The taraengine version, the runtime build and the platform. | |
| `GET /api/models` | The catalog and the pulled models: name, status, size, family, parameters, quantization, context, capabilities, tier, default and memory. | 503 |
| `GET /api/models/{name}` | One model in detail: also its native context, a digest, the preset in effect and, after a failure, the end of the server log. | 400, 404, 503 |
| `GET /api/loaded` | The loaded and sleeping models, with their status (`loaded` or `sleeping`), context window and memory (`bytes` and `source`, `measured` or `estimated`). | 503 |
| `POST /api/load` | `{"model":"...","context":N,"force":false}`, the last two optional. `context` is from 512 to 4194304 tokens; 0 or no value means the model's usual window. Loads the model and answers with it once it is up; a model that sleeps at that window is answered at once and wakes on its next request, and a model that is still loading is waited for. | 400, 404, 409 faulty, 502 load failed, 503, 504 over `load_timeout` |
| `POST /api/unload` | `{"model":"..."}`. Answers 204. | 400, 404, 503 |
| `POST /api/pull` | `{"model":"..."}` with a catalog name, `hf://owner/repo/file.gguf` or an absolute path. Streams the progress as server-sent events (`resolving`, `downloading`, `verifying`, `done`, `error`); with `?wait=1` it answers once, with `{"status":"done"}`. | 400 bad reference, 404 unknown name, 409 that name is busy or taken by another model or file, 502 |
| `DELETE /api/models/{name}` | Removes a pulled model and its sibling files. A model imported from a path or `hf://` whose file is already gone is dropped from the catalog all the same. Answers 204. | 400, 404, 409, 503 |
| `GET /api/doctor` | The machine (GPU, memory, tier, driver), the runtime status and the recommended model per tier. | |
| `/v1/*` | llama-server's OpenAI and Anthropic API, proxied. | 400 no `model` or not JSON, 401 without the key, 403 foreign origin, 404 model not pulled, 405 any other method but OPTIONS, 409 faulty model, 503 |

```sh
curl http://127.0.0.1:8272/api/loaded
curl http://127.0.0.1:8272/api/load -H 'Content-Type: application/json' -d '{"model":"gemma4:12b","context":16384}'
curl http://127.0.0.1:8272/api/loaded -H "Authorization: Bearer $TARAENGINE_API_KEY"   # when api_key is set
```

`/v1` is llama-server's OpenAI and Anthropic surface, proxied without buffering the answers, so streams arrive as they
are produced: `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/models`, `/v1/messages` and
`/v1/messages/count_tokens` among them.

- **Models.** Name the model by its catalog name, such as `gemma4:12b`. A request for a pulled model that is not
  loaded loads it first, and with `models_max` at 1 it replaces the model that was loaded. A request has to name a
  pulled model: a name the engine does not know, or has not pulled, is a 404 that lists up to three of the nearest
  pulled names; a model marked faulty is a 409 until `taraengine load <model> --force`; and a body without a `model`,
  or one that is not a JSON object, is a 400. `/v1/models` lists the pulled models under their catalog names and
  nothing that the router found elsewhere. Only GET, HEAD and POST are served: OPTIONS is answered by the daemon
  itself, as described under Privacy and security, and every other method is a 405 with an `Allow` header. The `model`
  field of a chat answer carries the id that llama-server knows the model by, which writes a hyphen for the colon:
  `gemma4-12b`.
- **Thinking.** `"chat_template_kwargs": {"enable_thinking": false}` switches thinking off and `true` on, and
  `"reasoning_effort": "low"`, `"medium"` or `"high"` sets the effort where the model's template honors it. The
  thoughts come back in `reasoning_content`, on the message and on streamed deltas.
- **Timings.** Every chat answer carries `usage` and `timings` (`prompt_n`, `prompt_ms`, `prompt_per_second`,
  `predicted_n`, `predicted_ms`, `predicted_per_second`, `cache_n`); in a stream they arrive on the last chunk.
  Answers that `/v1` passes on carry an `X-Taraengine-Version` header.
- **Limits.** A request body over 32 MiB is refused with 413.

The JSON types of the API are in `pkg/api`, and a typed Go client is in `pkg/client`.

## Configuration

taraengine needs no configuration file: every key has a default. When there is one, it is
`$XDG_CONFIG_HOME/taraengine/config.yaml` or `~/.config/taraengine/config.yaml`, on macOS too, and `--config FILE`
names another, which then has to exist. A key that this version does not know is an error that names the key and its
line and lists the accepted keys, so that a misspelled `api_key` cannot leave an engine open without a word.
`serve` also takes `--listen`, `--data-dir`, `--runtime-dir` and `--api-key`, which win over the file. The environment
variables `TARAENGINE_API_KEY` and `TARAENGINE_HF_TOKEN` win over the file as well. The other commands read the same
file for the data directory. They reach the daemon at `--host`, or, when `--host` is left out, at the `listen` address
of the file, `http://127.0.0.1:8272` when there is none. The key is `--api-key`, else `TARAENGINE_API_KEY`, and either
goes with any address. The `api_key` of the file goes only with the address of the file: it belongs to the daemon that
the file configures, so with `--host` it is not used, and a daemon that asks for a key then needs `--api-key` or
`TARAENGINE_API_KEY`.

Keep the key in the configuration file (with a mode that only you can read) or in the environment. Do not put it in
`--api-key` on a machine that other people use: the arguments of a command show in the process list of every user.

| Key | Default | What it does |
|-----|---------|--------------|
| `listen` | `127.0.0.1:8272` | The address the API listens on. See Privacy and security before you change it. |
| `api_key` | none | The key that every route but `GET` and `HEAD /api/health` requires, as a Bearer token. |
| `allowed_origins` | none | Web origins whose pages may use the API besides the pages served from `localhost`, `127.0.0.1` and `[::1]`. Write each as the browser sends it, `scheme://host` or `scheme://host:port`, such as `https://app.example`; an origin is matched whole, the case aside, and a value that is not an origin is a configuration error. See Privacy and security. |
| `data_dir` | the data directory above | Where the runtime, the models, the presets, the state and the logs live. |
| `models_max` | `1` | How many models stay loaded together. Loading another one replaces the first. |
| `sleep_idle_seconds` | `900` | Seconds without a request after which a loaded model goes to sleep, at least 1. The next request wakes it. `-1` never sleeps; `0` is refused. |
| `context` | `32768` | The default context window in tokens, at least 512, and never more than the model's own maximum. |
| `context_by_tier` | none | A context window per RAM tier, such as `"16": 16384`; the tiers are `small`, `16`, `32`, `48` and `other` for models outside the catalog. |
| `hf_token` | none | A Hugging Face token for gated or private repositories. It is sent to Hugging Face only and never logged. |
| `load_timeout` | `600` | Seconds that a load may take before it is given up, at least 1. Raise it for a slow disk. |
| `runtime_dir` | none | A runtime directory to run instead of the pinned build under the data directory. It has to hold the `manifest.json` that `runtime install` writes, and it is verified like the pinned build. |
| `preset_global` | none | Extra llama-server options for every model, as preset keys (the long option name without the dashes), laid over the engine's own: `jinja: "true"`, `flash-attn: auto`, `n-gpu-layers: "999"`, `reasoning-format: deepseek` and `load-on-startup: "false"`. Quote numbers. |

```yaml
# ~/.config/taraengine/config.yaml
api_key: change-me
sleep_idle_seconds: -1
context_by_tier:
  "16": 16384
  "48": 65536
preset_global:
  cache-type-k: q8_0
  cache-type-v: q8_0
```

The data directory holds:

```
runtime/<build>/    llama-server, its libraries, upstream's LICENSE and manifest.json
models/             GGUF files by owner and repository, links to local files, index.json
presets.ini         the llama-server presets, generated on every change: do not edit
registry.yaml       optional: a catalog that replaces the built-in one
state.json          faulty-model marks, remembered context windows, the last model loaded
lock                held by the running daemon, with its process id: a second serve on this directory is refused
child.key           the generated key of the supervised llama-server (mode 0600)
child.pid           the process id of the supervised llama-server, removed by a clean stop, read at the next start
llama-cache/        an empty download cache for llama-server, so that it never serves a model outside the catalog
logs/               taraengine.log and llama-server.log, rotated at 20 MB with three old files kept
```

## Privacy and security

**No telemetry.** taraengine sends no usage data, makes no update checks and files no crash reports. It talks to
two hosts, and only when you run a command that needs them: `huggingface.co` on `taraengine pull`, and `github.com`
on `taraengine runtime install` and `runtime update`. Both redirect downloads to their own content delivery hosts
(under `hf.co` and `githubusercontent.com`). Nothing else leaves the machine.

A pull is carried out by the daemon, so it is the `serve` process that connects to Hugging Face, during the pull and
for a short while after it. At any other time neither `serve` nor the `llama-server` it supervises holds a connection
beyond the loopback. A Hugging Face token is sent to Hugging Face only, and it is never logged.

What can reach the API:

- **Localhost by default.** `listen` is `127.0.0.1:8272`. To serve other machines, set `listen`, for example to
  `0.0.0.0:8272`, and set an `api_key`. taraengine logs a warning when it listens beyond localhost without a key.
- **A Bearer key.** With `api_key` set, every route but `GET` and `HEAD /api/health` needs
  `Authorization: Bearer <key>`, and a missing or wrong key is a 401. No other header counts: a client that sends its
  key as `x-api-key` is refused.
- **Web pages.** A page that you open in a browser can send requests to `127.0.0.1:8272`, and the browser attaches an
  `Origin` header to them. The daemon answers a request with an `Origin` header with 403 unless the page is served from
  `localhost`, `127.0.0.1` or `[::1]` (http or https, any port) or its origin is listed under `allowed_origins`, with or
  without an `api_key`. The preflight that a browser sends before a request across origins
  is answered by the daemon, for the allowed origins only, and is never passed on to `llama-server`. The CORS headers
  that `llama-server` puts on its own answers are removed, so that the headers of the daemon are the only ones a page
  ever sees. Programs that are not browsers, such as curl, the OpenAI and Anthropic SDKs and taracode, send no `Origin`
  and are not asked about it.
- **JSON only.** Every `POST` under `/api` and `/v1` must be `application/json`, a charset parameter allowed, and
  anything else is a 400 with a hint. A web page can make the browser post a form or plain text to any address
  without asking first, and the body would otherwise be read as JSON all the same. The OpenAI and Anthropic SDKs send
  `application/json`; with curl, send `-H 'Content-Type: application/json'`.
- **The Host header.** With no `api_key`, the API refuses a request whose Host is not one of its own addresses
  (`localhost`, `127.0.0.1`, `[::1]` or the host in `listen`) with a 403. That keeps out a web page that points its own
  DNS name at 127.0.0.1. With a key set, the key does that job. A daemon that listens on every interface skips the
  check.
- **The supervised server.** The `llama-server` that taraengine starts listens on a loopback port with a generated
  key. The key is in `child.key` in the data directory, readable by you only, and `llama-server` reads it from that
  file, so it is on no command line. The `/v1` proxy swaps your key for it. The processes that `llama-server` starts
  for each loaded model listen on random loopback ports and do not check that key, so any program on the same machine
  can reach them, as it can with any inference server on localhost. Keep that in mind on a machine that you share with
  people you do not trust. The server runs with an environment of its own: `PATH`, `HOME`, `TMPDIR`, `LANG`, the
  `LC_*` and `CUDA_*` variables as you have them, and the runtime's library paths, and nothing else. It inherits neither
  your API key nor your Hugging Face token, nor any `LLAMA_ARG_*` setting, and its `LLAMA_CACHE` is the empty
  `llama-cache` directory in the data directory, so it never serves a model that is not in the catalog.
- **Verified downloads.** Runtime archives are checked against the sha256 values pinned in the binary, and model files
  against the values pinned in the catalog, or for `hf://` files against the checksum that Hugging Face reports. A
  model name is checked before it touches the file system, so it cannot carry a path.
- **Logs.** `logs/taraengine.log` and `logs/llama-server.log` in the data directory are readable by you only. taraengine
  does not log prompts or answers, and the supervised `llama-server` runs at warning verbosity.

To report a vulnerability, see `SECURITY.md`.

## Credits

Built on [llama.cpp](https://github.com/ggml-org/llama.cpp) by ggml-org (MIT). `taraengine runtime install` downloads an
unmodified upstream build, and upstream's LICENSE stays next to it in the data directory. See NOTICE.

taraengine itself is released under the MIT License, see LICENSE.
