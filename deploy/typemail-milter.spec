Name:           typemail-milter
Version:        %{package_version}
Release:        %{package_release}
Summary:        Fail-open incoming Postfix subject tagging Milter
License:        Proprietary
BuildArch:      x86_64
Source0:        typemail-milter
Source1:        typemail-milter.service
Source2:        typemail.example.toml
AutoReqProv:    no
Requires:       postfix, systemd, shadow-utils, ca-certificates
Requires(pre):  shadow-utils
Requires(post): systemd
Requires(preun): systemd
Requires(postun): systemd

# Keep host-specific stripping, build IDs, and dependency generators out.
%global __os_install_post %{nil}
%global _build_id_links none
%global _binary_payload w9.gzdio
%global _rpmformat 4

%description
Tags incoming email subjects using OpenRouter. Runs as an unprivileged
local Milter without changing Postfix configuration.

%prep

%build

%install
install -D -m 0755 %{SOURCE0} %{buildroot}/usr/bin/typemail-milter
install -D -m 0644 %{SOURCE1} %{buildroot}/usr/lib/systemd/system/typemail-milter.service
install -d -m 0750 %{buildroot}/etc/typemail
install -m 0640 %{SOURCE2} %{buildroot}/etc/typemail/typemail.toml
install -m 0600 /dev/null %{buildroot}/etc/typemail/milter.env

%pre
set -e
getent group typemail >/dev/null || /usr/sbin/groupadd --system typemail
getent passwd typemail >/dev/null || /usr/sbin/useradd --system --gid typemail --no-create-home --home-dir / --shell /sbin/nologin typemail

%post
/bin/systemctl daemon-reload >/dev/null 2>&1 || :
/bin/systemctl enable typemail-milter.service >/dev/null 2>&1 || :
/bin/systemctl restart typemail-milter.service >/dev/null 2>&1 || :

%preun
if [ "$1" -eq 0 ]; then
    /bin/systemctl stop typemail-milter.service >/dev/null 2>&1 || :
    /bin/systemctl disable typemail-milter.service >/dev/null 2>&1 || :
fi

%postun
/bin/systemctl daemon-reload >/dev/null 2>&1 || :

%files
%attr(0755,root,root) /usr/bin/typemail-milter
%attr(0644,root,root) /usr/lib/systemd/system/typemail-milter.service
%dir %attr(0750,root,typemail) /etc/typemail
%config(noreplace) %attr(0640,root,typemail) /etc/typemail/typemail.toml
%config(noreplace) %attr(0600,root,root) /etc/typemail/milter.env
