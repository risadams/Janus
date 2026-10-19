#!/usr/bin/env bash
# Writes the three winget manifests for one release into a folder (#339):
#
#   scripts/winget-manifests.sh v1.2.3 dist/checksums.txt out
#   scripts/winget-manifests.sh v1.2.3 dist/checksums.txt out http://127.0.0.1:8902
#
# out/ gets RisAdams.Janus.yaml (the version), .installer.yaml, and
# .locale.en-US.yaml, in the shape microsoft/winget-pkgs takes. Each
# installer is the release's own zip, with its sum from checksums.txt, and
# janus.exe inside it installed as a portable command. The fourth argument
# replaces the release's download URL, so that a pull request can install
# the manifests from the archives it has just built. The release workflow
# submits them with wingetcreate on each stable tag.
set -euo pipefail

usage="usage: scripts/winget-manifests.sh <version> <checksums.txt> <folder> [base URL]"
version="${1:?$usage}"
sums="${2:?$usage}"
out="${3:?$usage}"
v="${version#v}"
base="${4:-https://github.com/risadams/Janus/releases/download/${version}}"
id=RisAdams.Janus
schema=1.10.0

sum() {
  local s
  s="$(awk -v n="$1" '$2 == n || $2 == "*" n { print $1 }' "$sums")"
  [ -n "$s" ] || { echo "winget-manifests.sh: $1 is not in ${sums}" >&2; exit 1; }
  echo "$s" | tr '[:lower:]' '[:upper:]'
}
installer() {
  local name="janus_${v}_windows_$2"
  cat <<YAML
- Architecture: $1
  InstallerUrl: ${base}/${name}.zip
  InstallerSha256: $(sum "${name}.zip")
  NestedInstallerFiles:
  - RelativeFilePath: ${name}\\janus.exe
    PortableCommandAlias: janus
YAML
}

mkdir -p "$out"
header="# Written by the Janus release workflow (scripts/winget-manifests.sh in risadams/Janus)."

cat > "${out}/${id}.yaml" <<YAML
${header}
# yaml-language-server: \$schema=https://aka.ms/winget-manifest.version.${schema}.schema.json

PackageIdentifier: ${id}
PackageVersion: ${v}
DefaultLocale: en-US
ManifestType: version
ManifestVersion: ${schema}
YAML

cat > "${out}/${id}.installer.yaml" <<YAML
${header}
# yaml-language-server: \$schema=https://aka.ms/winget-manifest.installer.${schema}.schema.json

PackageIdentifier: ${id}
PackageVersion: ${v}
InstallerType: zip
NestedInstallerType: portable
Commands:
- janus
Installers:
$(installer x64 amd64)
$(installer arm64 arm64)
ManifestType: installer
ManifestVersion: ${schema}
YAML

cat > "${out}/${id}.locale.en-US.yaml" <<YAML
${header}
# yaml-language-server: \$schema=https://aka.ms/winget-manifest.defaultLocale.${schema}.schema.json

PackageIdentifier: ${id}
PackageVersion: ${v}
PackageLocale: en-US
Publisher: Ris Adams
PublisherUrl: https://github.com/risadams
PublisherSupportUrl: https://github.com/risadams/Janus/issues
Author: Ris Adams
PackageName: Janus
PackageUrl: https://risadams.github.io/Janus/
License: MIT
LicenseUrl: https://github.com/risadams/Janus/blob/main/LICENSE
ShortDescription: A local proxy for Claude Code that tracks cost and keeps secrets local.
Description: |-
  Janus sits on your machine between Claude Code and the Anthropic API. It keeps the books on every
  request and, when you switch them on, sends chosen work to a local LM Studio model, keeps secrets
  on your machine, and keeps you working when Claude is down.
Moniker: janus
Tags:
- claude
- claude-code
- anthropic
- llm
- proxy
- cli
ReleaseNotesUrl: https://github.com/risadams/Janus/releases/tag/${version}
ManifestType: defaultLocale
ManifestVersion: ${schema}
YAML
