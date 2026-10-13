#!/usr/bin/env bash
# Container package checks only. Real systemd/SMTP/SELinux acceptance is separate.
set -euo pipefail
if [[ $# != 2 ]]; then
    echo "Usage: $0 PACKAGE OLDER_PACKAGE (run as root in a disposable container)" >&2
    exit 1
fi
package=$(realpath "$1")
older=$(realpath "$2")
export DEBIAN_FRONTEND=noninteractive
if [[ $package == *.deb ]]; then
    apt-get update
    apt-get install -y postfix systemd ca-certificates passwd libc-bin
    install_package() {
        : > /tmp/systemctl-calls
        apt-get install -y --allow-downgrades -o Dpkg::Options::=--force-confold "$1"
        check_activation_calls
    }
    remove_package() {
        : > /tmp/systemctl-calls
        apt-get remove -y typemail-milter
        check_removal_calls
    }
else
    manager=dnf
    command -v dnf >/dev/null || manager=yum
    "$manager" install -y postfix systemd shadow-utils ca-certificates
    install_package() {
        : > /tmp/systemctl-calls
        "$manager" install -y "$1"
        check_activation_calls
    }
    remove_package() {
        : > /tmp/systemctl-calls
        "$manager" remove -y typemail-milter
        check_removal_calls
    }
fi
# Record lifecycle calls even without PID 1 systemd. Keep real offline enable/disable.
systemctl_path=$(command -v systemctl)
mv "$systemctl_path" "$systemctl_path.real"
cat > "$systemctl_path" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >> /tmp/systemctl-calls
exec "$systemctl_path.real" "\$@"
EOF
chmod 0755 "$systemctl_path"
check_activation_calls() {
    for call in daemon-reload 'enable typemail-milter.service' 'restart typemail-milter.service'; do
        grep -Fxq "$call" /tmp/systemctl-calls
    done
    if grep -Ev '^(daemon-reload|enable typemail-milter\.service|restart typemail-milter\.service)$' /tmp/systemctl-calls; then
        echo 'Unexpected systemctl call during installation.' >&2
        exit 1
    fi
}
check_removal_calls() {
    for call in 'stop typemail-milter.service' 'disable typemail-milter.service' daemon-reload; do
        grep -Fxq "$call" /tmp/systemctl-calls
    done
    if grep -Ev '^(stop typemail-milter\.service|disable typemail-milter\.service|daemon-reload)$' /tmp/systemctl-calls; then
        echo 'Unexpected systemctl call during removal.' >&2
        exit 1
    fi
}
postfix_hash() {
    find /etc/postfix -type f -exec sha256sum {} + | sort
}
postfix_hash > /tmp/postfix-before
check_install() {
    getent passwd typemail
    getent group typemail
    [[ $(stat -c '%U:%G %a' /etc/typemail) == 'root:typemail 750' ]]
    [[ $(stat -c '%U:%G %a' /etc/typemail/typemail.toml) == 'root:typemail 640' ]]
    [[ $(stat -c '%U:%G %a' /etc/typemail/milter.env) == 'root:root 600' ]]
    [[ -x /usr/bin/typemail-milter ]]
    postfix_hash > /tmp/postfix-after
    diff -u /tmp/postfix-before /tmp/postfix-after
}
# Exercise the legacy .el7 -> neutral RPM path before reinstalling the release.
install_package "$older"
check_install
[[ -L /etc/systemd/system/multi-user.target.wants/typemail-milter.service ]]
printf '\n# local configuration\n' >> /etc/typemail/typemail.toml
printf 'OPENROUTER_API_KEY=smoke-test-not-a-secret\n' > /etc/typemail/milter.env
sha256sum /etc/typemail/{typemail.toml,milter.env} > /tmp/config-before
systemctl disable typemail-milter.service
install_package "$package"
check_install
sha256sum -c /tmp/config-before
[[ -L /etc/systemd/system/multi-user.target.wants/typemail-milter.service ]]
if [[ $package == *.deb ]]; then
    install_package "$older"
else
    : > /tmp/systemctl-calls
    "$manager" downgrade -y "$older"
    check_activation_calls
fi
check_install
sha256sum -c /tmp/config-before
[[ -L /etc/systemd/system/multi-user.target.wants/typemail-milter.service ]]
install_package "$package"
remove_package
[[ ! -e /usr/bin/typemail-milter ]]
[[ ! -e /etc/systemd/system/multi-user.target.wants/typemail-milter.service ]]
getent passwd typemail
if [[ $package == *.deb ]]; then
    sha256sum -c /tmp/config-before
    dpkg --purge typemail-milter
    [[ ! -e /etc/typemail/typemail.toml && ! -e /etc/typemail/milter.env ]]
else
    [[ $(stat -c '%U:%G %a' /etc/typemail/milter.env.rpmsave) == 'root:root 600' ]]
    mv /etc/typemail/typemail.toml.rpmsave /etc/typemail/typemail.toml
    mv /etc/typemail/milter.env.rpmsave /etc/typemail/milter.env
    sha256sum -c /tmp/config-before
    rm /etc/typemail/{typemail.toml,milter.env}
fi
# Fresh installation of the exact release artifact, not just the fixture.
install_package "$package"
check_install
[[ ! -s /etc/typemail/milter.env ]]
[[ -L /etc/systemd/system/multi-user.target.wants/typemail-milter.service ]]
remove_package
if [[ $package == *.deb ]]; then dpkg --purge typemail-milter; fi
getent passwd typemail
postfix_hash > /tmp/postfix-after
diff -u /tmp/postfix-before /tmp/postfix-after
echo 'Container installation checks passed; running-state and SMTP checks remain pending.'
