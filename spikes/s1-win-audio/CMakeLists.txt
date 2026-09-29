# MSVC build of the spike's native audio engine (the official toolchain for the
# Windows engine per the plan). From a "Developer PowerShell for VS":
#   cmake -S . -B build/msvc -G "Visual Studio 17 2022" -A x64
#   cmake --build build/msvc --config Release
# Output: build/msvc/Release/isshoni_audio.dll (copy next to s1.exe).
cmake_minimum_required(VERSION 3.21)
project(isshoni_audio LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_MSVC_RUNTIME_LIBRARY "MultiThreaded$<$<CONFIG:Debug>:Debug>")  # static CRT: no VC++ redist needed

add_library(isshoni_audio SHARED native/engine.cpp)
target_compile_definitions(isshoni_audio PRIVATE _WIN32_WINNT=0x0A00 WINVER=0x0A00 UNICODE _UNICODE)
target_link_libraries(isshoni_audio PRIVATE ole32 winmm)
if(MSVC)
  target_compile_options(isshoni_audio PRIVATE /W4 /permissive- /utf-8 /EHsc)
endif()
