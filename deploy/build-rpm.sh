#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 || ! $1 =~ ^[0-9]+(\.[0-9]+)*$ || ! ${2-1} =~ ^[0-9]+$ ]]; then
    echo "Usage: $0 VERSION [RELEASE] (numeric components only)" >&2
    exit 1
fi
version=$1
release=${2-1}
[[ $(uname -s) == Linux ]] || { echo 'A supported Linux build host is required.' >&2; exit 1; }
for tool in go rpmbuild; do
    command -v "$tool" >/dev/null || { echo "Required tool: $tool" >&2; exit 1; }
done
go_version=$(go env GOVERSION)
if [[ ! $go_version =~ ^go([0-9]+)\.([0-9]+) ]] || (( BASH_REMATCH[1] < 1 || (BASH_REMATCH[1] == 1 && BASH_REMATCH[2] < 23) )); then
    echo 'Go 1.23 or newer is required.' >&2
    exit 1
fi
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work"/{BUILD,BUILDROOT,RPMS,SOURCES,SPECS,SRPMS}
cd "$root"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 GOFLAGS='' go build -trimpath -buildvcs=false -o "$work/SOURCES/typemail-milter" ./cmd/typemail-milter
cp deploy/typemail-milter.service typemail.example.toml "$work/SOURCES/"
rpmbuild -bb --target x86_64 --define "_topdir $work" \
    --define "package_version $version" --define "package_release $release" \
    deploy/typemail-milter.spec
mkdir -p dist
package="typemail-milter-$version-$release.el7.x86_64.rpm"
cp "$work/RPMS/x86_64/$package" "dist/$package"
echo "Built $root/dist/$package"
