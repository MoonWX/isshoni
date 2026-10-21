#!/usr/bin/env bash
# Builds FFmpeg's libavcodec and libavutil as LGPL-2.1-or-later DLLs for the isshoni Windows engine, checks them and
# packs the deps release's two assets (docs/m2/06-windows-release.md §4, "06" below).
#
#   native/ffmpeg/build.sh [-w WORKDIR] [-j JOBS]
#
# Runs in MSYS2's bash (C:\msys64\usr\bin\bash.exe) on Windows x64, inside the MSVC environment that
# deploy/windows/msvc-env.ps1 sets up (VS 2022 or 2026, toolset 14.44, Windows SDK 10.0.26100.0). In CI that is
# .github/workflows/ffmpeg.yml; on a Windows PC, from the repository root:
#
#   powershell -NoProfile -File deploy\windows\msvc-env.ps1 C:\msys64\usr\bin\bash.exe native/ffmpeg/build.sh
#
# MSYS2 needs `pacman -S --needed make pkgconf diffutils zip` once; this script installs nothing and changes nothing
# outside WORKDIR (default .deps/ffmpeg-build in the repository). The same command rebuilds the DLLs from an unpacked
# source archive (below), whose src/ folder then supplies the upstream tarballs (06 §9.1, the relink check).
#
# Steps (06 §4.3):
#   1. the four upstream tarballs of native/ffmpeg/pins.json into WORKDIR/src, each checked against its SHA-256
#      (downloaded with three retries, unless already there or in the source archive's src/);
#   2. libvpl's dispatcher as a static library with the static CRT (CMake and Ninja from the VS instance);
#   3. nv-codec-headers and the AMF headers into WORKDIR/prefix;
#   4. FFmpeg: configure (06 §4.2, configure_args below), make, make install;
#   5. the checks of 06 §4.4: config.h, ffcheck (license, configuration, exactly four encoders), the DLLs' imports,
#      their names (against lock.json while it describes these inputs);
#   6. the layout ffmpeg-<ver>-r<rev>-win-x64/: bin/ (avcodec-<C>.dll, avutil-<U>.dll), pdb/, lib/ (import
#      libraries), include/, licenses/, ffmpeg-config.json (ffcheck's output) and build-manifest.json;
#   7. WORKDIR/out/ffmpeg-<ver>-r<rev>-win-x64.zip and the source archive ffmpeg-<ver>-r<rev>-source.tar, both with
#      the commit time as their timestamps (SOURCE_DATE_EPOCH), plus result.json (names, SHA-256s, majors, toolset)
#      and lock-candidate.json (the native/ffmpeg/lock.json a publishing run prints, 06 §4.1).
#
# Reproducibility (06 §4.6): configure runs in WORKDIR/build on the source in WORKDIR/build/src, and every path it
# is given (the prefixes, -I, -LIBPATH, PKG_CONFIG_PATH and the .pc files) is relative to that folder, so neither the
# compiled-in source file names nor the configuration string that avcodec_configuration() returns (../install,
# ../prefix) name a path of the build machine. /Brepro removes the timestamps and -PDBALTPATH keeps the PDB's path
# out of the DLLs, which should then depend only on the inputs and the toolset build, wherever WORKDIR is.
#
# Changing this file, pins.json or ffcheck.c changes inputsSha256 (06 §4.1): the FFmpeg workflow must then publish a
# new revision and lock.json be updated, or every consumer fails as stale.
#
# Environment: SOURCE_DATE_EPOCH and BUILD_COMMIT override the commit time and hash read from git (outside a git
# checkout they default to 1980-01-01 and "none"). GITHUB_ACTIONS=true folds the log into groups.
set -Eeuo pipefail
shopt -u patsub_replacement 2>/dev/null || true

# MSYS2's bash started directly (not as a login shell) has only the Windows PATH.
export PATH="/usr/bin:$PATH" LC_ALL=C TZ=UTC
umask 022

prog=build.sh
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../.." && pwd)
pins_file=$here/pins.json
lock_file=$here/lock.json

# DLLs the two FFmpeg DLLs may import: they exist in a clean Windows 10 22H2 System32, on N editions and on Windows
# Server too (06 §4.4). Add one only after checking that, with its reason in the pull request. The vendor runtimes
# (nvEncodeAPI64, amfrt64, the oneVPL runtimes), Media Foundation and d3d11/dxgi are loaded at run time.
system_dlls=(kernel32.dll user32.dll gdi32.dll advapi32.dll ole32.dll oleaut32.dll shell32.dll shlwapi.dll
  bcrypt.dll ntdll.dll version.dll cfgmgr32.dll setupapi.dll dxgi.dll d3d11.dll)

# The encoders FFmpeg is built with (06 D6); ffcheck.c checks the same list.
encoders='h264_nvenc,h264_amf,h264_qsv,h264_mf'

log() { printf '%s: %s\n' "$prog" "$*"; }
die() {
  printf '%s: %s\n' "$prog" "$*" >&2
  exit 1
}
group() {
  if [ "${GITHUB_ACTIONS:-}" = true ]; then
    [ "${in_group:-0}" = 1 ] && echo '::endgroup::'
    echo "::group::$*"
    in_group=1
  else
    printf '\n== %s\n' "$*"
  fi
}
endgroup() {
  if [ "${in_group:-0}" = 1 ]; then
    echo '::endgroup::'
    in_group=0
  fi
}

usage() {
  sed -n '2,/^set -E/{/^#/s/^# \{0,1\}//p;}' "${BASH_SOURCE[0]}"
}

# sha256 FILE prints the file's SHA-256 in lower-case hex.
sha256() {
  sha256sum "$1" | cut -c1-64
}

# pin ENTRY FIELD prints a string field of an entry of pins.json. Each entry is one line (06 §4.1; the desktoppkg
# tests keep it so), which keeps this script free of a JSON parser.
pin() {
  local value
  value=$(sed -n "s/^[[:space:]]*\"$1\":[[:space:]]*{\(.*\)}.*$/\1/p" "$pins_file" |
    sed -n "s/.*\"$2\":[[:space:]]*\"\([^\"]*\)\".*/\1/p")
  [ -n "$value" ] || die "native/ffmpeg/pins.json: no $1.$2"
  printf '%s\n' "$value"
}

# json_number FILE KEY and json_string FILE KEY read a top-level field of a flat JSON file written by this script,
# by ffcheck or by the FFmpeg workflow (one key per line).
json_number() { sed -n "s/^[[:space:]]*\"$2\":[[:space:]]*\([0-9][0-9]*\).*/\1/p" "$1" | head -n 1; }
json_string() { sed -n "s/^[[:space:]]*\"$2\":[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$1" | head -n 1; }

# json_escape STRING prints STRING as the inside of a JSON string.
json_escape() {
  local s=$1
  s=${s//\\/\\\\}
  s=${s//\"/\\\"}
  s=${s//$'\t'/\\t}
  s=${s//$'\r'/}
  s=${s//$'\n'/\\n}
  printf '%s' "$s"
}

# inputs_sha256 prints lock.json's inputsSha256: the SHA-256 of the lines "<sha256>  <path>" for pins.json, build.sh
# and ffcheck.c (06 §4.1; `go -C tools run ./desktoppkg ffmpeg-inputs` prints the same).
inputs_sha256() {
  local f
  for f in pins.json build.sh ffcheck.c; do
    printf '%s  native/ffmpeg/%s\n' "$(sha256 "$here/$f")" "$f"
  done | sha256sum | cut -c1-64
}

# leading_comment FILE prints the comment a header starts with (its license notice), without comment markers,
# trailing blanks and empty first and last lines.
leading_comment() {
  tr -d '\r' <"$1" | awk '
		NR == 1 && /^[[:space:]]*\/\*/ { block = 1 }
		NR == 1 && /^[[:space:]]*\/\// { slashes = 1 }
		block {
			line = $0; last = (line ~ /\*\//)
			sub(/\*+\/.*$/, "", line); sub(/^[[:space:]]*\/\*+/, "", line); sub(/^[[:space:]]*\*/, "", line)
			sub(/^ /, "", line); sub(/[[:space:]]+$/, "", line); lines[++n] = line
			if (last) exit
			next
		}
		slashes {
			if ($0 !~ /^[[:space:]]*\/\//) exit
			line = $0; sub(/^[[:space:]]*\/\/ ?/, "", line); sub(/[[:space:]]+$/, "", line); lines[++n] = line
			next
		}
		{ exit }
		END {
			first = 1; while (first <= n && lines[first] == "") first++
			while (n >= first && lines[n] == "") n--
			for (i = first; i <= n; i++) print lines[i]
		}'
}

# header_notices DIR TITLE writes the license notices of a header-only package that ships no license file
# (nv-codec-headers, the AMF headers): each distinct notice once, after the headers that carry it.
header_notices() {
  local dir=$1 title=$2 f notice
  printf '%s\n' "$title"
  (cd "$dir" && find . -type f -name '*.h' | sed 's|^\./||' | sort) | while IFS= read -r f; do
    notice=$(leading_comment "$dir/$f")
    # One record per header: the file name, then its notice with newlines as \001.
    printf '%s\t%s\n' "$f" "$(printf '%s' "$notice" | tr '\n' '\001')"
  done | awk -F '\t' '
		$2 == "" { printf "build.sh: %s has no license notice\n", $1 > "/dev/stderr"; bad = 1; next }
		!($2 in files) { order[++n] = $2 }
		{ files[$2] = files[$2] "\n  " $1 }
		END {
			if (bad || n == 0) exit 1
			for (i = 1; i <= n; i++) {
				text = order[i]; gsub(/\001/, "\n", text)
				printf "\n----------------------------------------------------------------------\nFiles:%s\n\n%s\n", files[order[i]], text
			}
		}'
}

# render TEMPLATE NAME=VALUE... prints TEMPLATE with each @NAME@ replaced by its VALUE, and fails when a placeholder
# is left.
render() {
  local template=$1 text pair
  text=$(<"$template")
  shift
  for pair in "$@"; do
    text=${text//@${pair%%=*}@/${pair#*=}}
  done
  case $text in *@[A-Z_]*@*) die "$(basename "$template") has a placeholder build.sh does not fill" ;; esac
  printf '%s\n' "$text"
}

# imports DLL prints "static NAME" and "delay NAME" for each DLL that DLL imports, as dumpbin lists them.
imports() {
  MSYS2_ARG_CONV_EXCL='*' dumpbin.exe -nologo -dependents "$(cygpath -m "$1")" | tr -d '\r' | awk '
		/Image has the following delay load dependencies:/ { kind = "delay"; skip = 1; next }
		/Image has the following dependencies:/ { kind = "static"; skip = 1; next }
		kind != "" && /^[[:space:]]*$/ { if (skip) { skip = 0; next } kind = ""; next }
		kind != "" { skip = 0; gsub(/^[[:space:]]+|[[:space:]]+$/, ""); print kind, $0 }'
}

# import_problem NAME [SIBLING] prints why the DLL NAME (lower case) may not be imported, or nothing when it may:
# a system DLL of system_dlls, or SIBLING (avcodec's avutil).
import_problem() {
  local name=$1 sibling=${2:-} d
  case $name in
  vcruntime* | msvcp* | api-ms-win-crt-* | ucrtbase* | concrt* | vccorlib*)
    echo 'a C runtime DLL: the build must use the static CRT (-MT, 06 D7)'
    return
    ;;
  mfplat.dll | mf.dll | mfreadwrite.dll | evr.dll)
    echo 'Media Foundation, which must be loaded at run time: it is missing on N editions and Windows Server (06 §4.4)'
    return
    ;;
  nvencodeapi64.dll | nvcuda.dll | amfrt64.dll | libvpl*.dll | libmfx*.dll)
    echo 'a vendor runtime, which must be loaded at run time (06 §4.4)'
    return
    ;;
  esac
  if [ -n "$sibling" ] && [ "$name" = "$sibling" ]; then
    return
  fi
  for d in "${system_dlls[@]}"; do
    if [ "$name" = "$d" ]; then
      return
    fi
  done
  echo "not on build.sh's list of system DLLs"
}

# check_imports DLL [SIBLING] fails unless DLL imports only DLLs that import_problem accepts.
check_imports() {
  local dll=$1 sibling=${2:-} list kind name why bad=0
  list=$(imports "$dll")
  [ -n "$list" ] || die "dumpbin listed no imports for $dll"
  while read -r kind name; do
    why=$(import_problem "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" "$sibling")
    if [ -n "$why" ]; then
      log "error: $(basename "$dll") imports $name ($kind): $why"
      bad=1
    fi
  done <<<"$list"
  [ "$bad" = 0 ] || die "$(basename "$dll") imports DLLs other than system DLLs (06 §4.4)"
  log "$(basename "$dll") imports: $(printf '%s\n' "$list" | awk '{printf "%s%s (%s)", sep, $2, $1; sep = ", "}')"
}

# fetch NAME URL SHA256 puts the tarball NAME into $src_dir: from the source archive's src/, a previous run, or a
# download (three retries), and fails unless its SHA-256 matches.
fetch() {
  local name=$1 url=$2 want=$3 dst=$src_dir/$1
  if [ -f "$dst" ] && [ "$(sha256 "$dst")" = "$want" ]; then
    log "$name: present"
    return
  fi
  rm -f "$dst" "$dst.part"
  if [ -f "$repo/src/$name" ]; then
    cp "$repo/src/$name" "$dst.part"
    log "$name: from src/"
  else
    curl -fsSL --proto '=https' --retry 3 --retry-all-errors --retry-delay 5 --connect-timeout 30 --max-time 900 \
      -o "$dst.part" "$url" || die "$name: download of $url failed"
    log "$name: downloaded from $url"
  fi
  local got
  got=$(sha256 "$dst.part")
  if [ "$got" != "$want" ]; then
    rm -f "$dst.part"
    die "$name: SHA-256 $got, but pins.json pins $want"
  fi
  mv "$dst.part" "$dst"
}

# unpack TARBALL DIR extracts TARBALL into a fresh DIR without its top-level folder.
unpack() {
  rm -rf "$2"
  mkdir -p "$2"
  tar -xf "$1" -C "$2" --strip-components=1 --no-same-owner
}

main() {
  local work='' jobs=''
  while getopts 'w:j:h' opt; do
    case $opt in
    w) work=$OPTARG ;;
    j) jobs=$OPTARG ;;
    h)
      usage
      exit 0
      ;;
    *)
      usage >&2
      exit 2
      ;;
    esac
  done
  shift $((OPTIND - 1))
  [ $# -eq 0 ] || {
    usage >&2
    exit 2
  }

  group 'Preflight'
  [ "$(uname -o 2>/dev/null)" = Msys ] || die "run this in MSYS2's bash (C:\\msys64\\usr\\bin\\bash.exe), not $(uname -s)"
  # vcvars64.bat sets these (deploy/windows/msvc-env.ps1).
  local v vc_tools_dir='' toolset='' sdk=''
  for v in VCToolsInstallDir VCToolsVersion WindowsSDKVersion; do
    [ -n "${!v:-}" ] || die "$v is not set: run this inside deploy/windows/msvc-env.ps1's MSVC environment"
  done
  vc_tools_dir=$(printenv VCToolsInstallDir)
  toolset=$(printenv VCToolsVersion)
  sdk=$(printenv WindowsSDKVersion)
  sdk=${sdk%\\}
  # MSVC's tools before MSYS2's /usr/bin, whose coreutils link.exe would otherwise shadow MSVC's linker; /usr/bin
  # before the rest of the Windows PATH (Git's and other Unix-like tools there).
  local msvc_bin
  msvc_bin=$(cygpath -u "$vc_tools_dir")
  msvc_bin=${msvc_bin%/}/bin/Hostx64/x64
  [ -x "$msvc_bin/cl.exe" ] || die "no cl.exe in $msvc_bin"
  export PATH="$msvc_bin:/usr/bin:$PATH"
  local tool
  for tool in make pkg-config cmp diff tar xz gzip curl sha256sum zip cygpath dumpbin.exe lib.exe; do
    command -v "$tool" >/dev/null || die "$tool not found (MSYS2: pacman -S --needed make pkgconf diffutils zip)"
  done
  log "MSVC toolset $toolset, Windows SDK $sdk, $(cl.exe 2>&1 | tr -d '\r' | head -n 1)"

  # CMake and Ninja of the VS instance (the component "C++ CMake tools for Windows"), else from PATH.
  local cmake_exe='' ninja_exe='' vs_cmake=''
  if [ -n "${VSINSTALLDIR:-}" ]; then
    vs_cmake=$(cygpath -u "$VSINSTALLDIR")
    vs_cmake=${vs_cmake%/}/Common7/IDE/CommonExtensions/Microsoft/CMake
    if [ -x "$vs_cmake/CMake/bin/cmake.exe" ]; then
      cmake_exe=$vs_cmake/CMake/bin/cmake.exe
    fi
    if [ -x "$vs_cmake/Ninja/ninja.exe" ]; then
      ninja_exe=$vs_cmake/Ninja/ninja.exe
    fi
  fi
  if [ -z "$cmake_exe" ]; then
    cmake_exe=$(command -v cmake.exe) || die 'no CMake: install the VS component "C++ CMake tools for Windows"'
  fi
  if [ -z "$ninja_exe" ]; then
    ninja_exe=$(command -v ninja.exe) || die 'no Ninja: install the VS component "C++ CMake tools for Windows"'
  fi

  # Inputs.
  local revision ff_version ff_url ff_sha nv_tag nv_url nv_sha amf_tag amf_url amf_sha vpl_tag vpl_url vpl_sha
  revision=$(sed -n 's/^[[:space:]]*"revision":[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$pins_file")
  [ -n "$revision" ] || die "native/ffmpeg/pins.json: no revision"
  ff_version=$(pin ffmpeg version)
  ff_url=$(pin ffmpeg url)
  ff_sha=$(pin ffmpeg sha256)
  nv_tag=$(pin nvCodecHeaders tag)
  nv_url=$(pin nvCodecHeaders url)
  nv_sha=$(pin nvCodecHeaders sha256)
  amf_tag=$(pin amfHeaders tag)
  amf_url=$(pin amfHeaders url)
  amf_sha=$(pin amfHeaders sha256)
  vpl_tag=$(pin libvpl tag)
  vpl_url=$(pin libvpl url)
  vpl_sha=$(pin libvpl sha256)
  local ff_tar=ffmpeg-$ff_version.tar.xz nv_tar=nv-codec-headers-$nv_tag.tar.gz amf_tar=AMF-headers-$amf_tag.tar.gz
  local vpl_tar=libvpl-$vpl_tag.tar.gz
  local name=ffmpeg-$ff_version-r$revision tag=deps-ffmpeg-$ff_version-r$revision
  local inputs
  inputs=$(inputs_sha256)

  local commit epoch
  commit=${BUILD_COMMIT:-$(git -C "$(cygpath -m "$repo")" rev-parse HEAD 2>/dev/null || echo none)}
  epoch=${SOURCE_DATE_EPOCH:-$(git -C "$(cygpath -m "$repo")" log -1 --format=%ct HEAD 2>/dev/null || echo 315532800)}
  log "$name ($tag), inputs $inputs, commit $commit, SOURCE_DATE_EPOCH $epoch"

  [ -n "$work" ] || work=$repo/.deps/ffmpeg-build
  mkdir -p "$work"
  work=$(cd "$work" && pwd)
  case $work in *[[:space:]]*) die "the work directory must not contain spaces: $work" ;; esac
  [ -n "$jobs" ] || jobs=$(nproc)
  src_dir=$work/src
  local deps=$work/deps prefix=$work/prefix build=$work/build install=$work/install out=$work/out
  local dist=$out/$name-win-x64
  mkdir -p "$src_dir"
  rm -rf "$deps" "$prefix" "$build" "$install" "$out"
  mkdir -p "$deps" "$prefix/include" "$prefix/lib" "$build" "$dist"
  endgroup

  group 'Sources (pins.json)'
  fetch "$ff_tar" "$ff_url" "$ff_sha"
  fetch "$nv_tar" "$nv_url" "$nv_sha"
  fetch "$amf_tar" "$amf_url" "$amf_sha"
  fetch "$vpl_tar" "$vpl_url" "$vpl_sha"
  unpack "$src_dir/$ff_tar" "$build/src"
  unpack "$src_dir/$nv_tar" "$deps/nv-codec-headers"
  unpack "$src_dir/$amf_tar" "$deps/amf"
  unpack "$src_dir/$vpl_tar" "$deps/libvpl"
  endgroup

  group "libvpl $vpl_tag (static, /MT)"
  MSYS2_ARG_CONV_EXCL='*' "$cmake_exe" -G Ninja -S "$(cygpath -m "$deps/libvpl")" -B "$(cygpath -m "$deps/libvpl-build")" \
    -DCMAKE_MAKE_PROGRAM="$(cygpath -m "$ninja_exe")" \
    -DCMAKE_C_COMPILER=cl -DCMAKE_CXX_COMPILER=cl \
    -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF \
    -DCMAKE_MSVC_RUNTIME_LIBRARY=MultiThreaded -DUSE_MSVC_STATIC_RUNTIME=ON \
    -DBUILD_TESTS=OFF -DBUILD_EXAMPLES=OFF -DINSTALL_EXAMPLES=OFF \
    -DCMAKE_INSTALL_PREFIX="$(cygpath -m "$prefix")"
  MSYS2_ARG_CONV_EXCL='*' "$cmake_exe" --build "$(cygpath -m "$deps/libvpl-build")" --parallel "$jobs"
  MSYS2_ARG_CONV_EXCL='*' "$cmake_exe" --install "$(cygpath -m "$deps/libvpl-build")"
  if [ ! -f "$prefix/lib/vpl.lib" ] || [ ! -f "$prefix/lib/pkgconfig/vpl.pc" ]; then
    die 'libvpl did not install vpl.lib and vpl.pc'
  fi
  # vpl.pc names the dispatcher's system libraries for MinGW only (whose gcc links advapi32 anyway); with MSVC the
  # static dispatcher needs the registry API (advapi32) and COM (ole32), or configure's libvpl check cannot link.
  sed -i 's/^Libs\.private:.*$/Libs.private: -ladvapi32 -lole32/' "$prefix/lib/pkgconfig/vpl.pc"
  grep -q '^Libs\.private: -ladvapi32 -lole32$' "$prefix/lib/pkgconfig/vpl.pc" ||
    printf 'Libs.private: -ladvapi32 -lole32\n' >>"$prefix/lib/pkgconfig/vpl.pc"
  endgroup

  group "nv-codec-headers $nv_tag, AMF headers $amf_tag"
  make -C "$deps/nv-codec-headers" PREFIX="$prefix" install
  [ -f "$prefix/lib/pkgconfig/ffnvcodec.pc" ] || die "nv-codec-headers did not install ffnvcodec.pc"
  # Its prefix is absolute; relative to the .pc file (as libvpl's), configure sees ../prefix/... like everything else.
  # shellcheck disable=SC2016 # ${pcfiledir} is pkg-config's variable
  sed -i 's|^prefix=.*$|prefix=${pcfiledir}/../..|' "$prefix/lib/pkgconfig/ffnvcodec.pc"
  [ -d "$deps/amf/AMF/core" ] || die "$amf_tar has no AMF/core folder"
  cp -R "$deps/amf/AMF" "$prefix/include/AMF"
  endgroup

  group "FFmpeg $ff_version: configure"
  # 06 §4.2, with every path relative to the build directory (see the header), plus two linker options:
  # -PDBALTPATH keeps the PDB's path out of the DLLs, and -INCREMENTAL:NO makes a release link (link.exe links
  # incrementally by default with FFmpeg's -debug, with jump thunks and padding in the DLLs).
  local configure_args=(
    --toolchain=msvc --arch=x86_64 --target-os=win64 --prefix=../install
    --disable-gpl --disable-nonfree --disable-version3
    --enable-shared --disable-static
    --disable-programs --disable-doc --disable-everything --disable-autodetect
    --disable-network --disable-x86asm
    --disable-avformat --disable-avdevice --disable-avfilter --disable-swscale --disable-swresample
    --enable-avcodec --enable-avutil
    --enable-d3d11va --enable-mediafoundation
    --enable-ffnvcodec --enable-nvenc --enable-amf --enable-libvpl
    --enable-encoder="$encoders"
    --extra-cflags='-MT -Brepro -I../prefix/include'
    --extra-ldflags='-Brepro -PDBALTPATH:%_PDB% -INCREMENTAL:NO -LIBPATH:../prefix/lib'
    --pkg-config-flags=--static
  )
  log "./src/configure ${configure_args[*]}"
  (cd "$build" && PKG_CONFIG_PATH=../prefix/lib/pkgconfig ./src/configure "${configure_args[@]}") || {
    tail -n 80 "$build/ffbuild/config.log" >&2 || true
    die 'configure failed (the end of ffbuild/config.log is above)'
  }
  endgroup

  group "FFmpeg $ff_version: make -j$jobs"
  make -C "$build" -j"$jobs"
  make -C "$build" install
  endgroup

  group 'Checks (06 §4.4)'
  local d
  for d in '#define CONFIG_GPL 0' '#define CONFIG_NONFREE 0' '#define CONFIG_VERSION3 0'; do
    grep -qx "$d" "$build/config.h" || die "config.h lacks \"$d\""
  done
  log 'config.h: CONFIG_GPL 0, CONFIG_NONFREE 0, CONFIG_VERSION3 0'

  mkdir -p "$dist/bin" "$dist/lib" "$dist/pdb" "$dist/include" "$dist/licenses"
  local dll lib count=0
  for dll in "$install"/bin/*.dll; do
    [ -e "$dll" ] || continue
    count=$((count + 1))
    case $(basename "$dll") in
    avcodec-[0-9]*.dll | avutil-[0-9]*.dll) cp "$dll" "$dist/bin/" ;;
    *) die "unexpected DLL $(basename "$dll") in the FFmpeg install" ;;
    esac
  done
  [ "$count" = 2 ] || die "the FFmpeg install has $count DLLs, want avcodec-<C>.dll and avutil-<U>.dll"
  for lib in avcodec avutil; do
    # FFmpeg installs a DLL's import library next to the DLL, in bin/.
    if [ -f "$install/bin/$lib.lib" ]; then
      cp "$install/bin/$lib.lib" "$dist/lib/"
    elif [ -f "$install/lib/$lib.lib" ]; then
      cp "$install/lib/$lib.lib" "$dist/lib/"
    else
      die "no import library $lib.lib in the FFmpeg install"
    fi
    set -- "$build/lib$lib/$lib"-*.pdb
    [ $# = 1 ] && [ -f "$1" ] || die "no single PDB for $lib in lib$lib/"
    cp "$1" "$dist/pdb/"
  done
  cp -R "$install/include/." "$dist/include/"

  # ffcheck (06 §4.4): built against the build's headers and import libraries, run next to the two DLLs.
  local ffc=$work/ffcheck
  rm -rf "$ffc"
  mkdir -p "$ffc"
  cp "$dist"/bin/*.dll "$ffc/"
  (cd "$ffc" && MSYS2_ARG_CONV_EXCL='*' cl.exe -nologo -MT -W4 -WX -external:W0 \
    -external:I "$(cygpath -m "$dist/include")" "$(cygpath -m "$here/ffcheck.c")" -Feffcheck.exe \
    -link -LIBPATH:"$(cygpath -m "$dist/lib")" avcodec.lib avutil.lib) || die 'ffcheck.c did not build'
  (cd "$ffc" && ./ffcheck.exe "$(cygpath -m "$dist/ffmpeg-config.json")") ||
    die "ffcheck failed (exit $?): license, configuration or encoder list (06 §4.4)"
  log "ffcheck: $(json_string "$dist/ffmpeg-config.json" license); encoders $encoders"

  local avcodec_major avutil_major avcodec_dll avutil_dll
  avcodec_major=$(json_number "$dist/ffmpeg-config.json" avcodecMajor)
  avutil_major=$(json_number "$dist/ffmpeg-config.json" avutilMajor)
  avcodec_dll=avcodec-$avcodec_major.dll
  avutil_dll=avutil-$avutil_major.dll
  [ -f "$dist/bin/$avcodec_dll" ] && [ -f "$dist/bin/$avutil_dll" ] ||
    die "the DLLs are not named $avcodec_dll and $avutil_dll after the versions ffcheck read: $(ls "$dist/bin")"

  check_imports "$dist/bin/$avutil_dll"
  check_imports "$dist/bin/$avcodec_dll" "$avutil_dll"

  local avcodec_sha avutil_sha
  avcodec_sha=$(sha256 "$dist/bin/$avcodec_dll")
  avutil_sha=$(sha256 "$dist/bin/$avutil_dll")
  if [ -f "$lock_file" ] && [ "$(json_string "$lock_file" inputsSha256)" = "$inputs" ]; then
    local lock_c lock_u
    lock_c=$(json_number "$lock_file" avcodecMajor)
    lock_u=$(json_number "$lock_file" avutilMajor)
    [ "$avcodec_dll" = "avcodec-$lock_c.dll" ] && [ "$avutil_dll" = "avutil-$lock_u.dll" ] ||
      die "the DLLs are $avcodec_dll and $avutil_dll, lock.json names avcodec-$lock_c.dll and avutil-$lock_u.dll"
    if grep -q "\"$avcodec_dll\": \"$avcodec_sha\"" "$lock_file" && grep -q "\"$avutil_dll\": \"$avutil_sha\"" "$lock_file"; then
      log "the DLLs are byte-identical to lock.json's ($(json_string "$lock_file" tag))"
    else
      log "the DLLs differ from lock.json's ($(json_string "$lock_file" tag), toolset $(json_string "$lock_file" msvcToolset); this build: $toolset)"
    fi
  else
    log 'lock.json does not describe these inputs (absent, or pins.json, build.sh or ffcheck.c changed): names checked against ffcheck only'
  fi
  endgroup

  group 'Layout, licenses, manifest'
  mkdir -p "$dist/licenses/ffmpeg" "$dist/licenses/libvpl" "$dist/licenses/nv-codec-headers" "$dist/licenses/amf"
  cp "$build/src/COPYING.LGPLv2.1" "$build/src/LICENSE.md" "$dist/licenses/ffmpeg/"
  cp "$deps/libvpl/LICENSE" "$dist/licenses/libvpl/LICENSE"
  header_notices "$deps/nv-codec-headers/include" \
    "nv-codec-headers $nv_tag ($nv_url): MIT. The package has no license file; each header carries its notice." \
    >"$dist/licenses/nv-codec-headers/LICENSE"
  header_notices "$deps/amf" \
    "AMD AMF headers $amf_tag ($amf_url): MIT. The package has no license file; each header carries its notice." \
    >"$dist/licenses/amf/LICENSE.txt"

  local configuration avcodec_version avutil_version
  configuration=$(sed -n 's/^[[:space:]]*"configuration":[[:space:]]*"\(.*\)",\{0,1\}[[:space:]]*$/\1/p' "$dist/ffmpeg-config.json" |
    sed 's/\\"/"/g; s/\\\\/\\/g')
  [ -n "$configuration" ] || die 'ffmpeg-config.json has no configuration'
  avcodec_version=$avcodec_major.$(json_number "$dist/ffmpeg-config.json" avcodecMinor).$(json_number "$dist/ffmpeg-config.json" avcodecMicro)
  avutil_version=$avutil_major.$(json_number "$dist/ffmpeg-config.json" avutilMinor).$(json_number "$dist/ffmpeg-config.json" avutilMicro)
  render "$here/README-LGPL.txt" FFMPEG_VERSION="$ff_version" REVISION="$revision" TAG="$tag" NAME="$name" \
    AVCODEC_DLL="$avcodec_dll" AVUTIL_DLL="$avutil_dll" AVCODEC_VERSION="$avcodec_version" \
    AVUTIL_VERSION="$avutil_version" MSVC_TOOLSET="$toolset" CONFIGURATION="$configuration" \
    >"$dist/licenses/ffmpeg/README-LGPL.txt"

  local msys_pkgs=''
  if command -v pacman >/dev/null; then
    msys_pkgs=$(pacman -Q make pkgconf diffutils zip tar 2>/dev/null |
      awk '{printf "%s\"%s\": \"%s\"", sep, $1, $2; sep = ", "}')
  fi
  local cmake_version
  cmake_version=$(MSYS2_ARG_CONV_EXCL='*' "$cmake_exe" --version | tr -d '\r' | sed -n '1s/^cmake version //p')
  cat >"$dist/build-manifest.json" <<EOF
{
  "name": "$name",
  "tag": "$tag",
  "revision": $revision,
  "inputsSha256": "$inputs",
  "commit": "$(json_escape "$commit")",
  "sourceDateEpoch": $epoch,
  "inputs": {
    "ffmpeg": {"version": "$ff_version", "file": "$ff_tar", "sha256": "$ff_sha"},
    "nvCodecHeaders": {"tag": "$nv_tag", "file": "$nv_tar", "sha256": "$nv_sha"},
    "amfHeaders": {"tag": "$amf_tag", "file": "$amf_tar", "sha256": "$amf_sha"},
    "libvpl": {"tag": "$vpl_tag", "file": "$vpl_tar", "sha256": "$vpl_sha"}
  },
  "msvcToolset": "$(json_escape "$toolset")",
  "windowsSdk": "$(json_escape "$sdk")",
  "vsInstance": "$(json_escape "${MSVC_ENV_VS_VERSION:-}")",
  "vsName": "$(json_escape "${MSVC_ENV_VS_NAME:-}")",
  "cmake": "$(json_escape "$cmake_version")",
  "msys2": {$msys_pkgs},
  "avcodecMajor": $avcodec_major,
  "avutilMajor": $avutil_major,
  "dlls": {"$avcodec_dll": "$avcodec_sha", "$avutil_dll": "$avutil_sha"}
}
EOF
  endgroup

  group 'Archives'
  # Same timestamps, order and attributes on every build: the commit time, names sorted, no extra fields.
  find "$dist" -exec touch -h -d "@$epoch" {} +
  (cd "$out" && find "$name-win-x64" -type f | sort | zip -X -D -q -9 -@ "$name-win-x64.zip")

  local srcpkg=$out/$name-source
  mkdir -p "$srcpkg/src" "$srcpkg/native/ffmpeg" "$srcpkg/deploy/windows"
  cp "$src_dir/$ff_tar" "$src_dir/$nv_tar" "$src_dir/$amf_tar" "$src_dir/$vpl_tar" "$srcpkg/src/"
  cp "$here/pins.json" "$here/build.sh" "$here/ffcheck.c" "$here/README-LGPL.txt" "$srcpkg/native/ffmpeg/"
  # The MSVC environment step, so the archive alone rebuilds the DLLs (06 §9.1's relink check).
  local f
  for f in msvc-env.ps1 tools.json; do
    if [ -f "$repo/deploy/windows/$f" ]; then
      cp "$repo/deploy/windows/$f" "$srcpkg/deploy/windows/"
    fi
  done
  (cd "$srcpkg/src" && for f in *; do printf '%s  %s\n' "$(sha256 "$f")" "$f"; done) >"$srcpkg/SHA256SUMS"
  {
    printf 'FFmpeg %s for isshoni (%s), libavcodec %s and libavutil %s\n\n' "$ff_version" "$tag" "$avcodec_version" "$avutil_version"
    printf 'Configuration (avcodec_configuration()):\n%s\n\n' "$configuration"
    printf 'MSVC toolset %s, Windows SDK %s\n' "$toolset" "$sdk"
    printf 'Build: native/ffmpeg/build.sh in MSYS2 bash, inside deploy/windows/msvc-env.ps1 (see README-LGPL.txt)\n'
  } >"$srcpkg/CONFIGURE.txt"
  tar --format=gnu --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$epoch" --mode='u=rwX,go=rX' \
    -cf "$out/$name-source.tar" -C "$out" "$name-source"

  local zip_sha tar_sha
  zip_sha=$(sha256 "$out/$name-win-x64.zip")
  tar_sha=$(sha256 "$out/$name-source.tar")
  cat >"$out/result.json" <<EOF
{
  "name": "$name",
  "tag": "$tag",
  "revision": $revision,
  "zip": "$name-win-x64.zip",
  "zipSha256": "$zip_sha",
  "sourceTar": "$name-source.tar",
  "sourceTarSha256": "$tar_sha",
  "inputsSha256": "$inputs",
  "commit": "$(json_escape "$commit")",
  "avcodecMajor": $avcodec_major,
  "avutilMajor": $avutil_major,
  "avcodecDll": "$avcodec_dll",
  "avcodecSha256": "$avcodec_sha",
  "avutilDll": "$avutil_dll",
  "avutilSha256": "$avutil_sha",
  "msvcToolset": "$(json_escape "$toolset")",
  "windowsSdk": "$(json_escape "$sdk")"
}
EOF
  cat >"$out/lock-candidate.json" <<EOF
{
  "revision": $revision,
  "tag": "$tag",
  "inputsSha256": "$inputs",
  "assets": {
    "$name-win-x64.zip": "$zip_sha",
    "$name-source.tar": "$tar_sha"
  },
  "avcodecMajor": $avcodec_major,
  "avutilMajor": $avutil_major,
  "dlls": {"$avcodec_dll": "$avcodec_sha", "$avutil_dll": "$avutil_sha"},
  "msvcToolset": "$(json_escape "$toolset")",
  "commit": "$(json_escape "$commit")"
}
EOF
  endgroup

  log "$out/$name-win-x64.zip $zip_sha"
  log "$out/$name-source.tar $tar_sha"
  log "$avcodec_dll $avcodec_sha"
  log "$avutil_dll $avutil_sha"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  trap 'echo "$prog: failed at line $LINENO: $BASH_COMMAND" >&2' ERR
  main "$@"
fi
