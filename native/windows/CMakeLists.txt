# native/windows: isshoni-media.dll and its Windows tests (docs/m2/04-windows-engine.md §4, §13; "04" below). The root
# native/CMakeLists.txt adds this folder on Windows only, with MSVC; build it with the presets win-x64-release (the
# shipped DLL), win-x64-ci (test hooks and tests), win-x64-debug and win-x64-asan, in the environment of
# deploy/windows/msvc-env.ps1 (task native:build on a Windows PC, the windows-native job in CI).
#
# Layout. Later slices add files only (README §5, "Shared files and their owners"):
#   src/<area>/*.cpp                 the engine: every .cpp under src/ goes into the object library im_engine, which the
#                                    DLL and every test executable link; headers are included as "<area>/<name>.h".
#                                    Each area's entry points are declared in its header, and the skeleton's versions
#                                    live in <area>/*_stub.cpp until the area's slice replaces them.
#   isshoni-media.def                the exports: exactly the IM_API functions of native/include/isshoni_media.h
#   resources/version.rc.in          VERSIONINFO, configured from IM_VERSION and IM_WINVER (04 §13.4)
#   shaders/<name>_{vs,ps,cs}.hlsl   compiled at build time by fxc into <build>/shaders/<name>.h: entry point main,
#                                    bytecode array g_<name>, included as "shaders/<name>.h" (04 §13.1)
#   third_party/                     headers compiled in (OpenH264's, 04 §9.9), on the include path
#   tests/*_test.cpp                 one GoogleTest executable per file (IM_BUILD_TESTS), linked with im_engine and
#                                    registered with ctest under the file's name; each file belongs to one slice
#   tests/support/                   the marker and probe DLLs of dll_search_test, built with the tests, never shipped
#
# System libraries: the ones 04 §13.1 names are linked below. An area that needs another one names it in its source
# with #pragma comment(lib, "<name>.lib"), and its DLL goes onto deploy/windows/dll-imports.txt in the same change
# (tools/desktoppkg dll-check fails on an unlisted import). C++/WinRT code (README WA50) does that for windowsapp.lib:
# the linker searches such default libraries after the ones on its command line, so the functions kernel32.lib and the
# others below provide keep coming from their classic DLLs, and only WinRT's resolve to windowsapp's API sets.
#
# mfplat.dll and avrt.dll are delay-loaded (absent on N editions and Windows Server without Media Foundation,
# 04 §13.1), through src/engine/delayload.cpp's System32-only hook. /DEPENDENTLOADFLAG is not set: README WA11's
# dll_search_test records whether the delay-load helper would honor it (06 §5.1's conditional load-config check).

enable_language(RC)

get_filename_component(IM_NATIVE_DIR "${CMAKE_CURRENT_SOURCE_DIR}/.." ABSOLUTE)

# ---------------------------------------------------------------------------------------------- the engine's code
file(GLOB_RECURSE IM_ENGINE_SOURCES CONFIGURE_DEPENDS "${CMAKE_CURRENT_SOURCE_DIR}/src/*.cpp")
add_library(im_engine OBJECT ${IM_ENGINE_SOURCES})
target_include_directories(im_engine PUBLIC "${CMAKE_CURRENT_SOURCE_DIR}/src" "${CMAKE_CURRENT_SOURCE_DIR}/third_party")
target_compile_definitions(
  im_engine
  PUBLIC IM_BUILDING_DLL
  PRIVATE "IM_BUILD_VERSION=\"${IM_VERSION}\"" "IM_BUILD_COMMIT=\"${IM_COMMIT}\""
          "IM_BUILD_FFMPEG_TAG=\"${IM_FFMPEG_TAG}\"")
if(IM_AVCODEC_MAJOR AND IM_AVUTIL_MAJOR)
  # The FFmpeg DLL names the engine loads at run time (04 §9.2): avcodec-<major>.dll, avutil-<major>.dll.
  target_compile_definitions(im_engine PRIVATE IM_AVCODEC_MAJOR=${IM_AVCODEC_MAJOR} IM_AVUTIL_MAJOR=${IM_AVUTIL_MAJOR})
endif()
target_link_libraries(
  im_engine
  PUBLIC im_common
         im_build_flags
         kernel32
         user32
         ole32
         oleaut32
         d3d11
         dxgi
         dxguid
         dwmapi
         shcore
         synchronization
         avrt
         mfplat
         delayimp)
# 05's portable libraries and 04's Opus stage, as their slices add them (native/common/CMakeLists.txt).
foreach(_area audio audiopolicy audiomix)
  if(TARGET im_${_area})
    target_link_libraries(im_engine PUBLIC im_${_area})
  endif()
endforeach()
# LNK4199 ("/DELAYLOAD ignored; no imports found") while no code imports a delay-loaded DLL yet.
target_link_options(im_engine INTERFACE /DELAYLOAD:mfplat.dll /DELAYLOAD:avrt.dll /IGNORE:4199)

# FFmpeg is loaded at run time and never import-linked (04 §9.2): only the build zip's headers are used here.
# IM_FFMPEG_ROOT is optional until README WA24 wires the locked build into the windows-native job.
if(IM_FFMPEG_ROOT)
  if(NOT EXISTS "${IM_FFMPEG_ROOT}/include/libavcodec/avcodec.h")
    message(FATAL_ERROR "IM_FFMPEG_ROOT '${IM_FFMPEG_ROOT}' has no include/libavcodec/avcodec.h: pass the unpacked "
                        "FFmpeg build zip (task ffmpeg:fetch prints its folder)")
  endif()
  target_include_directories(im_engine SYSTEM PRIVATE "${IM_FFMPEG_ROOT}/include")
  target_compile_definitions(im_engine PRIVATE IM_HAVE_FFMPEG_HEADERS=1)
endif()

# Shaders: shaders/<name>_vs.hlsl, _ps.hlsl and _cs.hlsl, compiled by the Windows SDK's fxc (no run-time compiler,
# no d3dcompiler_47.dll dependency, 04 §13.1).
file(GLOB IM_SHADERS CONFIGURE_DEPENDS "${CMAKE_CURRENT_SOURCE_DIR}/shaders/*.hlsl")
if(IM_SHADERS)
  find_program(IM_FXC fxc REQUIRED)
  set(_shader_dir "${CMAKE_CURRENT_BINARY_DIR}/shaders")
  file(MAKE_DIRECTORY "${_shader_dir}")
  set(_shader_headers "")
  foreach(_hlsl IN LISTS IM_SHADERS)
    get_filename_component(_name "${_hlsl}" NAME_WE)
    if(_name MATCHES "_vs$")
      set(_profile vs_5_0)
    elseif(_name MATCHES "_ps$")
      set(_profile ps_5_0)
    elseif(_name MATCHES "_cs$")
      set(_profile cs_5_0)
    else()
      message(FATAL_ERROR "${_hlsl}: a shader's file name ends in _vs, _ps or _cs (its profile)")
    endif()
    add_custom_command(
      OUTPUT "${_shader_dir}/${_name}.h"
      COMMAND "${IM_FXC}" /nologo /WX /O3 /T ${_profile} /E main /Vn g_${_name} /Fh "${_shader_dir}/${_name}.h"
              "${_hlsl}"
      DEPENDS "${_hlsl}"
      COMMENT "fxc ${_name}.hlsl"
      VERBATIM)
    list(APPEND _shader_headers "${_shader_dir}/${_name}.h")
  endforeach()
  add_custom_target(im_shaders DEPENDS ${_shader_headers})
  add_dependencies(im_engine im_shaders)
  target_include_directories(im_engine PRIVATE "${CMAKE_CURRENT_BINARY_DIR}")
endif()

# ------------------------------------------------------------------------------------------------ isshoni-media.dll
# The version resource is compiled in a target of its own: rc.exe takes none of the C++ compiler's options, which
# im_build_flags would otherwise pass to it.
string(REPLACE "." "," IM_WINVER_COMMAS "${IM_WINVER}")
configure_file("${CMAKE_CURRENT_SOURCE_DIR}/resources/version.rc.in" "${CMAKE_CURRENT_BINARY_DIR}/version.rc" @ONLY)
add_library(im_version_resource OBJECT "${CMAKE_CURRENT_BINARY_DIR}/version.rc")

add_library(isshoni-media SHARED "${CMAKE_CURRENT_SOURCE_DIR}/isshoni-media.def")
target_link_libraries(isshoni-media PRIVATE im_engine im_version_resource)
set_target_properties(isshoni-media PROPERTIES LINKER_LANGUAGE CXX)
# CMake's RelWithDebInfo links /INCREMENTAL, which /OPT:REF (im_build_flags) turns off with a warning.
target_link_options(isshoni-media PRIVATE "$<$<NOT:$<CONFIG:Debug>>:/INCREMENTAL:NO>")

# ------------------------------------------------------------------------------------------------------------ tests
if(IM_BUILD_TESTS)
  # dll_search_test's DLLs (04 §18.2): a marker that writes "<its path>.loaded" when it loads, planted by the test in
  # its child's current directory and PATH, and two probes that delay-load it with the default delay-load helper,
  # without and with /DEPENDENTLOADFLAG:0x800. They live in a folder of their own: next to the test executable, the
  # application folder would find them before the current directory does.
  set(IM_TEST_DLL_DIR "${CMAKE_CURRENT_BINARY_DIR}/dlltest")
  add_library(im-test-missing SHARED "${CMAKE_CURRENT_SOURCE_DIR}/tests/support/marker_dll.cpp")
  set_target_properties(im-test-missing PROPERTIES RUNTIME_OUTPUT_DIRECTORY "${IM_TEST_DLL_DIR}")
  target_link_libraries(im-test-missing PRIVATE im_build_flags)
  foreach(_probe plain flag)
    add_library(im-test-dlprobe-${_probe} SHARED "${CMAKE_CURRENT_SOURCE_DIR}/tests/support/dlprobe.cpp")
    set_target_properties(im-test-dlprobe-${_probe} PROPERTIES RUNTIME_OUTPUT_DIRECTORY "${IM_TEST_DLL_DIR}")
    target_link_libraries(im-test-dlprobe-${_probe} PRIVATE im_build_flags im-test-missing delayimp)
    target_link_options(im-test-dlprobe-${_probe} PRIVATE /DELAYLOAD:im-test-missing.dll)
  endforeach()
  target_link_options(im-test-dlprobe-flag PRIVATE /DEPENDENTLOADFLAG:0x800)

  file(GLOB _tests CONFIGURE_DEPENDS "${CMAKE_CURRENT_SOURCE_DIR}/tests/*_test.cpp")
  foreach(_src IN LISTS _tests)
    get_filename_component(_name "${_src}" NAME_WE)
    if(TARGET ${_name})
      message(FATAL_ERROR "${_src}: a target named ${_name} already exists; test file names must be unique across "
                          "native/common and native/windows")
    endif()
    add_executable(${_name} "${_src}")
    target_link_libraries(${_name} PRIVATE im_engine GTest::gmock_main)
    target_compile_definitions(
      ${_name}
      PRIVATE "IM_NATIVE_DIR=\"${IM_NATIVE_DIR}\"" "IM_TESTDATA_DIR=\"${CMAKE_CURRENT_SOURCE_DIR}/tests/testdata\""
              "IM_TEST_DLL_DIR=\"${IM_TEST_DLL_DIR}\"" "IM_TEST_OUT_DIR=\"${CMAKE_CURRENT_BINARY_DIR}\"")
    add_test(NAME ${_name} COMMAND ${_name})
  endforeach()

  if(TARGET dll_search_test)
    # The engine's delay-load hook in a test binary (04 §18.2): an extra delay-loaded DLL that no system folder holds,
    # and mfplat.dll, which the hook must still load from System32.
    target_link_libraries(dll_search_test PRIVATE im-test-missing mfplat)
    target_link_options(dll_search_test PRIVATE /DELAYLOAD:im-test-missing.dll)
    add_dependencies(dll_search_test im-test-dlprobe-plain im-test-dlprobe-flag)
  endif()
endif()
