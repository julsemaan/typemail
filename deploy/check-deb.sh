#!/usr/bin/env bash
set -euo pipefail

if [[ $# != 3 || ! $2 =~ ^[0-9]+(\.[0-9]+)*$ || ! $3 =~ ^[0-9]+$ ]]; then
    echo "Usage: $0 PACKAGE VERSION RELEASE" >&2
    exit 1
fi
for tool in dpkg-deb tar readelf go; do
    command -v "$tool" >/dev/null || { echo "Required tool: $tool" >&2; exit 1; }
done
root=$(cd "$(dirname "$0")/.." && pwd)
package=$(realpath "$1")
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export LC_ALL=C

dpkg-deb --field "$package" > "$work/control"
cat > "$work/expected" <<EOF
Package: typemail-milter
Version: $2-$3
Section: mail
Priority: optional
Architecture: amd64
Maintainer: Typemail <typemail@localhost>
Depends: postfix, systemd, ca-certificates, passwd, libc-bin
Description: Fail-open incoming Postfix subject tagging Milter
 Tags incoming email subjects using OpenRouter. Runs as an unprivileged
 local Milter without changing Postfix configuration.
EOF
diff -u "$work/expected" "$work/control"

dpkg-deb --fsys-tarfile "$package" | tar --numeric-owner -tvf - | awk '{print $1, $2, $6}' | sort > "$work/files"
cat > "$work/expected" <<'EOF'
-rw------- 0/0 ./etc/typemail/milter.env
-rw-r----- 0/0 ./etc/typemail/typemail.toml
-rw-r--r-- 0/0 ./usr/lib/systemd/system/typemail-milter.service
-rwxr-xr-x 0/0 ./usr/bin/typemail-milter
drwxr-x--- 0/0 ./etc/typemail/
drwxr-xr-x 0/0 ./
drwxr-xr-x 0/0 ./etc/
drwxr-xr-x 0/0 ./usr/
drwxr-xr-x 0/0 ./usr/bin/
drwxr-xr-x 0/0 ./usr/lib/
drwxr-xr-x 0/0 ./usr/lib/systemd/
drwxr-xr-x 0/0 ./usr/lib/systemd/system/
EOF
diff -u "$work/expected" "$work/files"

dpkg-deb --ctrl-tarfile "$package" | tar --numeric-owner -tvf - | awk '{print $1, $2, $6}' | sort > "$work/files"
cat > "$work/expected" <<'EOF'
-rw-r--r-- 0/0 ./conffiles
-rw-r--r-- 0/0 ./control
-rwxr-xr-x 0/0 ./postinst
-rwxr-xr-x 0/0 ./postrm
-rwxr-xr-x 0/0 ./prerm
drwxr-xr-x 0/0 ./
EOF
diff -u "$work/expected" "$work/files"

dpkg-deb --control "$package" "$work/control-files"
printf '/etc/typemail/typemail.toml\n/etc/typemail/milter.env\n' > "$work/expected"
diff -u "$work/expected" "$work/control-files/conffiles"
for script in postinst prerm postrm; do
    [[ $(head -n 1 "$work/control-files/$script") == '#!/bin/sh' ]]
    sh -n "$work/control-files/$script"
done

dpkg-deb --extract "$package" "$work/payload"
cmp "$root/typemail.example.toml" "$work/payload/etc/typemail/typemail.toml"
cmp "$root/deploy/typemail-milter.service" "$work/payload/usr/lib/systemd/system/typemail-milter.service"
[[ ! -s $work/payload/etc/typemail/milter.env ]]
binary="$work/payload/usr/bin/typemail-milter"
readelf -h "$binary" | grep -q 'Machine:.*Advanced Micro Devices X86-64'
readelf -l "$binary" > "$work/program-headers"
readelf -d "$binary" > "$work/dynamic"
if grep -q INTERP "$work/program-headers" || grep -q NEEDED "$work/dynamic"; then
    echo 'Packaged binary is not static.' >&2
    exit 1
fi
go version -m "$binary" > "$work/build-settings"
for setting in CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1; do
    grep -Eq "build[[:space:]]+$setting$" "$work/build-settings"
done
echo "Debian inspection passed: $package"
