# native/common: the portable C++20 parts of the engine (docs/m2/04-windows-engine.md §1, §4) and their tests
# (§18.1). No OS header outside include/im/os/.
#
# Every library here has the same layout, so a slice that adds code adds files only (docs/m2/README.md §3 item 14):
#   include/im/<area>/*.h    public headers
#   src/**/*.cpp             sources (none: a header-only INTERFACE library)
#   tests/*_test.cpp         one GoogleTest executable per file, registered with ctest under the file's name
#   tests/*_test.c           the same for C tests (no GoogleTest)
#   tests/testdata/          read through the IM_TESTDATA_DIR define
#   tests/fuzz/*_fuzz.cpp    libFuzzer targets (IM_BUILD_FUZZ, clang), seeds in tests/fuzz/corpus/<target>/
# A test or fuzz target is named after its file, so test and fuzz file names must be unique across common/ and its
# areas (configure stops on a clash): an area's file carries a prefix where a name could repeat (audiomix/tests/
# mix_ring_test.cpp beside common's ring_test.cpp). Sources are compiled on every OS, so OS-specific code (the os/
# shim's src/os/*.cpp) is guarded with #if (_WIN32, __linux__, __APPLE__); the system libraries it needs are linked
# below.
# im_common is this folder. im_audio (audio/: PcmSink, the Opus stage, the PCM tap, the test tone; 04 §10.3),
# im_audiopolicy (audiopolicy/, 05) and im_audiomix (audiomix/, 05) are added below when their folder exists.

# im_check_target_name(<name> <file>): stops with a clear message when another library already has a target <name>.
function(im_check_target_name name file)
  if(TARGET ${name})
    message(FATAL_ERROR "${file}: a target named ${name} already exists; test and fuzz file names must be unique "
                        "across native/common and its areas (rename the file, e.g. with the area as a prefix)")
  endif()
endfunction()

# im_add_tests(<library> <dir>): the tests and fuzz targets of the library whose folder is <dir>.
function(im_add_tests lib dir)
  if(IM_BUILD_TESTS)
    file(GLOB _tests CONFIGURE_DEPENDS "${dir}/tests/*_test.cpp" "${dir}/tests/*_test.c")
    foreach(_src IN LISTS _tests)
      get_filename_component(_name "${_src}" NAME_WE)
      im_check_target_name(${_name} "${_src}")
      add_executable(${_name} "${_src}")
      target_link_libraries(${_name} PRIVATE ${lib})
      if(_src MATCHES "\\.cpp$")
        target_link_libraries(${_name} PRIVATE GTest::gmock_main)
      endif()
      target_compile_definitions(${_name} PRIVATE IM_TESTDATA_DIR="${dir}/tests/testdata")
      add_test(NAME ${_name} COMMAND ${_name})
    endforeach()
  endif()
  if(IM_BUILD_FUZZ)
    file(GLOB _fuzzers CONFIGURE_DEPENDS "${dir}/tests/fuzz/*_fuzz.cpp")
    foreach(_src IN LISTS _fuzzers)
      get_filename_component(_name "${_src}" NAME_WE)
      im_check_target_name(${_name} "${_src}")
      add_executable(${_name} "${_src}")
      target_link_libraries(${_name} PRIVATE ${lib})
      target_link_options(${_name} PRIVATE -fsanitize=fuzzer)
      # ctest runs every seed once (libFuzzer runs file arguments and exits), or 10000 generated inputs without
      # seeds; the long runs (60 s per target) are the nightly's.
      file(GLOB _seeds CONFIGURE_DEPENDS "${dir}/tests/fuzz/corpus/${_name}/*")
      if(_seeds)
        add_test(NAME ${_name} COMMAND ${_name} ${_seeds})
      else()
        add_test(NAME ${_name} COMMAND ${_name} -runs=10000)
      endif()
    endforeach()
  endif()
endfunction()

# ------------------------------------------------------------------------------------------------------ im_common
file(GLOB_RECURSE IM_COMMON_SOURCES CONFIGURE_DEPENDS "${CMAKE_CURRENT_SOURCE_DIR}/src/*.cpp")
add_library(im_common STATIC ${IM_COMMON_SOURCES})
target_include_directories(im_common PUBLIC "${CMAKE_CURRENT_SOURCE_DIR}/include")
target_link_libraries(im_common PUBLIC im_abi im_build_flags)
if(WIN32)
  # The os/ shim's wake-up (WaitOnAddress, WakeByAddressSingle; 04 §6.2) for the DLL and common's own tests.
  target_link_libraries(im_common PUBLIC synchronization)
endif()
im_add_tests(im_common "${CMAKE_CURRENT_SOURCE_DIR}")

# ----------------------------------------------------------------------------------------- the areas of 04 and 05
# im_add_area(<folder> [dependencies...]): library im_<folder> from <folder>/, linked with im_common and the
# dependencies, plus its tests. Nothing happens while the folder doesn't exist.
function(im_add_area area)
  set(_dir "${CMAKE_CURRENT_SOURCE_DIR}/${area}")
  file(GLOB_RECURSE _srcs CONFIGURE_DEPENDS "${_dir}/src/*.cpp")
  set(_lib im_${area})
  if(_srcs)
    add_library(${_lib} STATIC ${_srcs})
    target_include_directories(${_lib} PUBLIC "${_dir}/include")
    target_link_libraries(${_lib} PUBLIC im_common ${ARGN})
  else()
    add_library(${_lib} INTERFACE)
    target_include_directories(${_lib} INTERFACE "${_dir}/include")
    target_link_libraries(${_lib} INTERFACE im_common ${ARGN})
  endif()
  im_add_tests(${_lib} "${_dir}")
endfunction()

if(EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/audio")
  find_package(Opus CONFIG REQUIRED) # vcpkg port opus (BSD-3-Clause)
  im_add_area(audio Opus::opus)
endif()

if(EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/audiopolicy")
  im_add_area(audiopolicy)
endif()

if(EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/audiomix")
  # vcpkg port speexdsp (BSD-3-Clause) installs no CMake package: find its header and library.
  find_path(IM_SPEEXDSP_INCLUDE_DIR speex/speex_resampler.h REQUIRED)
  find_library(IM_SPEEXDSP_LIBRARY NAMES speexdsp libspeexdsp REQUIRED)
  add_library(im_speexdsp INTERFACE)
  target_include_directories(im_speexdsp SYSTEM INTERFACE "${IM_SPEEXDSP_INCLUDE_DIR}")
  target_link_libraries(im_speexdsp INTERFACE "${IM_SPEEXDSP_LIBRARY}")
  im_add_area(audiomix im_speexdsp)
endif()
