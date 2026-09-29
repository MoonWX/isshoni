#!/bin/sh
# Cross-compiles native/engine.cpp into dist/isshoni_audio.dll using MinGW-w64 in Docker.
# (Official builds use MSVC via CMakeLists.txt; this is for development from macOS/Linux.)
set -eu
cd "$(dirname "$0")/.."
mkdir -p dist
docker image inspect isshoni-mingw >/dev/null 2>&1 || docker build -q -t isshoni-mingw -f build/Dockerfile.mingw build
docker run --rm -v "$PWD":/src -w /src isshoni-mingw \
  x86_64-w64-mingw32-g++-posix -std=c++20 -O2 -Wall -Wextra -Wno-unknown-pragmas \
    -D_WIN32_WINNT=0x0A00 -DWINVER=0x0A00 -DUNICODE -D_UNICODE \
    -shared -o dist/isshoni_audio.dll native/engine.cpp \
    -static -static-libgcc -static-libstdc++ -lole32 -lwinmm
echo "built dist/isshoni_audio.dll"
