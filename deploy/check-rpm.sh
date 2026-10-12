#!/usr/bin/env bash
set -euo pipefail

if [[ $# != 3 || ! $2 =~ ^[0-9]+(\.[0-9]+)*$ || ! $3 =~ ^[0-9]+$ ]]; then
    echo "Usage: $0 PACKAGE VERSION RELEASE" >&2
    exit 1
fi
for tool in rpm rpm2cpio cpio readelf; do
    command -v "$tool" >/dev/null || { echo "Required tool: $tool" >&2; exit 1; }
done
root=$(cd "$(dirname "$0")/.." && pwd)
package=$(realpath "$1")
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

[[ $(rpm -qp --qf '%{NAME} %{VERSION} %{RELEASE} %{ARCH} %{PAYLOADCOMPRESSOR}' "$package") == "typemail-milter $2 $3 x86_64 gzip" ]]
# RPM v4 packages have the classic lead, not the RPM v6 format.
[[ $(od -An -tx1 -N8 "$package" | xargs) == 'ed ab ee db 03 00 00 00' ]]
rpm -qp --qf '[%{FILENAMES} %{FILEMODES:perms} %{FILEUSERNAME} %{FILEGROUPNAME} %{FILEFLAGS:fflags}\n]' "$package" | sort > "$work/files"
cat > "$work/expected" <<'EOF'
/etc/typemail drwxr-x--- root typemail 
/etc/typemail/milter.env -rw------- root root cn
/etc/typemail/typemail.toml -rw-r----- root typemail cn
/usr/bin/typemail-milter -rwxr-xr-x root root 
/usr/lib/systemd/system/typemail-milter.service -rw-r--r-- root root 
EOF
diff -u "$work/expected" "$work/files"

rpm -qp --requires "$package" > "$work/requires"
for dependency in postfix systemd shadow-utils ca-certificates /bin/sh; do
    grep -Fx "$dependency" "$work/requires" >/dev/null
done
# Only features understood by CentOS 7's RPM 4.11 are allowed.
grep -Ev '^(/bin/sh|postfix|systemd|shadow-utils|ca-certificates|rpmlib\(CompressedFileNames\) <= 3\.0\.4-1|rpmlib\(PayloadFilesHavePrefix\) <= 4\.0-1|rpmlib\(FileDigests\) <= 4\.6\.0-1)$' "$work/requires" > "$work/unexpected" || true
[[ ! -s $work/unexpected ]] || { cat "$work/unexpected" >&2; exit 1; }

mkdir "$work/payload"
(cd "$work/payload" && rpm2cpio "$package" | cpio -idm --quiet --no-absolute-filenames)
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
echo "RPM inspection passed: $package"
