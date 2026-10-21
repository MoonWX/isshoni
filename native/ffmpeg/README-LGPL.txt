FFmpeg in isshoni
=================

The isshoni Windows app uses two libraries of FFmpeg @FFMPEG_VERSION@, libavcodec and libavutil, as the DLLs
@AVCODEC_DLL@ (libavcodec @AVCODEC_VERSION@) and @AVUTIL_DLL@ (libavutil @AVUTIL_VERSION@) in its install folder.
FFmpeg is free software under the GNU Lesser General Public License, version 2.1 or (at your option) any later
version (LGPL-2.1-or-later). The license text and FFmpeg's LICENSE.md are in this folder: COPYING.LGPLv2.1 (named
COPYING.LGPLv2.1.txt in an isshoni installation) and LICENSE.md. isshoni itself is under the Apache License 2.0 and
links these DLLs dynamically. The DLLs are built from unmodified FFmpeg source and are not signed by the isshoni
project.


How they were built
-------------------

Build @TAG@ (FFmpeg @FFMPEG_VERSION@, build revision @REVISION@) with the MSVC toolset @MSVC_TOOLSET@, whose compiler
cl.exe is build @MSVC_COMPILER@ and whose linker link.exe is build @MSVC_LINKER@, and the build script
native/ffmpeg/build.sh. The configuration, as avcodec_configuration() reports it:

  @CONFIGURATION@

It is an LGPL build: no GPL, nonfree or version 3 parts, and no FFmpeg component other than the four hardware H.264
encoders h264_amf, h264_mf, h264_nvenc and h264_qsv. Intel's libvpl dispatcher (MIT) is linked into the DLLs
statically, and the NVIDIA and AMD headers (MIT) are compiled in; their licenses are in the folders libvpl,
nv-codec-headers and amf next to this one.


The source code
---------------

The complete corresponding source code, with the build script, is published with every isshoni release as the
release asset isshoni-desktop_<version>_ffmpeg-source.tar, where <version> is the version of isshoni you installed
(https://github.com/MoonWX/isshoni/releases), and as the asset @NAME@-source.tar of the release @TAG@:

  https://github.com/MoonWX/isshoni/releases/tag/@TAG@

It holds the source tarballs of FFmpeg, nv-codec-headers, the AMF headers and libvpl exactly as their projects
publish them, the build script and the configuration above.


Replacing the DLLs
------------------

You may replace both DLLs with your own build of FFmpeg: an interface-compatible build with the same major versions
(@AVCODEC_DLL@ and @AVUTIL_DLL@, the same file names, in the install folder) and at least the shipped minor versions
(libavcodec @AVCODEC_VERSION@, libavutil @AVUTIL_VERSION@). isshoni checks avcodec_version() and avutil_version()
when it loads the DLLs and does not check their contents. An isshoni update installs the shipped DLLs again.

To rebuild the shipped DLLs from the source archive: install MSYS2 (then, in its shell, run
"pacman -S --needed make pkgconf diffutils zip") and Visual Studio 2022 17.14 or 2026 Build Tools with the MSVC
v14.44 x64 build tools and the Windows 11 SDK 10.0.26100; unpack the archive and run, in its top folder:

  powershell -NoProfile -File deploy\windows\msvc-env.ps1 C:\msys64\usr\bin\bash.exe native/ffmpeg/build.sh

The DLLs appear in .deps\ffmpeg-build\out\@NAME@-win-x64\bin. Built with the toolset, cl.exe and link.exe builds
named above, they should be byte-identical to the shipped ones; other builds of the v14.44 tools give equivalent
DLLs whose bytes can differ. build.sh prints the builds it used and records them in build-manifest.json.
