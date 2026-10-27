# Packaging and publishing

How to get a `cli-to-api` release onto the two extra install channels:

* **Homebrew** — `brew tap baydisng13/cli-to-api https://github.com/baydisng13/cli-to-api` then `brew install cli-to-api`
* **npm** — `npx cliapi`, `npm i -g cliapi`

This is a runbook for a human doing it for the first time. Everything here
happens **after** a release exists; nothing in this document builds binaries.

---

## 0. The thing to get right: ordering

Both channels **download the archives that `.github/workflows/release.yml`
published**. Neither of them builds anything. So:

```
git tag v0.2.0 && git push origin v0.2.0
        │
        ▼
  release.yml runs  ──►  GitHub Release v0.2.0 exists, with
                         6 archives + checksums.txt
        │
        ├──────────────►  Homebrew: regenerate + push the formula
        └──────────────►  npm: bump version to 0.2.0, npm publish
```

If you publish to npm before the release exists, every `npm install` of that
version fails in `postinstall` with a 404 — and you cannot unpublish-and-retry
freely, npm only lets you republish a given version number once it has been
removed, within 72 hours. **Wait for the green release.**

Check before you start:

```sh
gh release view v0.2.0 --repo baydisng13/cli-to-api
# you want 7 assets: 4 tar.gz, 2 zip, checksums.txt
```

### Version strings

Three spellings of the same version, and they must agree:

| Where | Spelling | Example |
| --- | --- | --- |
| Git tag / release / asset names | with `v` | `v0.2.0` |
| Homebrew `version` stanza | bare | `0.2.0` |
| npm `package.json` `version` | bare | `0.2.0` |

The npm installer and the formula both re-add the `v` when building URLs.

### Before the first publish of either channel

The repository has **no `LICENSE` file**. Add one:

* `brew audit --strict` wants a `license` stanza in the formula, and it must
  match the actual licence. The formula currently carries a comment instead of a
  guess — fill it in once `LICENSE` exists.
* `npm publish` warns about a missing licence and the package page shows
  "No license". Add a matching `"license"` field to `packaging/npm/package.json`.

Do not guess an SPDX identifier on the owner's behalf.

---

## 1. Homebrew tap

### What is in the repo

| File | What it is |
| --- | --- |
| `Formula/cli-to-api.rb` | The formula **template** — the source of truth, edited here. |
| `packaging/homebrew/update-formula.sh` | Points the template at a published release (version + 4 sha256s). |
| `.github/workflows/tap-publish.yml` | Optional, manual-only CI version of the same thing. |

The formula is **bottle-less**: it does not compile anything and there are no
Homebrew bottles. Each platform downloads the release archive for its own
os/arch and installs the one binary inside. That is why it has four
`url` + `sha256` pairs inside `on_macos`/`on_linux` × `on_arm`/`on_intel`
blocks rather than a single source tarball.

### 1a. One-time: create the tap repository

This repository **is its own tap**. A Homebrew tap is any git repo with a
top-level `Formula/` directory, and `brew tap <name> <url>` accepts an
arbitrary URL, so no separate `homebrew-tap` repository is needed and the
formula stays in the monorepo next to the code it installs.

```bash
brew tap baydisng13/cli-to-api https://github.com/baydisng13/cli-to-api
brew trust baydisng13/cli-to-api
brew install cli-to-api
```

Homebrew 7 refuses to load a formula from an untrusted third-party tap:

```
Error: Refusing to load formula ... from untrusted tap baydisng13/cli-to-api.
Run `brew trust ...` to trust it.
```

That gate applies to every third-party tap, not just this arrangement, so
it is not a cost of keeping the formula in the monorepo. Mention it
wherever the tap is advertised, or users hit it on their first install.

The cost is one extra word for the user compared with the conventional
`brew install cli-to-api`, which would require a second repo
named exactly `homebrew-tap`. If you later decide that shorter line is worth
a second repo, copy `Formula/cli-to-api.rb` into it unchanged — the formula
itself does not care which repo it is served from.


### 1b. Generate the formula for a release

From a clone of **this** repo:

```sh
packaging/homebrew/update-formula.sh v0.2.0
```

That script:

1. downloads `checksums.txt` from the `v0.2.0` release,
2. pulls out the sha256 for each of the four non-Windows archives
   (`darwin_arm64`, `darwin_amd64`, `linux_arm64`, `linux_amd64`),
3. refuses to continue unless each one is exactly 64 lowercase hex characters,
4. rewrites the `version` and the four `sha256` stanzas in
   `Formula/cli-to-api.rb` in place,
5. re-checks the result with `ruby -c` before overwriting the original.

It is idempotent — each `sha256` line is matched to the archive named on the
`url` line above it, so you can run it repeatedly, and re-run it for a newer
tag over an already-updated formula.

It pushes nothing. Review the diff:

```sh
git diff Formula/cli-to-api.rb
```

There is no Windows entry: Homebrew does not support Windows. Windows users get
`install.ps1`, npm, or a manual download.

#### Getting the sha256 values by hand

If you would rather not run the script, or want to check it:

```sh
curl -fsSL https://github.com/baydisng13/cli-to-api/releases/download/v0.2.0/checksums.txt
```

```
3bf104d8…  cli-to-api_v0.2.0_darwin_arm64.tar.gz
a1b2c3d4…  cli-to-api_v0.2.0_darwin_amd64.tar.gz
…
```

The first column is the sha256; paste it into the `sha256` line that follows
the matching `url` line in the formula. The formula's four URLs end in
`_darwin_arm64.tar.gz`, `_darwin_amd64.tar.gz`, `_linux_arm64.tar.gz`,
`_linux_amd64.tar.gz` — line them up by that suffix, not by position.

### 1c. Land it in the tap

```sh
git add Formula/cli-to-api.rb
git add Formula/cli-to-api.rb
git commit -m "cli-to-api v0.2.0"
git push
```

### 1d. Verify it actually installs

```sh
brew untap baydisng13/cli-to-api 2>/dev/null || true   # start clean
brew tap baydisng13/cli-to-api https://github.com/baydisng13/cli-to-api
brew install cli-to-api
cli-to-api version        # must print v0.2.0

# Run the formula's own test block
brew test cli-to-api

# Lint it (style + the sha256/licence checks)
brew audit --strict --online baydisng13/cli-to-api/cli-to-api
```

`brew audit` is only available once the tap is installed — it cannot be run
against the template in this repo.

If `brew install` reports a checksum mismatch, the formula's sha256 does not
match the asset: re-run `update-formula.sh` against the right tag.

### 1e. Updating for a later version

```sh
packaging/homebrew/update-formula.sh v0.3.0
git add Formula/cli-to-api.rb && git commit -am "cli-to-api v0.3.0" && git push
```

Users then get it with `brew update && brew upgrade cli-to-api`.

The formula carries a `livecheck` block (`strategy :github_latest`), so
`brew livecheck cli-to-api` tells you when the formula has fallen behind the
newest GitHub release.

### 1f. Updating the formula on a new release

The formula lives in this repo, so updating it is an ordinary commit — no
cross-repository write, no Personal Access Token, no second workflow:

```bash
packaging/homebrew/update-formula.sh v0.2.0   # rewrites version + sha256s
git add Formula/cli-to-api.rb
git commit -m "brew: cli-to-api v0.2.0"
git push
```

Run it only after the release workflow is green — the script reads the real
`checksums.txt` from the published release, so the release must exist first.

This is the whole reason the formula sits at `Formula/` in the monorepo
rather than in a separate `homebrew-tap` repo: a cross-repo push would have
needed a PAT in secrets, and a workflow that assumes a secret which may not
exist fails on every release with an opaque auth error.

## 2. npm wrapper

### What is in the repo

```
packaging/npm/
├── package.json          name, version, bin, files allowlist, os/cpu, engines
├── bin/cli-to-api.js     the shim npm links as `cli-to-api`
├── scripts/install.js    postinstall: download, verify sha256, extract
├── .npmignore            belt-and-braces: never publish a downloaded binary
└── README.md             the npmjs.com package page
```

The package **contains no binaries**. On install, `scripts/install.js`:

1. maps `process.platform`/`process.arch` → the release's os/arch strings
   (`win32`→`windows`, `x64`→`amd64`), failing clearly on anything else,
2. downloads `cli-to-api_v<version>_<os>_<arch>.{tar.gz,zip}` **and**
   `checksums.txt` from the release matching `package.json`'s version,
3. verifies the archive's sha256 against `checksums.txt` and **refuses to
   install on a mismatch** — the same bar as `install.sh`,
4. extracts the single binary **in-process** with `node:zlib` (a small tar
   reader for `.tar.gz`, a small zip reader for `.zip`) — it does **not** shell
   out to `tar` or `unzip`, so it does not care what the machine has installed,
5. writes to a unique temp file and `rename()`s it into place, `chmod 0755` on
   unix, so a failure never leaves a half-written binary where the shim can
   exec it.

Zero npm dependencies. It uses the global `fetch`, hence `engines.node >= 18`.

### 2a. One-time: npm account and name

```sh
npm whoami          # are you logged in?
npm login           # if not (opens a browser; 2FA if enabled)

# Is the name free / already yours?
npm view cli-to-api
# "npm error 404 'cli-to-api' is not in this registry" => free
```

If the name is taken, publish under a scope instead — change `name` in
`packaging/npm/package.json` to `cliapi` and update
`packaging/npm/README.md` and `docs/INSTALL.md` to match. `--access public` is
required for a scoped package (it is harmless for an unscoped one).

### 2b. Publish a version

```sh
cd packaging/npm

# 1. The version MUST equal the git tag without its leading "v".
#    --no-git-tag-version: do not let npm create its own tag, the Go release
#    tag is the one that matters.
npm version 0.2.0 --no-git-tag-version

# 2. Confirm the release it will download from already exists.
gh release view v0.2.0 --repo baydisng13/cli-to-api >/dev/null && echo "release OK"

# 3. See exactly what would be published. Expect 4 files and NO binary.
npm pack --dry-run

# 4. Publish.
npm publish --access public
```

Then commit the version bump:

```sh
cd ../..
git add packaging/npm/package.json
git commit -m "npm: v0.2.0"
git push
```

### 2c. Verify it actually installs

In a scratch directory, on a real machine:

```sh
cd "$(mktemp -d)"
npx --yes cli-to-api@0.2.0 version     # must print v0.2.0

# and the global install
npm i -g cliapi@0.2.0
which cli-to-api && cli-to-api version
```

Check the failure mode too — it must be a sentence, not a stack trace:

```sh
npm i -g cliapi@0.2.0 --ignore-scripts
cli-to-api version      # prints how to fix it, exits 1
```

### 2d. If a publish goes wrong

```sh
# Within 72 hours of publishing, and only if nothing depends on it:
npm unpublish cli-to-api@0.2.0

# Otherwise: deprecate and publish a fixed patch version.
npm deprecate cli-to-api@0.2.0 "broken install; use 0.2.1"
```

Remember the npm version has to match a *real* release tag, so a fix generally
means tagging `v0.2.1` first, letting `release.yml` finish, and only then
publishing npm `0.2.1`.

### 2e. Things deliberately not done

* **No proxy support.** The installer uses the global `fetch`, which ignores
  `HTTP_PROXY`/`HTTPS_PROXY`, and honouring them properly needs a dependency
  (`undici`'s `ProxyAgent`) that this package will not take. It **detects** the
  variables and warns that the download will go direct, pointing at
  `install.sh`/`install.ps1`, which do honour them. It does not pretend.
* **No optionalDependencies-per-platform split.** The usual
  `@pkg/linux-x64`-style layout means publishing six extra packages per release
  and keeping them in lockstep; a verified download from the release we already
  publish is less machinery for the same result. The trade-off is that installs
  need network access to `github.com` and `--ignore-scripts` leaves a
  non-working CLI (which the shim explains).
* **No npm publish from CI.** It needs an `NPM_TOKEN` secret, which does not
  exist, and the same reasoning as the tap applies.

---

## 3. After both are published

Add the new routes to `docs/INSTALL.md` and `README.md`:

```sh
brew install cli-to-api
npx cliapi --help
npm i -g cliapi
```

(That edit is outside the scope of the packaging files themselves — it is a
docs change for whoever lands this.)

## 4. Release checklist

```
[ ] git tag vX.Y.Z && git push origin vX.Y.Z
[ ] release.yml green; `gh release view vX.Y.Z` lists 7 assets
[ ] packaging/homebrew/update-formula.sh vX.Y.Z
[ ] run update-formula.sh, commit Formula/cli-to-api.rb, push
[ ] brew update && brew upgrade cli-to-api; brew test cli-to-api
[ ] cd packaging/npm && npm version X.Y.Z --no-git-tag-version
[ ] npm pack --dry-run  (4 files, no binary)
[ ] npm publish --access public
[ ] npx --yes cli-to-api@X.Y.Z version
[ ] commit the package.json bump
```
