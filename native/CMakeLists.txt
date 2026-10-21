# The native media engine (docs/m2/04-windows-engine.md §13.1): the C ABI header (include/), the portable C++20 parts
# and their tests (common/, on every OS) and, on Windows, isshoni-media.dll (windows/). Build with the presets of
# CMakePresets.json (`cmake --preset host-debug`, `cmake --build --preset host-debug`, `ctest --preset host-debug`);
# they take their dependencies from vcpkg in manifest mode (vcpkg.json, VCPKG_ROOT).
cmake_minimum_required(VERSION 3.28)

project(isshoni_media LANGUAGES C CXX)

# ---------------------------------------------------------------------------------------------------------- options
option(IM_BUILD_TESTS "Build the unit tests (GoogleTest from the vcpkg feature tests)" OFF)
option(IM_TEST_HOOKS "Test hooks: IM_ENC_FAKE, failure injection, the luma tap (preset win-x64-ci only; never shipped)"
       OFF)
option(IM_BUILD_FUZZ "Build the libFuzzer targets of tests/fuzz/ (clang; preset host-fuzz)" OFF)
set(IM_SANITIZE "" CACHE STRING "Sanitizers for every target: address, thread or address,undefined (presets *-asan, *-tsan)")
set(IM_FFMPEG_ROOT "" CACHE PATH "The unpacked FFmpeg build zip (06 §4.3): include/ at build time, bin/ at run time")
set(IM_VERSION "0.0.0-dev" CACHE STRING "Release SemVer of the engine (im_build_info, ProductVersion)")
set(IM_WINVER "0.0.0.0" CACHE STRING "Four 16-bit numbers for VERSIONINFO (06 §5.3)")
set(IM_COMMIT "" CACHE STRING "Short commit of the build (im_build_info)")

if(NOT IM_VERSION MATCHES "^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\\+[0-9A-Za-z.-]+)?$")
  message(FATAL_ERROR "IM_VERSION '${IM_VERSION}' is not a SemVer version")
endif()
if(NOT IM_WINVER MATCHES "^[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+$")
  message(FATAL_ERROR "IM_WINVER '${IM_WINVER}' is not four numbers")
endif()
string(REPLACE "." ";" _im_winver_parts "${IM_WINVER}")
foreach(_part IN LISTS _im_winver_parts)
  if(_part GREATER 65535)
    message(FATAL_ERROR "IM_WINVER '${IM_WINVER}': each part must be at most 65535")
  endif()
endforeach()
if(NOT IM_COMMIT MATCHES "^[0-9a-f]*$")
  message(FATAL_ERROR "IM_COMMIT '${IM_COMMIT}' is not a hex commit id")
endif()
if(NOT IM_SANITIZE MATCHES "^(|address|thread|address,undefined|undefined)$")
  message(FATAL_ERROR "IM_SANITIZE '${IM_SANITIZE}': use address, thread, undefined or address,undefined")
endif()

# The FFmpeg build this tree pins (ffmpeg/lock.json, 06 §4.1; written after the first publishing run). windows/ compiles
# the DLL names avcodec-<IM_AVCODEC_MAJOR>.dll and avutil-<IM_AVUTIL_MAJOR>.dll and IM_FFMPEG_TAG in (04 §9.2). Empty
# while the lock doesn't exist or doesn't name them yet.
set(IM_FFMPEG_TAG "")
set(IM_AVCODEC_MAJOR "")
set(IM_AVUTIL_MAJOR "")
set(_im_lock "${CMAKE_CURRENT_SOURCE_DIR}/ffmpeg/lock.json")
if(EXISTS "${_im_lock}")
  set_property(DIRECTORY APPEND PROPERTY CMAKE_CONFIGURE_DEPENDS "${_im_lock}")
  file(READ "${_im_lock}" _im_lock_json)
  string(JSON _tag ERROR_VARIABLE _err GET "${_im_lock_json}" tag)
  string(JSON _avcodec ERROR_VARIABLE _err GET "${_im_lock_json}" avcodecMajor)
  string(JSON _avutil ERROR_VARIABLE _err GET "${_im_lock_json}" avutilMajor)
  if(_tag MATCHES "^deps-ffmpeg-" AND _avcodec MATCHES "^[0-9]+$" AND _avutil MATCHES "^[0-9]+$")
    set(IM_FFMPEG_TAG "${_tag}")
    set(IM_AVCODEC_MAJOR "${_avcodec}")
    set(IM_AVUTIL_MAJOR "${_avutil}")
  else()
    message(STATUS "ffmpeg/lock.json names no published build yet (tag, avcodecMajor, avutilMajor)")
  endif()
endif()

# ------------------------------------------------------------------------------------------------ language, flags
set(CMAKE_C_STANDARD 11)
set(CMAKE_C_STANDARD_REQUIRED ON)
set(CMAKE_C_EXTENSIONS OFF)
set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
set(CMAKE_POSITION_INDEPENDENT_CODE ON)
# Static CRT everywhere on Windows (/MT, /MTd): no VC++ redistributable on users' PCs (04 §2, 06 D7). vcpkg's
# x64-windows-static triplet builds libopus and speexdsp the same way.
set(CMAKE_MSVC_RUNTIME_LIBRARY "MultiThreaded$<$<CONFIG:Debug>:Debug>")
set(CMAKE_MSVC_DEBUG_INFORMATION_FORMAT "ProgramDatabase")

# A build directory inside the source tree (the presets' build/<preset>) ignores itself, so git status stays clean.
file(RELATIVE_PATH _im_bin_rel "${CMAKE_CURRENT_SOURCE_DIR}" "${CMAKE_BINARY_DIR}")
if(NOT _im_bin_rel MATCHES "^\\.\\.")
  file(WRITE "${CMAKE_BINARY_DIR}/.gitignore" "# written by native/CMakeLists.txt\n*\n")
endif()

# im_build_flags: the warning, hardening and sanitizer flags every target of this tree links (04 §13.1).
add_library(im_build_flags INTERFACE)
if(MSVC)
  target_compile_options(
    im_build_flags
    INTERFACE /W4
              /WX
              /permissive-
              /utf-8
              /sdl
              /Gy
              /Gw
              /FS
              /Zc:preprocessor
              $<$<COMPILE_LANGUAGE:CXX>:/EHsc>
              $<$<COMPILE_LANGUAGE:CXX>:/Zc:__cplusplus>)
  target_compile_definitions(
    im_build_flags
    INTERFACE WIN32_LEAN_AND_MEAN
              NOMINMAX
              UNICODE
              _UNICODE
              _WIN32_WINNT=0x0A00
              WINVER=0x0A00)
  target_link_options(im_build_flags INTERFACE /DEBUG:FULL /CETCOMPAT /DYNAMICBASE /HIGHENTROPYVA /NXCOMPAT
                      "$<$<NOT:$<CONFIG:Debug>>:/OPT:REF;/OPT:ICF>")
  if(IM_SANITIZE STREQUAL "")
    # Control Flow Guard; left out of sanitizer builds, which also drop /RTC1 below.
    target_compile_options(im_build_flags INTERFACE /guard:cf)
    target_link_options(im_build_flags INTERFACE /guard:cf)
  else()
    if(NOT IM_SANITIZE STREQUAL "address")
      message(FATAL_ERROR "MSVC supports IM_SANITIZE=address only")
    endif()
    target_compile_options(im_build_flags INTERFACE /fsanitize=address)
    foreach(_lang C CXX)
      string(REPLACE "/RTC1" "" CMAKE_${_lang}_FLAGS_DEBUG "${CMAKE_${_lang}_FLAGS_DEBUG}")
    endforeach()
    target_link_options(im_build_flags INTERFACE /INCREMENTAL:NO)
  endif()
else()
  target_compile_options(im_build_flags INTERFACE -Wall -Wextra -Wpedantic -Wshadow -Werror)
  if(NOT IM_SANITIZE STREQUAL "")
    target_compile_options(im_build_flags INTERFACE -fsanitize=${IM_SANITIZE} -fno-omit-frame-pointer
                                                    -fno-sanitize-recover=all)
    target_link_options(im_build_flags INTERFACE -fsanitize=${IM_SANITIZE})
  endif()
  if(IM_BUILD_FUZZ)
    # Coverage instrumentation for libFuzzer; the fuzz targets themselves link -fsanitize=fuzzer (common/).
    target_compile_options(im_build_flags INTERFACE -fsanitize=fuzzer-no-link)
  endif()
endif()

if(IM_TEST_HOOKS)
  target_compile_definitions(im_build_flags INTERFACE IM_TEST_HOOKS=1)
endif()

# im_abi: the C ABI header, isshoni_media.h.
add_library(im_abi INTERFACE)
target_include_directories(im_abi INTERFACE "${CMAKE_CURRENT_SOURCE_DIR}/include")

# ------------------------------------------------------------------------------------------------------- targets
if(IM_BUILD_TESTS OR IM_BUILD_FUZZ)
  enable_testing()
endif()
if(IM_BUILD_TESTS)
  find_package(GTest CONFIG REQUIRED)
endif()

add_subdirectory(common)

if(WIN32 AND EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/windows/CMakeLists.txt")
  if(NOT MSVC)
    message(FATAL_ERROR "isshoni-media.dll builds with MSVC only (04 §13.1)")
  endif()
  add_subdirectory(windows)
endif()
