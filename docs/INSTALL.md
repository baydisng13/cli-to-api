# Installing cli-to-api

`cli-to-api` is a single static binary with no runtime dependencies. Pick whichever
route suits you — the one-line installers are the fastest, `go install` is the
best option if you already have a Go toolchain.

---

## macOS / Linux (one line)

```sh
curl -fsSL https://raw.githubusercontent.com/baydisng13/cli-to-api/main/install.sh | sh
```

The script detects your OS and CPU, downloads the matching release archive **and
`checksums.txt`, verifies the SHA-256**, and installs the binary.

Options (set them before the pipe):

```sh
# Install a specific release instead of the latest
curl -fsSL https://raw.githubusercontent.com/baydisng13/cli-to-api/main/install.sh \
  | CLI_TO_API_VERSION=v0.1.0 sh

# Install somewhere specific
curl -fsSL https://raw.githubusercontent.com/baydisng13/cli-to-api/main/install.sh \
  | CLI_TO_API_INSTALL_DIR="$HOME/bin" sh
```

| Variable | Default |
| --- | --- |
| `CLI_TO_API_VERSION` | latest GitHub release |
| `CLI_TO_API_INSTALL_DIR` | `/usr/local/bin` if writable, else `$HOME/.local/bin` |

The installer **never runs `sudo`**. If `/usr/local/bin` would need root, it says
so and falls back to `$HOME/.local/bin`. If the directory it used is not on your
`PATH`, it prints the exact `export PATH=...` line to add to your shell rc file.

Supported platforms: `darwin/amd64`, `darwin/arm64`, `linux/amd64`, `linux/arm64`.
Anything else exits with a clear message pointing at `go install`.

---

## Windows (one line)

Windows PowerShell 5.1 or PowerShell 7+:

```powershell
irm https://raw.githubusercontent.com/baydisng13/cli-to-api/main/install.ps1 | iex
```

To pin a version, set the variable first:

```powershell
$env:CLI_TO_API_VERSION = 'v0.1.0'
irm https://raw.githubusercontent.com/baydisng13/cli-to-api/main/install.ps1 | iex
```

It installs to `%LOCALAPPDATA%\Programs\cli-to-api` and adds that directory to
your **user** PATH (no administrator rights needed, machine PATH untouched).
Open a new terminal afterwards so the PATH change is picked up.

Supported: `windows/amd64`, `windows/arm64`. 32-bit x86 is not built.

---

## With a Go toolchain

```sh
go install github.com/baydisng13/cli-to-api/cmd/cli-to-api@latest
```

The binary lands in `$(go env GOPATH)/bin` (usually `~/go/bin`) — make sure that
is on your `PATH`.

Note: `go install` builds from source, so the version string is derived by the Go
toolchain from the module rather than injected by the release pipeline. Released
binaries report the exact release tag.

---

## Manual download

Grab an archive from <https://github.com/baydisng13/cli-to-api/releases>.

### Archive naming convention

```
cli-to-api_<version>_<os>_<arch>.tar.gz     # macOS and Linux
cli-to-api_<version>_<os>_<arch>.zip        # Windows
```

* `<version>` — the release tag **including the leading `v`**, e.g. `v0.1.0`
* `<os>` — `darwin`, `linux`, or `windows`
* `<arch>` — `amd64` or `arm64`

Examples:

```
cli-to-api_v0.1.0_darwin_arm64.tar.gz     # Apple Silicon Mac
cli-to-api_v0.1.0_darwin_amd64.tar.gz     # Intel Mac
cli-to-api_v0.1.0_linux_amd64.tar.gz
cli-to-api_v0.1.0_linux_arm64.tar.gz
cli-to-api_v0.1.0_windows_amd64.zip
cli-to-api_v0.1.0_windows_arm64.zip
checksums.txt                             # sha256 of every archive above
```

Each archive contains exactly one file at its root: `cli-to-api`
(or `cli-to-api.exe` on Windows).

Not sure which arch you are on? `uname -m` → `x86_64`/`amd64` means `amd64`,
`aarch64`/`arm64` means `arm64`.

### Install it by hand (macOS / Linux)

```sh
tar -xzf cli-to-api_v0.1.0_linux_amd64.tar.gz
chmod +x cli-to-api
sudo mv cli-to-api /usr/local/bin/     # or: mv cli-to-api ~/.local/bin/
cli-to-api version
```

---

## Verifying checksums by hand

Download the archive **and** `checksums.txt` from the same release, put them in
one directory, then:

```sh
# Linux (GNU coreutils) — verifies just your file and ignores the rest
sha256sum --ignore-missing -c checksums.txt

# macOS / BSD — compare the two lines yourself
shasum -a 256 cli-to-api_v0.1.0_darwin_arm64.tar.gz
grep darwin_arm64 checksums.txt
```

```powershell
# Windows PowerShell
Get-FileHash .\cli-to-api_v0.1.0_windows_amd64.zip -Algorithm SHA256
Select-String windows_amd64 .\checksums.txt
```

The hashes must match exactly (case-insensitively). If they do not, **do not run
the binary** — delete it and open an issue.

---

## macOS: Gatekeeper and the unsigned binary

Release binaries are cross-compiled on Linux and are **not code-signed or
notarized**. This matters only if macOS has flagged the file as quarantined,
which happens when you download it with a **web browser**:

> "cli-to-api" cannot be opened because the developer cannot be verified.

Two ways out:

```sh
# 1. Strip the quarantine attribute (adjust the path to where you installed it)
xattr -d com.apple.quarantine /usr/local/bin/cli-to-api
```

2. Or: in Finder, right-click (Control-click) the binary → **Open** → **Open**
   in the dialog. macOS then remembers your approval for that file.

Downloads made by `curl` — which is what `install.sh` uses — are **not**
quarantined, so the one-line installer does not hit this at all.

---

## Upgrading

Re-run the same one-line installer. It overwrites the existing binary in place.

---

## Uninstalling

**macOS / Linux**

```sh
rm -f "$(command -v cli-to-api)"
# or, if you know where it went:
rm -f /usr/local/bin/cli-to-api
rm -f "$HOME/.local/bin/cli-to-api"
```

If you added a `PATH` line to your shell rc file for `~/.local/bin` and no longer
want it, remove that line too.

**Windows (PowerShell)**

```powershell
Remove-Item -Recurse -Force "$env:LOCALAPPDATA\Programs\cli-to-api"

# Remove it from the user PATH as well
$dir = "$env:LOCALAPPDATA\Programs\cli-to-api"
$p   = [Environment]::GetEnvironmentVariable('Path','User')
$new = ($p -split ';' | Where-Object { $_ -and $_.TrimEnd('\') -ine $dir.TrimEnd('\') }) -join ';'
[Environment]::SetEnvironmentVariable('Path', $new, 'User')
```

**Installed with `go install`**

```sh
rm -f "$(go env GOPATH)/bin/cli-to-api"
```

---

## Building from source

```sh
git clone https://github.com/baydisng13/cli-to-api
cd cli-to-api
go build -o cli-to-api ./cmd/cli-to-api
./cli-to-api version
```

To reproduce a release build locally (`make build` does this for you):

```sh
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=v0.1.0" \
  -o cli-to-api ./cmd/cli-to-api
```
