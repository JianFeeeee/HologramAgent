# HomeAgent RPM Spec
# Build: rpmbuild -ba homeagent.spec
#
# Define variant at build time:
#   rpmbuild -ba --define "variant full" homeagent.spec
#   rpmbuild -ba --define "variant server" homeagent.spec
#   rpmbuild -ba --define "variant client" homeagent.spec

%define _prefix /usr
%define _bindir %{_prefix}/bin
%define _unitdir %{_prefix}/lib/systemd/system
%define _datadir %{_prefix}/share/homeagent
%define _varlibdir /var/lib/homeagent

%if %{undefined variant}
%define variant full
%endif

Name: homeagent-%{variant}
Version: %{_version}
Release: 1%{?dist}
Summary: HomeAgent - Personal AI Home Assistant
License: Proprietary
URL: https://github.com/trueagent/HomeAgent
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
HomeAgent is a personal AI home assistant that integrates large language
models with system automation.

%if "%{variant}" == "full"
This package includes the full suite: homed (daemon), waiter (CLI),
and homeagent-gui (desktop GUI).
%else
%if "%{variant}" == "server"
This package includes the server components: homed (daemon) and waiter (CLI).
%else
%if "%{variant}" == "client"
This package includes the client components: waiter (CLI) and
homeagent-gui (desktop GUI). Connects to a remote HomeAgent server.
%endif
%endif
%endif

%install
mkdir -p %{buildroot}%{_bindir}
mkdir -p %{buildroot}%{_unitdir}
mkdir -p %{buildroot}%{_varlibdir}

%if "%{variant}" == "full" || "%{variant}" == "server"
install -m 755 %{_sourcedir}/homed %{buildroot}%{_bindir}/homed
install -m 644 %{_sourcedir}/homeagent.service %{buildroot}%{_unitdir}/homeagent.service
%endif

# waiter 三个变体都要：server 也含 CLI（对齐 deb 的 stage_variant
# 与 control-server 的 "waiter: command-line client" 描述）。
install -m 755 %{_sourcedir}/waiter %{buildroot}%{_bindir}/waiter

%if "%{variant}" == "full" || "%{variant}" == "client"
mkdir -p %{buildroot}%{_datadir}/homeagent-gui
cp -r %{_sourcedir}/homeagent-gui-linux-*/* %{buildroot}%{_datadir}/homeagent-gui/
%endif

%files
%defattr(-,root,root,-)

%if "%{variant}" == "full" || "%{variant}" == "server"
%{_bindir}/homed
%{_unitdir}/homeagent.service
%dir %{_varlibdir}
%endif

%{_bindir}/waiter

%if "%{variant}" == "full" || "%{variant}" == "client"
%dir %{_datadir}/homeagent-gui
%{_datadir}/homeagent-gui/*
%endif

%post
%if "%{variant}" == "full" || "%{variant}" == "server"
mkdir -p %{_varlibdir}
%systemd_post homeagent.service
%endif

%preun
%if "%{variant}" == "full" || "%{variant}" == "server"
%systemd_preun homeagent.service
%endif

%postun
%if "%{variant}" == "full" || "%{variant}" == "server"
%systemd_postun_with_restart homeagent.service
%endif

%changelog
* Mon Jul 20 2026 HomeAgent Team <team@homeagent.ai> - %{version}-1
- Initial Linux packaging
