# HologramAgent RPM Spec
# Build: rpmbuild -ba hologramagent.spec
#
# Define variant at build time:
#   rpmbuild -ba --define "variant full" hologramagent.spec
#   rpmbuild -ba --define "variant server" hologramagent.spec
#   rpmbuild -ba --define "variant client" hologramagent.spec

%define _prefix /usr
%define _bindir %{_prefix}/bin
%define _unitdir %{_prefix}/lib/systemd/system
%define _datadir %{_prefix}/share/hologramagent
%define _varlibdir /var/lib/hologram

%if %{undefined variant}
%define variant full
%endif

Name: hologramagent-%{variant}
Version: %{_version}
Release: 1%{?dist}
Summary: HologramAgent - Personal AI Home Assistant
License: Proprietary
URL: https://github.com/trueagent/HologramAgent
Group: System Environment/Daemons
BuildArch: %{_arch}

%if "%{variant}" == "full" || "%{variant}" == "server"
Requires: systemd
%endif
%if "%{variant}" == "full"
Requires: libX11, libxcb, libdrm, mesa-libGL, nss, nspr, atk, at-spi2-atk, cairo, cups-libs, pango, gtk3
%endif
%if "%{variant}" == "client"
Requires: libX11, libxcb, libdrm, mesa-libGL, nss, nspr, atk, at-spi2-atk, cairo, cups-libs, pango, gtk3
%endif

%description
HologramAgent is a personal AI home assistant that integrates large language
models with system automation.

%if "%{variant}" == "full"
This package includes the full suite: homed (daemon), waiter (CLI),
and hologram-gui (desktop GUI).
%else
%if "%{variant}" == "server"
This package includes the server components: homed (daemon) and waiter (CLI).
%else
%if "%{variant}" == "client"
This package includes the client components: waiter (CLI) and
hologram-gui (desktop GUI). Connects to a remote HologramAgent server.
%endif
%endif
%endif

%install
mkdir -p %{buildroot}%{_bindir}
mkdir -p %{buildroot}%{_unitdir}
mkdir -p %{buildroot}%{_varlibdir}

%if "%{variant}" == "full" || "%{variant}" == "server"
install -m 755 %{_sourcedir}/homed %{buildroot}%{_bindir}/homed
install -m 644 %{_sourcedir}/hologramagent.service %{buildroot}%{_unitdir}/hologramagent.service
%endif

# waiter 三个变体都要：server 也含 CLI（对齐 deb 的 stage_variant
# 与 control-server 的 "waiter: command-line client" 描述）。
install -m 755 %{_sourcedir}/waiter %{buildroot}%{_bindir}/waiter

%if "%{variant}" == "full" || "%{variant}" == "client"
mkdir -p %{buildroot}%{_datadir}/hologram-gui
cp -r %{_sourcedir}/hologram-gui-linux-*/* %{buildroot}%{_datadir}/hologram-gui/
%endif

%files
%defattr(-,root,root,-)

%if "%{variant}" == "full" || "%{variant}" == "server"
%{_bindir}/homed
%{_unitdir}/hologramagent.service
%dir %{_varlibdir}
%endif

%{_bindir}/waiter

%if "%{variant}" == "full" || "%{variant}" == "client"
%dir %{_datadir}/hologram-gui
%{_datadir}/hologram-gui/*
%endif

%post
%if "%{variant}" == "full" || "%{variant}" == "server"
mkdir -p %{_varlibdir}
%systemd_post hologramagent.service
%endif

%preun
%if "%{variant}" == "full" || "%{variant}" == "server"
%systemd_preun hologramagent.service
%endif

%postun
%if "%{variant}" == "full" || "%{variant}" == "server"
%systemd_postun_with_restart hologramagent.service
%endif

%changelog
* Mon Jul 20 2026 HologramAgent Team <team@hologramagent.ai> - %{version}-1
- Initial Linux packaging
