// A remark plugin for the docs site. The pages in docs/ are written to be
// read on GitHub, and some of their links leave docs/ (the README, SECURITY,
// source files) or point at a folder or a file that is not a page (an HTML
// diagram source). None of those exist on the site, so this rewrites them
// to the same file on GitHub. Links between pages, and images, are left for
// Docusaurus to resolve.
//
// A page of the stable docs (#404) is read from
// docs-site/versioned_docs/version-X.Y/, a copy of docs/ on the docs/vX.Y
// branch, so its links are resolved as if it were in docs/ and point at
// that branch rather than main.
const path = require('path');

const repo = 'https://github.com/risadams/Janus';
const root = path.resolve(__dirname, '../../..');
const docs = path.join(root, 'docs');
const versioned = path.join(root, 'docs-site', 'versioned_docs');
const page = /\.mdx?$/;
const image = /\.(png|jpe?g|gif|svg|webp)$/i;

// place says which copy of docs/ a page is in: its folder, and the branch
// the files around it are on.
function place(from) {
  const rel = path.relative(versioned, from);
  const m = /^version-(\d+\.\d+)(?:[\\/]|$)/.exec(rel);
  if (rel.startsWith('..') || !m) {
    return { from, branch: 'main' };
  }
  const copy = path.join(versioned, `version-${m[1]}`);
  return { from: path.join(root, 'docs', path.relative(copy, from)), branch: `docs/v${m[1]}` };
}

function rewrite(url, file) {
  const { from, branch } = place(file);
  if (!url || /^[a-z][a-z0-9+.-]*:/i.test(url) || url.startsWith('#') || url.startsWith('/')) {
    return null;
  }
  const [target, fragment] = url.split('#');
  if (!target) {
    return null;
  }
  const resolved = path.resolve(path.dirname(from), decodeURIComponent(target));
  const inDocs = resolved === docs || resolved.startsWith(docs + path.sep);
  const isFolder = url.endsWith('/') || !path.extname(resolved);
  if (inDocs && !isFolder && (page.test(resolved) || image.test(resolved))) {
    return null;
  }
  const relative = path.relative(root, resolved).split(path.sep).join('/');
  const kind = isFolder ? 'tree' : 'blob';
  return `${repo}/${kind}/${branch}/${relative}${fragment ? `#${fragment}` : ''}`;
}

function walk(node, from) {
  if ((node.type === 'link' || node.type === 'definition') && typeof node.url === 'string') {
    const url = rewrite(node.url, from);
    if (url) {
      node.url = url;
    }
  }
  for (const child of node.children || []) {
    walk(child, from);
  }
}

module.exports = function repoLinks() {
  return (tree, file) => {
    if (file.path) {
      walk(tree, file.path);
    }
  };
};
