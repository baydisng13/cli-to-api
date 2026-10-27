# cli-to-api

Serve local agentic CLIs behind an OpenAI-compatible API. Single Go binary, no
dependencies, no database.

Point any OpenAI client at `http://localhost:8080/v1` and the text comes from
whatever CLI you have installed — Antigravity (`agy`), any shell command, or an
upstream API. Each API key is scoped to **one backend and an explicit list of
models**.

## Install

```sh
npx cliapi --help      # one-off, nothing installed globally
npm i -g cliapi        # or install it for good
```

## Use

```sh
cli-to-api key create      # wizard: pick a CLI, pick models, get a key
cli-to-api start           # serve on :8080
cli-to-api doctor          # which backends are installed and answering
```

Config lives in `$CLI_TO_API_HOME`, else `$XDG_CONFIG_HOME/cli-to-api`, else
`~/.config/cli-to-api`.

Full documentation: <https://github.com/baydisng13/cli-to-api>

## What this package actually does

It ships **no binaries**. On install, a `postinstall` script downloads the
GitHub Release archive matching this package's version for your platform,
**verifies its SHA-256 against the release's `checksums.txt`**, and extracts the
single `cli-to-api` binary next to the `bin` shim. Extraction happens in-process
(`node:zlib`) — no `tar` or `unzip` needed, on any OS.

If the checksum does not match, nothing is installed.

### Requirements

* Node.js **18 or newer** (the installer uses the global `fetch`).
* Network access to `github.com` at install time.

### Supported platforms

| OS | Architectures |
| --- | --- |
| macOS | `arm64` (Apple Silicon), `x64` (Intel) |
| Linux | `x64`, `arm64` |
| Windows | `x64`, `arm64` |

Anything else fails with a message pointing at
`go install github.com/baydisng13/cli-to-api/cmd/cli-to-api@latest`.

### Proxies

The installer uses Node's global `fetch`, which does **not** honour
`HTTP_PROXY` / `HTTPS_PROXY`. If you are behind a proxy it will warn you and the
download will go direct (and probably fail). Use one of the other installers
instead — both honour proxy environment variables:

```sh
curl -fsSL https://raw.githubusercontent.com/baydisng13/cli-to-api/main/install.sh | sh
```

```powershell
irm https://raw.githubusercontent.com/baydisng13/cli-to-api/main/install.ps1 | iex
```

### `--ignore-scripts` / air-gapped installs

Set `CLI_TO_API_SKIP_DOWNLOAD=1` to skip the download deliberately. Either way,
running `cli-to-api` without the binary prints exactly how to fix it rather than
a stack trace — including the one-liner to re-run the download inside an
existing install.

## Other install routes

See <https://github.com/baydisng13/cli-to-api/blob/main/docs/INSTALL.md> —
shell one-liners, Homebrew (`brew tap baydisng13/cli-to-api https://github.com/baydisng13/cli-to-api`),
`go install`, and manual download with checksum verification.
