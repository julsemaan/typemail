#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 || ! $1 =~ ^[0-9]+(\.[0-9]+)*$ || ! ${2-1} =~ ^[0-9]+$ ]]; then
    echo "Usage: $0 VERSION [RELEASE] (numeric components only)" >&2
    exit 1
fi
version=$1
release=${2-1}
[[ $(uname -s) == Linux ]] || { echo 'A supported Linux build host is required.' >&2; exit 1; }
for tool in go dpkg-deb; do
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
umask 022
mkdir -p "$work/package/DEBIAN" "$work/package/usr/bin"
cd "$root"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 GOFLAGS='' go build -trimpath -buildvcs=false -o "$work/package/usr/bin/typemail-milter" ./cmd/typemail-milter
chmod 0755 "$work/package/usr/bin/typemail-milter"
install -D -m 0644 deploy/typemail-milter.service "$work/package/usr/lib/systemd/system/typemail-milter.service"
install -d -m 0750 "$work/package/etc/typemail"
install -m 0640 typemail.example.toml "$work/package/etc/typemail/typemail.toml"
install -m 0600 /dev/null "$work/package/etc/typemail/milter.env"
cat > "$work/package/DEBIAN/control" <<EOF
Package: typemail-milter
Version: $version-$release
Section: mail
Priority: optional
Architecture: amd64
Maintainer: Typemail <typemail@localhost>
Depends: postfix, systemd, ca-certificates, passwd, libc-bin
Description: Fail-open incoming Postfix subject tagging Milter
 Tags incoming email subjects using OpenRouter. Runs as an unprivileged
 local Milter without changing Postfix configuration.
EOF
printf '/etc/typemail/typemail.toml\n/etc/typemail/milter.env\n' > "$work/package/DEBIAN/conffiles"
cat > "$work/package/DEBIAN/postinst" <<'EOF'
#!/bin/sh
set -e
if [ "$1" = configure ]; then
    getent group typemail >/dev/null || groupadd --system typemail
    getent passwd typemail >/dev/null || useradd --system --gid typemail --no-create-home --home-dir / --shell /usr/sbin/nologin typemail
    install -d -o root -g typemail -m 0750 /etc/typemail
    # Respect conffiles deliberately removed by the administrator.
    if [ -f /etc/typemail/typemail.toml ]; then
        chown root:typemail /etc/typemail/typemail.toml
        chmod 0640 /etc/typemail/typemail.toml
    fi
    if [ -f /etc/typemail/milter.env ]; then
        chown root:root /etc/typemail/milter.env
        chmod 0600 /etc/typemail/milter.env
    fi
    systemctl daemon-reload >/dev/null 2>&1 || :
fi
EOF
cat > "$work/package/DEBIAN/prerm" <<'EOF'
#!/bin/sh
set -e
if [ "$1" = remove ]; then
    systemctl stop typemail-milter.service >/dev/null 2>&1 || :
    systemctl disable typemail-milter.service >/dev/null 2>&1 || :
fi
EOF
cat > "$work/package/DEBIAN/postrm" <<'EOF'
#!/bin/sh
set -e
systemctl daemon-reload >/dev/null 2>&1 || :
if [ "$1" = purge ]; then
    for file in /etc/typemail/typemail.toml /etc/typemail/milter.env; do
        rm -f "$file" "$file.dpkg-old" "$file.dpkg-new" "$file.dpkg-dist"
    done
    rmdir /etc/typemail 2>/dev/null || :
fi
EOF
chmod 0755 "$work/package/DEBIAN/"{postinst,prerm,postrm}
mkdir -p dist
package="typemail-milter_${version}-${release}_amd64.deb"
dpkg-deb --root-owner-group --build "$work/package" "$work/$package"
cp "$work/$package" "dist/$package"
echo "Built $root/dist/$package"
