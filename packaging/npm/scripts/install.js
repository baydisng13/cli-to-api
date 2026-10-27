#!/usr/bin/env node
//
// postinstall — download the cli-to-api release binary that matches this
// package's version and this machine's platform.
//
// The npm package ships NO binaries. On install it fetches
//
//   https://github.com/baydisng13/cli-to-api/releases/download/v<version>/
//     cli-to-api_v<version>_<os>_<arch>.tar.gz   (darwin, linux)
//     cli-to-api_v<version>_<os>_<arch>.zip      (windows)
//     checksums.txt
//
// verifies the archive's sha256 against checksums.txt, extracts the single
// binary it contains, and places it at bin/cli-to-api(.exe).
//
// Zero npm dependencies: node:fs, node:path, node:crypto, node:zlib and the
// global fetch (hence engines.node >= 18). Extraction is done in-process —
// nothing is shelled out to, so this does not depend on `tar` or `unzip`
// existing on the machine (notably on Windows).
//
// Escape hatch: set CLI_TO_API_SKIP_DOWNLOAD=1 to skip this entirely (air-gapped
// builds, Docker layers that vendor the binary another way). The bin shim then
// prints an actionable error instead of a stack trace.

'use strict';

const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const zlib = require('node:zlib');

const REPO = 'baydisng13/cli-to-api';
const BINARY = 'cli-to-api';

const pkg = require('../package.json');

// ------------------------------------------------------------------ util ---

function fail(message, hint) {
  console.error(`\ncli-to-api: install failed: ${message}`);
  if (hint) console.error(`\n${hint}`);
  console.error(
    `\nYou can install cli-to-api another way instead:\n` +
      `  macOS/Linux : curl -fsSL https://raw.githubusercontent.com/${REPO}/main/install.sh | sh\n` +
      `  Windows     : irm https://raw.githubusercontent.com/${REPO}/main/install.ps1 | iex\n` +
      `  Go toolchain: go install github.com/${REPO}/cmd/${BINARY}@latest\n` +
      `  By hand     : https://github.com/${REPO}/releases\n`,
  );
  process.exit(1);
}

// -------------------------------------------------------------- platform ---

// Maps a Node platform/arch pair onto the GOOS/GOARCH strings the release
// workflow uses in its asset names. Exported for testing.
function targetFor(platform, arch) {
  const osMap = {
    darwin: 'darwin',
    linux: 'linux',
    win32: 'windows',
  };
  const archMap = {
    x64: 'amd64',
    arm64: 'arm64',
  };

  const goos = osMap[platform];
  const goarch = archMap[arch];

  if (!goos || !goarch) {
    return null;
  }
  return { goos, goarch, ext: goos === 'windows' ? '.zip' : '.tar.gz' };
}

// Builds the exact release asset name. Mirrors release.yml:
//   base="${BINARY}_${TAG}_${GOOS}_${GOARCH}"
// where TAG keeps its leading "v".
function assetNameFor(version, goos, goarch, ext) {
  return `${BINARY}_v${version}_${goos}_${goarch}${ext}`;
}

// ---------------------------------------------------------------- verify ---

function sha256(buf) {
  return crypto.createHash('sha256').update(buf).digest('hex');
}

// Parses `sha256sum` output: "<64 hex>  <name>" (the name may carry a leading
// "*" for binary mode). Returns the expected hash for `name`, or null.
function expectedSumFor(checksumsText, name) {
  for (const line of checksumsText.split('\n')) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    const parts = trimmed.split(/\s+/);
    if (parts.length < 2) continue;
    const hash = parts[0];
    let file = parts[parts.length - 1].replace(/^\*/, '');
    file = file.replace(/^.*\//, ''); // defensive: strip any directory part
    if (file === name) {
      return /^[0-9a-f]{64}$/.test(hash) ? hash : null;
    }
  }
  return null;
}

// --------------------------------------------------------------- extract ---

// Minimal reader for the single-file ustar/GNU tarball the release pipeline
// produces. Returns the contents of `wanted`, or null.
function extractFromTarGz(gzBuf, wanted) {
  const tar = zlib.gunzipSync(gzBuf);
  const BLOCK = 512;

  for (let off = 0; off + BLOCK <= tar.length; ) {
    const header = tar.subarray(off, off + BLOCK);

    // Two consecutive zero blocks end the archive; one is enough to stop.
    if (header.every((b) => b === 0)) break;

    const rawName = header.subarray(0, 100).toString('utf8').replace(/\0.*$/, '');
    const prefix = header.subarray(345, 500).toString('utf8').replace(/\0.*$/, '');
    const name = prefix ? `${prefix}/${rawName}` : rawName;

    const sizeField = header.subarray(124, 136).toString('utf8').replace(/\0.*$/, '').trim();
    const size = parseInt(sizeField, 8);
    if (!Number.isFinite(size) || size < 0) {
      throw new Error(`corrupt tar header (bad size field ${JSON.stringify(sizeField)})`);
    }

    const typeFlag = String.fromCharCode(header[156]);
    const dataStart = off + BLOCK;
    const dataEnd = dataStart + size;
    if (dataEnd > tar.length) {
      throw new Error('corrupt tar (entry runs past end of archive)');
    }

    const isFile = typeFlag === '0' || typeFlag === '\0';
    if (isFile && name === wanted) {
      return tar.subarray(dataStart, dataEnd);
    }

    off = dataStart + Math.ceil(size / BLOCK) * BLOCK;
  }

  return null;
}

// Minimal zip reader: locate the end-of-central-directory record, walk the
// central directory, then read the one entry we want out of its local header.
// Handles stored (0) and deflated (8) entries, which is all `zip -X` emits.
function extractFromZip(buf, wanted) {
  const EOCD_SIG = 0x06054b50;
  const CEN_SIG = 0x02014b50;
  const LOC_SIG = 0x04034b50;

  // EOCD is at most 22 + 65535 bytes from the end.
  let eocd = -1;
  const scanStart = Math.max(0, buf.length - (22 + 0xffff));
  for (let i = buf.length - 22; i >= scanStart; i--) {
    if (buf.readUInt32LE(i) === EOCD_SIG) {
      eocd = i;
      break;
    }
  }
  if (eocd < 0) throw new Error('not a zip file (no end-of-central-directory record)');

  const entryCount = buf.readUInt16LE(eocd + 10);
  let p = buf.readUInt32LE(eocd + 16);

  for (let i = 0; i < entryCount; i++) {
    if (p + 46 > buf.length || buf.readUInt32LE(p) !== CEN_SIG) {
      throw new Error('corrupt zip central directory');
    }
    const method = buf.readUInt16LE(p + 10);
    const compSize = buf.readUInt32LE(p + 20);
    const uncompSize = buf.readUInt32LE(p + 24);
    const nameLen = buf.readUInt16LE(p + 28);
    const extraLen = buf.readUInt16LE(p + 30);
    const commentLen = buf.readUInt16LE(p + 32);
    const localOff = buf.readUInt32LE(p + 42);
    const name = buf.subarray(p + 46, p + 46 + nameLen).toString('utf8');

    if (name === wanted) {
      if (localOff + 30 > buf.length || buf.readUInt32LE(localOff) !== LOC_SIG) {
        throw new Error('corrupt zip (bad local file header)');
      }
      const lNameLen = buf.readUInt16LE(localOff + 26);
      const lExtraLen = buf.readUInt16LE(localOff + 28);
      const dataStart = localOff + 30 + lNameLen + lExtraLen;
      const dataEnd = dataStart + compSize;
      if (dataEnd > buf.length) throw new Error('corrupt zip (entry runs past end of file)');

      const raw = buf.subarray(dataStart, dataEnd);
      const out = method === 0 ? raw : method === 8 ? zlib.inflateRawSync(raw) : null;
      if (out === null) throw new Error(`unsupported zip compression method ${method}`);
      if (uncompSize !== 0 && out.length !== uncompSize) {
        throw new Error('corrupt zip (decompressed size mismatch)');
      }
      return out;
    }

    p += 46 + nameLen + extraLen + commentLen;
  }

  return null;
}

// -------------------------------------------------------------- download ---

async function download(url) {
  const res = await fetch(url, {
    redirect: 'follow',
    headers: { 'user-agent': `cli-to-api-npm/${pkg.version}` },
  });
  if (!res.ok) {
    const err = new Error(`HTTP ${res.status} ${res.statusText} for ${url}`);
    err.status = res.status;
    throw err;
  }
  return Buffer.from(await res.arrayBuffer());
}

// ------------------------------------------------------------------ main ---

async function main() {
  if (process.env.CLI_TO_API_SKIP_DOWNLOAD) {
    console.log(
      'cli-to-api: CLI_TO_API_SKIP_DOWNLOAD is set — skipping the binary download.',
    );
    return;
  }

  const version = pkg.version;
  // The version is interpolated into a URL and a filename; validate before use.
  if (!/^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$/.test(version)) {
    fail(`package version is not a plain semver string: ${JSON.stringify(version)}`);
  }

  const target = targetFor(process.platform, process.arch);
  if (!target) {
    fail(
      `unsupported platform ${process.platform}/${process.arch}.`,
      'cli-to-api publishes prebuilt binaries for:\n' +
        '  darwin/x64, darwin/arm64, linux/x64, linux/arm64, win32/x64, win32/arm64\n' +
        'On anything else, build from source with a Go toolchain:\n' +
        `  go install github.com/${REPO}/cmd/${BINARY}@latest`,
    );
  }

  const { goos, goarch, ext } = target;
  const assetName = assetNameFor(version, goos, goarch, ext);
  const baseUrl = `https://github.com/${REPO}/releases/download/v${version}`;

  for (const proxyVar of ['HTTPS_PROXY', 'https_proxy', 'HTTP_PROXY', 'http_proxy']) {
    if (process.env[proxyVar]) {
      console.warn(
        `cli-to-api: warning: ${proxyVar} is set, but this installer uses Node's\n` +
          `  global fetch, which does NOT honour proxy environment variables.\n` +
          `  The download below goes direct. If your network requires the proxy it\n` +
          `  will fail; install with install.sh / install.ps1 (curl and Invoke-\n` +
          `  WebRequest both honour the proxy) or download the archive by hand.`,
      );
      break;
    }
  }

  const binName = goos === 'windows' ? `${BINARY}.exe` : BINARY;
  const destDir = path.join(__dirname, '..', 'bin');
  const dest = path.join(destDir, binName);
  // Unique temp name so two concurrent installs cannot tread on each other.
  const tmp = path.join(destDir, `.${binName}.${process.pid}.${Date.now()}.tmp`);

  console.log(`cli-to-api: downloading ${assetName}`);

  let archive;
  let checksumsText;
  try {
    archive = await download(`${baseUrl}/${assetName}`);
  } catch (e) {
    if (e.status === 404) {
      fail(
        `no release asset ${assetName} at ${baseUrl}`,
        `That usually means one of:\n` +
          `  * the GitHub release v${version} does not exist yet (the npm package was\n` +
          `    published before the release — see docs/PACKAGING.md), or\n` +
          `  * that release has no ${goos}/${goarch} build.`,
      );
    }
    fail(`could not download ${assetName}: ${e.message}`);
  }

  try {
    checksumsText = (await download(`${baseUrl}/checksums.txt`)).toString('utf8');
  } catch (e) {
    fail(`could not download checksums.txt: ${e.message}`);
  }

  if (!archive.length) fail(`downloaded ${assetName} is empty`);

  const expected = expectedSumFor(checksumsText, assetName);
  if (!expected) {
    fail(
      `no usable sha256 entry for ${assetName} in checksums.txt`,
      'Refusing to install an unverified binary.',
    );
  }

  const actual = sha256(archive);
  if (actual !== expected) {
    fail(
      `CHECKSUM MISMATCH for ${assetName}\n` +
        `  expected: ${expected}\n` +
        `  actual:   ${actual}`,
      'Refusing to install. The download does not match the published checksum.',
    );
  }
  console.log(`cli-to-api: checksum OK (sha256 ${actual})`);

  let payload;
  try {
    payload =
      ext === '.zip'
        ? extractFromZip(archive, binName)
        : extractFromTarGz(archive, binName);
  } catch (e) {
    fail(`could not extract ${assetName}: ${e.message}`);
  }
  if (!payload || !payload.length) {
    fail(`${assetName} did not contain a '${binName}' entry at its root`);
  }

  // Write to a temp file first, then rename into place, so a crash or a full
  // disk can never leave a half-written binary where the shim will exec it.
  try {
    fs.mkdirSync(destDir, { recursive: true });
    fs.writeFileSync(tmp, payload, { mode: 0o755 });
    if (process.platform !== 'win32') fs.chmodSync(tmp, 0o755);
    fs.renameSync(tmp, dest);
  } catch (e) {
    try {
      fs.rmSync(tmp, { force: true });
    } catch {
      /* best effort */
    }
    fail(`could not install the binary to ${dest}: ${e.message}`);
  }

  console.log(`cli-to-api: installed ${dest}`);
}

if (require.main === module) {
  main().catch((e) => {
    fail(e && e.stack ? e.stack : String(e));
  });
}

module.exports = { targetFor, assetNameFor, expectedSumFor };
