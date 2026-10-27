#!/usr/bin/env node
//
// bin shim — hands straight over to the real cli-to-api binary that the
// postinstall script placed next to this file.
//
// If that binary is missing (most commonly: `npm install --ignore-scripts`, or
// an install whose postinstall failed), print something a human can act on
// rather than an ENOENT stack trace.

'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const REPO = 'baydisng13/cli-to-api';

const binName = process.platform === 'win32' ? 'cli-to-api.exe' : 'cli-to-api';
const binPath = path.join(__dirname, binName);

if (!fs.existsSync(binPath)) {
  let version = 'this version';
  try {
    version = `v${require('../package.json').version}`;
  } catch {
    /* fall back to the generic wording */
  }

  process.stderr.write(
    `\ncli-to-api: the ${binName} binary is not installed.\n\n` +
      `The npm package is only a downloader: it fetches the ${version} release\n` +
      `binary from GitHub in a postinstall script, and that step did not run or\n` +
      `did not finish.\n\n` +
      `Fix it with ONE of:\n\n` +
      `  # re-run the download (works inside an existing install)\n` +
      `  node "${path.join(__dirname, '..', 'scripts', 'install.js')}"\n\n` +
      `  # or reinstall without --ignore-scripts\n` +
      `  npm install -g cli-to-api\n\n` +
      `  # or skip npm entirely\n` +
      `  curl -fsSL https://raw.githubusercontent.com/${REPO}/main/install.sh | sh\n\n` +
      `If the download keeps failing, the error it prints says why; releases are\n` +
      `at https://github.com/${REPO}/releases\n\n`,
  );
  process.exit(1);
}

const result = spawnSync(binPath, process.argv.slice(2), {
  stdio: 'inherit',
  windowsHide: false,
});

if (result.error) {
  const e = result.error;
  if (e.code === 'EACCES') {
    process.stderr.write(
      `\ncli-to-api: ${binPath} is not executable.\n` +
        `Try: chmod +x "${binPath}"\n\n`,
    );
  } else {
    process.stderr.write(`\ncli-to-api: could not run ${binPath}: ${e.message}\n\n`);
  }
  process.exit(1);
}

// Re-raise the child's terminating signal so wrappers see the same outcome;
// if that somehow does not kill us, fall back to the conventional 128+n.
if (result.signal) {
  process.kill(process.pid, result.signal);
  process.exit(1);
}

process.exit(result.status === null ? 1 : result.status);
