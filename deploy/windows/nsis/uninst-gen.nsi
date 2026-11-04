; uninst-gen.nsi: the prebuilt-uninstaller generator (docs/m2/06-windows-release.md section 6.9). CI compiles it with
; tools/desktoppkg nsis -uninstaller-generator (VERSION, WINVER, OUTFILE) and runs it on the Windows runner with /S:
; it writes uninstall.exe next to itself and quits, and installer.nsi then packs that file (PREBUILT_UNINSTALLER)
; instead of calling WriteUninstaller. Nothing happens between the two steps in 0.x; after SignPath's approval, a
; signing request for uninstall.exe slots in there (06 section 12.4). Exit codes: 0 when uninstall.exe was written,
; 6 when it couldn't be, 2 without /S (it then writes nothing).

Unicode true
SetCompressor /SOLID lzma
RequestExecutionLevel user
ManifestSupportedOS all
ManifestDPIAware true

!define ISSHONI_UNINSTALLER_ONLY
!include common.nsh

Name "${PRODUCT}"
OutFile "${OUTFILE}"
InstallDir "$LOCALAPPDATA\Programs\isshoni"
BrandingText "$(Branding)"
ShowUninstDetails show

!insertmacro IsshoniUninstallPages
!insertmacro MUI_LANGUAGE "English"
!include /CHARSET=UTF8 lang\English.nsh

!insertmacro IsshoniVersionInfo "isshoni uninstaller"

Function .onInit
  ${IfNot} ${Silent}
    SetErrorLevel ${EXIT_OTHER}
    Quit
  ${EndIf}
  ClearErrors
  WriteUninstaller "$EXEDIR\uninstall.exe"
  ; A Quit from .onInit exits 2 ("aborted by script") unless an error level was set: 0 is set explicitly.
  ${If} ${Errors}
    SetErrorLevel ${EXIT_WRITE}
  ${Else}
    SetErrorLevel 0
  ${EndIf}
  Quit
FunctionEnd

Section "-generator"
SectionEnd

!include uninstall.nsh
