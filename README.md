# taraengine

<p align="center">
  <strong>The Tara Vision Inference Engine</strong><br>
  Local, private and OpenAI-compatible: Gemma, GLM and Qwen on NVIDIA Linux machines and Apple Silicon Macs.<br>
  Built on llama.cpp. Made for taracode. No telemetry, ever.
</p>

<p align="center">
  <a href="https://github.com/tara-vision/taraengine/releases"><img src="https://img.shields.io/github/v/release/tara-vision/taraengine?style=for-the-badge&logo=github&color=blue" alt="Release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-green?style=for-the-badge" alt="License: MIT"></a>
  <a href="https://github.com/sponsors/tara-vision"><img src="https://img.shields.io/badge/Sponsor-%E2%9D%A4-ea4aaa?style=for-the-badge&logo=githubsponsors&logoColor=white" alt="Sponsor tara-vision"></a>
  <a href="https://github.com/tara-vision/taraengine/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/tara-vision/taraengine/ci.yml?branch=main&style=for-the-badge&logo=github&label=CI" alt="CI"></a>
  <a href="https://app.codecov.io/github/tara-vision/taraengine"><img src="https://img.shields.io/codecov/c/github/tara-vision/taraengine?branch=main&style=for-the-badge&logo=codecov&logoColor=white&label=coverage" alt="Coverage"></a>
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.27-00ADD8?style=for-the-badge&logo=go&logoColor=white" alt="Go Version"></a>
</p>

<p align="center">
  <a href="https://github.com/tara-vision/taraengine/stargazers"><img src="https://img.shields.io/github/stars/tara-vision/taraengine?style=for-the-badge&logo=github&color=yellow" alt="Stars"></a>
  <a href="https://github.com/tara-vision/taraengine/network/members"><img src="https://img.shields.io/github/forks/tara-vision/taraengine?style=for-the-badge&logo=github&color=orange" alt="Forks"></a>
  <a href="https://github.com/tara-vision/taraengine/issues"><img src="https://img.shields.io/github/issues/tara-vision/taraengine?style=for-the-badge&logo=github&color=red" alt="Issues"></a>
</p>

<p align="center">
  <a href="#quick-start"><img src="https://img.shields.io/badge/Quick_Start-blue?style=flat-square" alt="Quick Start"></a>
  <a href="#commands"><img src="https://img.shields.io/badge/Commands-teal?style=flat-square" alt="Commands"></a>
  <a href="#service"><img src="https://img.shields.io/badge/Service-blueviolet?style=flat-square" alt="Service"></a>
  <a href="#models"><img src="https://img.shields.io/badge/Models-purple?style=flat-square" alt="Models"></a>
  <a href="#api"><img src="https://img.shields.io/badge/API-indigo?style=flat-square" alt="API"></a>
  <a href="#configuration"><img src="https://img.shields.io/badge/Configuration-green?style=flat-square" alt="Configuration"></a>
  <a href="CONTRIBUTING.md"><img src="https://img.shields.io/badge/Contributing-orange?style=flat-square" alt="Contributing"></a>
</p>

---

## Why taraengine?

- **Everything above the kernels** - taraengine owns the catalog, the pull, the presets, the pinned runtime, the
  process supervision, the API and the measurement; the math is done by ggml through llama.cpp's server
- **One pinned runtime** - Each release pins exactly one llama.cpp build, downloaded once, hashed against the
  lock before it is unpacked and checked again every time the engine starts
- **A curated catalog** - Models pinned by size and sha256, with the projector and the draft model they come
  with, and a KV cache chosen for each on an RTX 5090 board
- **OpenAI and Anthropic APIs** - `/v1` is llama-server's surface, proxied as it streams, with tools, thinking
  and vision where the model has them
- **Measured** - Every chat answer carries its timings, and `taraengine ps` shows what a loaded model holds in
  memory, measured on NVIDIA and estimated on a Mac
- **Privacy-first** - Localhost by default and a Bearer key when you set one; no telemetry, no update checks of
  its own, and the network only for the downloads that a pull, an install or an update needs
- **No account required** - Open source, MIT-licensed; install it and run it

## Status

This release serves the pinned runtime, the catalog with its tuned presets, the control API and the OpenAI-compatible
passthrough, on Apple Silicon Macs and NVIDIA Linux machines. It installs with one command and runs as a service, which
fetches the llama.cpp build that its binary pins. taracode's next release runs on it.

## Quick Start

### 1. Install taraengine

On a Mac:

```sh
brew install --cask tara-vision/taracode/taraengine
```

On Linux:

```sh
curl -fsSL https://engine.tara.vision/install.sh | bash
```

Both install the binary and the service, a LaunchAgent on macOS and a systemd service on Linux, and the service
fetches the pinned llama.cpp build for the machine on its own. [Install](#install) has the packages, the verified
download by hand and the build from source.

### 2. Pull a model

```sh
taraengine pull gemma4:12b
```

On a Mac the first command that needs the engine starts the service. A command waits while the engine fetches its
runtime. `gemma4:12b` suits 16 GB of RAM. On a smaller machine, pull the model that `taraengine doctor` names, and use
that name in step 3.

### 3. Ask it something

```sh
curl http://127.0.0.1:8272/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"gemma4:12b","messages":[{"role":"user","content":"hello"}]}'
```

The chat request loads the model on first use. `taraengine doctor` shows the machine, the runtime and the daemon, and
recommends the catalog's model for the RAM it finds: for the machine of the daemon when one answers, and for this
machine when none does. taracode's next release finds the engine at this address without configuration, and other
OpenAI-compatible clients can use it now; a web page from another origin needs `allowed_origins`.

## Commands

| Command                        | What it does                                                                                                               |
|--------------------------------|----------------------------------------------------------------------------------------------------------------------------|
| `taraengine serve`             | Runs the engine: supervises llama-server and serves the API on the listen address                                          |
| `taraengine pull <model>`      | Pulls a catalog model with its siblings, or registers another GGUF (`hf://...` or an absolute path)                        |
| `taraengine list`              | Lists the catalog and the pulled models                                                                                    |
| `taraengine load <model>`      | Loads a model, optionally at a context window (`--context`) or past a faulty mark (`--force`), and waits until it is ready |
| `taraengine ps`                | Shows the loaded models and their memory                                                                                   |
| `taraengine unload <model>`    | Unloads a model                                                                                                            |
| `taraengine rm <model>`        | Removes a pulled model and its siblings from disk                                                                          |
| `taraengine doctor`            | Shows the machine, the runtime and the daemon, and names the model for your RAM                                            |
| `taraengine service install`   | Installs the service for this binary and starts it: at login on macOS, at boot on Linux                                    |
| `taraengine service start`     | Starts the service, and has it start at login or boot again                                                                |
| `taraengine service stop`      | Stops the service until the next `service start`                                                                           |
| `taraengine service restart`   | Restarts the service, or starts it when it does not run                                                                    |
| `taraengine service status`    | Shows the service and the engine it runs; ends with 3 when it does not run                                                 |
| `taraengine service uninstall` | Stops the service and removes it; the models and the configuration stay                                                    |
| `taraengine update`            | Updates taraengine to the latest release the way it was installed (`--check` only reports)                                 |
| `taraengine runtime install`   | Downloads, verifies and unpacks the pinned llama.cpp build                                                                 |
| `taraengine runtime update`    | Moves to the build this taraengine pins                                                                                    |
| `taraengine runtime status`    | Shows the installed build, and whether it is verified                                                                      |
| `taraengine version`           | Prints the taraengine version                                                                                              |

The commands that talk to the daemon (`pull`, `list`, `ps`, `load`, `unload`, `rm` and `doctor`) take its address and
key from the configuration file; `--host`, `--api-key` and `TARAENGINE_API_KEY` win over it, as
[Configuration](#configuration) explains. Every one of them but `doctor` waits for an engine that is starting, and
prints what it is doing. `taraengine <command> --help` lists the flags.

## Install

**macOS:** the Homebrew cask, from the taracode tap.

```sh
brew install --cask tara-vision/taracode/taraengine
```

The cask installs the binary, signed by Tara Vision, LLC and notarized by Apple, and the service. `brew upgrade` moves
both to a new release and restarts the engine; `brew uninstall --cask taraengine` stops the service and removes it,
and `--zap` removes the models, the configuration and the logs as well.

**Linux, or a Mac without Homebrew:** the installer script.

```sh
curl -fsSL https://engine.tara.vision/install.sh | bash
```

It downloads the binary for the machine, checks it against the release's `checksums.txt`, installs it to
`/usr/local/bin` (with sudo when the directory needs it), and installs the service: as you on macOS, as root on Linux.
Its variables go after the pipe: `curl -fsSL https://engine.tara.vision/install.sh | TARAENGINE_VERSION=v0.3.0 bash`
installs that release (v0.3.0 or later), `INSTALL_DIR=<dir>` another directory (on Linux one outside `/home`, which the
service cannot reach), and `TARAENGINE_NO_SERVICE=1` the binary alone. On Linux it also installs the OpenMP library that
llama.cpp needs (`libgomp1`, or `libgomp`) with the system's package manager when the machine lacks it. Running it again
updates taraengine.

**Packages:** the release has a `.deb` and an `.rpm` for amd64 and arm64, which depend on the OpenMP library that
llama.cpp needs. On Debian and Ubuntu:

```sh
sudo apt install ./taraengine_<version>_linux_amd64.deb
```

On Fedora and RHEL:

```sh
sudo dnf install ./taraengine_<version>_linux_amd64.rpm
```

A package installs the binary as `/usr/bin/taraengine`, LICENSE and NOTICE under `/usr/share/doc/taraengine/`, and the
system service. Removing the package removes the service; a Debian purge also removes `/etc/taraengine` and
`/var/lib/taraengine`.

**By hand, verified:** download the binary for your machine from the
[releases page](https://github.com/tara-vision/taraengine/releases): `taraengine-darwin-arm64` for Apple Silicon,
`taraengine-linux-amd64` or `taraengine-linux-arm64` for Linux. Every release also carries `checksums.txt`, a keyless
cosign signature of it as a Sigstore bundle (`checksums.txt.sigstore.json`) and SLSA provenance.

```sh
BASE=https://github.com/tara-vision/taraengine/releases/latest/download
BIN=taraengine-$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
curl -fsSL --fail-early -O $BASE/$BIN -O $BASE/checksums.txt -O $BASE/checksums.txt.sigstore.json && \
  cosign verify-blob --bundle checksums.txt.sigstore.json \
    --certificate-identity-regexp 'https://github.com/tara-vision/taraengine/.github/workflows/release.yml@refs/tags/v.*' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt && \
  grep " $BIN\$" checksums.txt > $BIN.sha256 && sha256sum -c $BIN.sha256 && \
  sudo mkdir -p /usr/local/bin && sudo install -m 0755 $BIN /usr/local/bin/taraengine
```

The steps form one chain, so a download, a signature or a checksum that fails stops it before anything is installed.
Then install the service: `taraengine service install` on macOS, `sudo /usr/local/bin/taraengine service install` on
Linux. `taraengine version` prints the version you installed. The signature covers `checksums.txt`, and the checksums
cover every binary and package of the release, so verifying the one verifies the rest. The provenance can be checked
with [slsa-verifier](https://github.com/slsa-framework/slsa-verifier). The macOS binary is signed and notarized, so
macOS runs it also when a browser downloaded it.

**Updates:** `taraengine update` brings taraengine to the latest release the way it came: through Homebrew for the
cask, the new package for a `.deb` or an `.rpm` (run it with sudo), or the binary in place (with sudo when its
directory needs it), and then it restarts the service. `taraengine update --check` only says whether a newer release
is out. It looks the release up only when you run it.

To build from source, clone the repository and run `make build`; [Development](#development) lists the other
targets.

## Service

`taraengine service install` installs the service for the binary that runs it and starts it; the cask, the installer
script and the packages run it for you. The service runs `taraengine serve --service`, which fetches the pinned
runtime when it is missing and keeps its stderr to warnings.

|               | macOS                                                 | Linux                                                                    |
|---------------|-------------------------------------------------------|--------------------------------------------------------------------------|
| What          | a LaunchAgent of your user, `vision.tara.taraengine`  | a systemd system service, `taraengine.service`, run as user `taraengine` |
| Starts        | at login                                              | at boot                                                                  |
| Definition    | `~/Library/LaunchAgents/vision.tara.taraengine.plist` | `/etc/systemd/system/taraengine.service`                                 |
| Configuration | `~/.config/taraengine/config.yaml`                    | `/etc/taraengine/config.yaml`, written once and then left alone          |
| Data          | `~/Library/Application Support/taraengine`            | `/var/lib/taraengine`                                                    |
| Logs          | `~/Library/Logs/taraengine/service.log`, the data log | `journalctl -u taraengine`, `/var/lib/taraengine/logs/taraengine.log`    |
| Changed       | as your user                                          | with sudo                                                                |

`service stop` stops the service until `service start`, across logins and reboots, and an install or an upgrade keeps a
stopped service stopped. `service restart` restarts it, or starts a stopped one, which then runs at login or boot again;
`service status` shows it and the engine it runs, and ends with 3 when it does not run; `service uninstall` stops it and
removes the definition, while the models and the configuration stay. On Linux, a normal user's commands reach the system
service at the default address; when the configuration sets an `api_key`, set `TARAENGINE_API_KEY`. The unit makes
`/usr`, `/boot`, `/efi` and `/etc` read-only for the engine and hides `/home`, `/root` and `/run/user`, so the binary
and `data_dir` belong outside them; an admin who needs `/home` lifts `ProtectHome` with a drop-in
(`sudo systemctl edit taraengine`). The service imports a local GGUF file as the user `taraengine`, so the file has to
be outside `/home`, such as under `/srv/models`, and readable by that user. Without systemd, as in a container, run
`taraengine serve` under the supervisor you have.

## Runtime

taraengine does not carry llama.cpp inside its binary. The service fetches it from the llama.cpp releases on GitHub
on its own, `taraengine runtime install` fetches it for a daemon you run by hand, and each taraengine release pins
exactly one upstream build.

- **Pinned.** The binary embeds `internal/runtime/runtime.lock.yaml`: the build number and the sha256 of every
  release archive it may install, per platform.
- **Verified.** The download is hashed and compared with the lock before anything is unpacked. The unpacking refuses
  any entry that would land outside the target directory, and any link that does not point at a sibling file. The
  sha256 of the installed `llama-server` goes into a `manifest.json` next to it, and `taraengine serve` checks the
  binary against that file when it starts. It refuses to start when the runtime is missing or does not match. Run by
  hand, `serve` never downloads a runtime itself; run by the service, as `serve --service`, it installs the pinned
  build first, trying again after 10, 30 and 60 seconds and then every five minutes, and removes the builds it no
  longer pins once the new one verifies. A configured `runtime_dir` is left alone: nothing is fetched or removed for it.
- **Kept whole.** The directory holds `llama-server`, its libraries and upstream's `LICENSE`. A failed install leaves
  the previous one as it was.
- **Moved on request.** `taraengine runtime update` moves to the build that the running taraengine pins, never to
  another one, and says `already at <build>` when there is nothing to do. `taraengine runtime status` says whether
  the runtime that the daemon would run on is verified, which is the `runtime_dir` of the configuration when it is set
  and the pinned build under the data directory otherwise, and `--dir-only` prints just that directory.

The build is chosen for the machine:

| Machine                                           | Build     |
|---------------------------------------------------|-----------|
| Apple Silicon                                     | Metal     |
| Linux with an NVIDIA driver of 580 or newer       | CUDA 13.4 |
| Linux amd64 with an NVIDIA driver of 570 or newer | CUDA 12.8 |
| Any other Linux machine                           | CPU       |

The driver version comes from `nvidia-smi`. The Linux arm64 builds are pinned too, but have not been tested on a
machine. Other systems, such as Intel Macs, are not supported in this release. A CUDA build needs the CUDA runtime
libraries of its major version. When `ldconfig` does not list them, `runtime install` also fetches upstream's CUDA
runtime bundle, a few hundred megabytes; `--cudart` fetches it always and `--no-cudart` never. `taraengine doctor`
shows the GPU, the RAM tier and the runtime it found, and names a model that needs a newer runtime than the pinned one.

The runtime lives in `<data dir>/runtime/<build>/`. The data directory is `~/Library/Application Support/taraengine`
on macOS, and `$XDG_DATA_HOME/taraengine` (default `~/.local/share/taraengine`) on Linux; the Linux service uses
`/var/lib/taraengine`.

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

| Name          | Tier            | Size    | Capabilities            | KV cache |
|---------------|-----------------|---------|-------------------------|----------|
| gemma4:e4b    | small (default) | 4.6 GB  | tools, thinking, vision | q8_0     |
| gemma4:12b    | 16 (default)    | 7.2 GB  | tools, thinking, vision | f16      |
| gemma4:26b    | 32              | 14.6 GB | tools, thinking, vision | f16      |
| gemma4:31b    | 48              | 18.0 GB | tools, thinking, vision | f16      |
| glm-4.7-flash | 32 (default)    | 18.2 GB | tools, thinking         | q8_0     |
| qwen3.6:27b   | 32              | 19.1 GB | tools, thinking, vision | q8_0     |
| qwen3.8:27b   | 32              | 19.0 GB | tools, thinking, vision | q8_0     |
| qwen3.6:35b   | 48 (default)    | 20.4 GB | tools, thinking, vision | q8_0     |

Size is the main file, as `taraengine list` shows it; the projector and the draft file come on top. KV cache is the
type of the model's attention cache: q8_0 holds less memory than f16 at the same window, and an entry keeps f16 where
q8_0 cost it more than a tenth of its speed on the RTX 5090 board. A `cache-type-k` or `cache-type-v` in
`preset_global` sets another for every model, these included. llama.cpp quantizes the V cache only with flash
attention, so with `flash-attn: "off"` in `preset_global` these models keep an f16 V cache. A tier is the RAM class a
model is meant for. `small` suits any machine, and a machine with at least 14, 30 or 46 GiB of RAM counts as `16`,
`32` or `48`. `(default)` marks the model that `taraengine doctor` recommends for a tier. Each model keeps the license
of its authors; read it on the model's Hugging Face page before you use it.

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
```

`taraengine rm <model>` deletes a pulled model and its projector and draft file from disk.

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
| `GET /api/health` | `{"status":"ok"}` or `"degraded"`, with the backend's name; while the daemon starts, `"starting"` with a `detail` that names the step. Open without a key; `HEAD` works too. | |
| `GET /api/version` | The taraengine version, the runtime build and the platform. | |
| `GET /api/models` | The catalog and the pulled models: name, status, size, family, parameters, quantization, context, capabilities, tier, default and memory. | 503 |
| `GET /api/models/{name}` | One model in detail: also its native context, a digest, the preset in effect and, after a failure, the end of the server log. | 400, 404, 503 |
| `GET /api/loaded` | The loaded and sleeping models, with their status (`loaded` or `sleeping`), context window and memory (`bytes` and `source`, `measured` or `estimated`). | 503 |
| `POST /api/load` | `{"model":"...","context":N,"force":false}`, the last two optional. `context` is from 512 to 4194304 tokens; 0 or no value means the model's usual window. Loads the model and answers with it once it is up; a model that sleeps at that window is answered at once and wakes on its next request, and a model that is still loading is waited for. | 400, 404, 409 faulty, 502 load failed, 503, 504 over `load_timeout` |
| `POST /api/unload` | `{"model":"..."}`. Answers 204. | 400, 404, 503 |
| `POST /api/pull` | `{"model":"..."}` with a catalog name, `hf://owner/repo/file.gguf` or an absolute path. Streams the progress as server-sent events (`resolving`, `downloading`, `verifying`, `done`, `error`); with `?wait=1` it answers once, with `{"status":"done"}`. | 400 bad reference, 404 unknown name, 409 that name is busy or taken by another model or file, 502 |
| `DELETE /api/models/{name}` | Removes a pulled model and its sibling files. A model imported from a path or `hf://` whose file is already gone is dropped from the catalog all the same. Answers 204. | 400, 404, 409, 503 |
| `GET /api/doctor` | The machine (GPU, memory, tier, driver), the runtime status and the recommended model per tier. | |
| `/v1/*` | llama-server's OpenAI and Anthropic API, proxied. | 400, 401, 403, 404, 405, 409, 503 |

While the daemon starts, `GET` and `HEAD /api/health` and `GET /api/version` answer, and every other request that passes
the host, origin and key checks answers 503 with `Retry-After: 5` and the error `taraengine is starting: <detail>`.

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
names another, which then has to exist. The Linux system service reads `/etc/taraengine/config.yaml`.

A key that this version does not know is an error that names the key and its
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
| `preset_global` | none | Extra llama-server options for every model, as preset keys (the long option name without the dashes), laid over the engine's own: `jinja: "true"`, `flash-attn: auto`, `n-gpu-layers: "999"`, `reasoning-format: deepseek` and `load-on-startup: "false"`. A key set here also wins over the catalog's own preset for that key, as the KV cache of some models. Quote numbers. |

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

**No telemetry.** taraengine sends no usage data and files no crash reports, and it looks for a new release only when
you run `taraengine update`. It connects only when something needs it: to `huggingface.co` on `taraengine pull`; to
`github.com` on `taraengine runtime install`, `runtime update` and `taraengine update`, and when the service starts
without a verified copy of the build it pins, as on the first start after an install or after an upgrade that pins a new
build; and to `engine.tara.vision` when `taraengine update` cannot get the latest version from GitHub. Hugging Face and
GitHub redirect downloads to their own content delivery hosts (under `hf.co` and `githubusercontent.com`). Nothing else
leaves the machine.

A pull is carried out by the daemon, so it is the `serve` process that connects to Hugging Face, during the pull and for
a short while after it. Under the service, `serve --service` also connects to GitHub while it fetches its runtime. At
any other time neither `serve` nor the `llama-server` it supervises holds a connection beyond the loopback. A Hugging
Face token is sent to Hugging Face only, and it is never logged.

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

To report a vulnerability, see [SECURITY.md](SECURITY.md).

## Development

```sh
make build            # Build the binary
make test             # Run the tests with the race detector
make coverage-gate    # Check the per-package coverage floors
make lint             # Run golangci-lint
make hygiene          # Check dashes, quotes, lab names and Go file sizes
make integration      # Run the real pinned runtime with a one-megabyte model; needs the network
make vuln             # Run govulncheck
```

[CONTRIBUTING.md](CONTRIBUTING.md) lists the checks that every change has to pass.

## Contributing

Contributions are welcome! Please read our [Contributing Guide](CONTRIBUTING.md)
and [Code of Conduct](CODE_OF_CONDUCT.md).

## Security

For security issues, please see our [Security Policy](SECURITY.md).

## Sponsoring

taraengine is free, MIT-licensed and built without telemetry or a cloud service. If it saves you time, you can
[sponsor Tara Vision on GitHub](https://github.com/sponsors/tara-vision) to keep the releases, the catalog and the
runtime pins coming.

## Credits

Built on [llama.cpp](https://github.com/ggml-org/llama.cpp) by ggml-org (MIT). `taraengine runtime install` downloads an
unmodified upstream build, and upstream's LICENSE stays next to it in the data directory. See [NOTICE](NOTICE).

## License

MIT License - see [LICENSE](LICENSE) for details.

---

<p align="center">
  Built with ❤️ by <a href="https://tara.vision">Tara Vision</a> · Created by <a href="https://github.com/dayanstef">Dejan Stefanoski</a>
</p>
