// Prepares the docs of the newest stable release for the site to build
// beside main's (#404). The site's default version is what the newest
// stable release ships: the docs/ folder of the newest docs/vX.Y branch,
// which scripts/tag-release.sh moves to each stable tag and which takes
// cherry-picked fixes between releases. main's docs/ is built as "Next".
//
// The branch's docs are written into versioned_docs/ for Docusaurus at
// build time and never committed, so docs/ on main stays the only copy in
// git. With no docs/v* branch (a fork, or a clone without its branches),
// nothing is written and the site builds main's docs alone, as before.
//
// Run by `npm run build` and `npm start` before Docusaurus.
const { execFileSync } = require('child_process');
const fs = require('fs');
const os = require('os');
const path = require('path');

const site = path.resolve(__dirname, '..');

function git(args, options = {}) {
  return execFileSync('git', args, { cwd: site, maxBuffer: 1 << 28, ...options });
}

// newestBranch returns the ref of the newest docs/vX.Y branch, and its
// version. The branch on origin is preferred, being what is published; a
// local one is used only where origin has none, as in a clone that made it
// by hand to try a fix.
function newestBranch() {
  let refs;
  try {
    refs = git(['for-each-ref', '--format=%(refname)', 'refs/remotes/origin/docs/', 'refs/heads/docs/'])
      .toString()
      .split('\n')
      .filter(Boolean);
  } catch {
    return null; // not a git checkout
  }
  let best = null;
  for (const ref of refs) {
    const m = /^refs\/(remotes\/origin|heads)\/docs\/v(\d+)\.(\d+)$/.exec(ref);
    if (!m) {
      continue;
    }
    const candidate = { ref, version: `${m[2]}.${m[3]}`, major: +m[2], minor: +m[3], remote: m[1] !== 'heads' };
    if (
      !best ||
      candidate.major > best.major ||
      (candidate.major === best.major && candidate.minor > best.minor) ||
      (candidate.version === best.version && candidate.remote && !best.remote)
    ) {
      best = candidate;
    }
  }
  return best;
}

// extract writes the files under docs/ at ref into dir.
function extract(ref, dir) {
  const files = git(['ls-tree', '-r', '-z', '--name-only', '--full-tree', ref, '--', 'docs/'])
    .toString()
    .split('\0')
    .filter(Boolean);
  for (const file of files) {
    const out = path.join(dir, path.relative('docs', file));
    fs.mkdirSync(path.dirname(out), { recursive: true });
    fs.writeFileSync(out, git(['show', `${ref}:${file}`]));
  }
  return files.length;
}

// sidebar reads docs-site/sidebars.js as it is at ref, so the stable docs
// are listed as that release listed them, never naming a page it lacks.
function sidebar(ref) {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'janus-sidebars-'));
  try {
    const file = path.join(tmp, 'sidebars.js');
    fs.writeFileSync(file, git(['show', `${ref}:docs-site/sidebars.js`]));
    return require(file);
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
}

function main() {
  for (const generated of ['versioned_docs', 'versioned_sidebars', 'versions.json']) {
    fs.rmSync(path.join(site, generated), { recursive: true, force: true });
  }
  const branch = newestBranch();
  if (!branch) {
    console.log('versions: no docs/v* branch; building main\'s docs alone');
    return;
  }
  const dir = path.join(site, 'versioned_docs', `version-${branch.version}`);
  const count = extract(branch.ref, dir);
  if (count === 0) {
    throw new Error(`versions: ${branch.ref} has no docs/ folder`);
  }
  fs.mkdirSync(path.join(site, 'versioned_sidebars'), { recursive: true });
  fs.writeFileSync(
    path.join(site, 'versioned_sidebars', `version-${branch.version}-sidebars.json`),
    JSON.stringify(sidebar(branch.ref), null, 2),
  );
  fs.writeFileSync(path.join(site, 'versions.json'), JSON.stringify([branch.version]));
  console.log(`versions: ${count} files from ${branch.ref} as v${branch.version}; main as Next`);
}

main();
