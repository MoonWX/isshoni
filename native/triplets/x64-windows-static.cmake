# vcpkg's own x64-windows-static (static libraries, static CRT), overlaid through native/vcpkg-configuration.json to
# add one thing: the MSVC toolset of deploy/windows/tools.json ("msvc".toolset), which deploy/windows/msvc-env.ps1
# sets up for the project's own build. Without it vcpkg builds the ports with the newest toolset installed (14.51 on
# windows-2025), whose STL objects call helpers such as __std_rotate that 14.44's static libraries lack, so linking
# them into the 14.44 build fails. `task lint:pins` checks that the version below equals tools.json's.
set(VCPKG_TARGET_ARCHITECTURE x64)
set(VCPKG_CRT_LINKAGE static)
set(VCPKG_LIBRARY_LINKAGE static)
set(VCPKG_PLATFORM_TOOLSET v143)
set(VCPKG_PLATFORM_TOOLSET_VERSION 14.44)
