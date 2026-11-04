; common.nsh: the defines, registry paths and macros that installer.nsi, uninst-gen.nsi and uninstall.nsh share
; (docs/m2/06-windows-release.md, "06" below: sections 2.2-2.4, 6). Built only through tools/desktoppkg nsis, which passes
; VERSION, WINVER, STAGE, OUTFILE, WEBVIEW2_SETUP and, in CI, PREBUILT_UNINSTALLER (06 section 6.2), and runs makensis -WX.
;
; Every user-visible string is a LangString of lang\English.nsh (06 section 6.10; `desktoppkg check` enforces it). Only
; built-in plugins (System, nsDialogs, Banner, nsExec) and headers are used.

!ifndef ISSHONI_COMMON_NSH
!define ISSHONI_COMMON_NSH

!ifndef VERSION
  !error "VERSION is not defined: build the installer with tools/desktoppkg nsis"
!endif
!ifndef WINVER
  !error "WINVER is not defined: build the installer with tools/desktoppkg nsis"
!endif
!ifndef OUTFILE
  !error "OUTFILE is not defined: build the installer with tools/desktoppkg nsis"
!endif

!include LogicLib.nsh
!include FileFunc.nsh
!include WordFunc.nsh
!include WinVer.nsh
!include x64.nsh
!include MUI2.nsh
!include nsDialogs.nsh

; ---- names (06 section 2.2-2.4) ----
!define PRODUCT "isshoni"
!define PUBLISHER "isshoni project"
!define COPYRIGHT "Copyright the isshoni authors. Apache-2.0."
!define AUMID "io.isshoni.desktop"
!define SITE "https://moonwx.github.io/isshoni"
!define APP_EXE "isshoni.exe"
!define FILE_LIST "install-files.txt"

; ---- registry, all under HKCU (06 section 2.3); HKLM is only ever read ----
!define REG_APP "Software\isshoni"
!define REG_UNINST "Software\Microsoft\Windows\CurrentVersion\Uninstall\isshoni"
!define REG_PROTO "Software\Classes\isshoni"
!define REG_RUN "Software\Microsoft\Windows\CurrentVersion\Run"
!define REG_AUMID "Software\Classes\AppUserModelId\${AUMID}"
!define REG_CLSID "Software\Classes\CLSID"

; ---- the process contract (06 section 2.4) ----
!define MUTEX_RUNNING "Local\io.isshoni.desktop.running"
!define MUTEX_INSTALLING "Local\io.isshoni.desktop.installing"
!define SYNCHRONIZE 0x00100000
!define ERROR_ALREADY_EXISTS 183

; ---- WebView2 (06 section 6.5) ----
; WEBVIEW2_MIN must equal the runtime minimum of docs/m2/03-desktop-app.md section 5.4 (Evergreen 120.0; older: the app
; shows a message box and exits 1), so that a stale runtime never installs cleanly and then refuses to start.
!define WEBVIEW2_MIN "120.0.0.0"
!define WEBVIEW2_GUID "{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}"
!define WEBVIEW2_TIMEOUT_MS 900000
!define WEBVIEW2_PAGE "https://developer.microsoft.com/microsoft-edge/webview2/"

; ---- installer exit codes (06 section 6.4) ----
!define EXIT_OTHER 2
!define EXIT_RUNNING 3
!define EXIT_NEWER 4
!define EXIT_UNSUPPORTED 5
!define EXIT_WRITE 6
!define EXIT_WEBVIEW2 7
!define EXIT_ANOTHER_SETUP 8

; ---- waits (06 section 6.7, section 6.8) ----
!define WAIT_POLL_MS 500
!define WAIT_POLLS 120 ; 60 s
!define RENAME_POLL_MS 250
!define RENAME_POLLS 40 ; 10 s
!define FORGET_TIMEOUT_MS 30000

!ifndef ISSHONI_UNINSTALLER_ONLY
  ; The installer's handle of MUTEX_INSTALLING (06 section 6.7), closed only right before it starts the app.
  Var InstallingMutex
!endif

; MutexExists NAME OUT: OUT is 1 while the named mutex exists (OpenMutexW(SYNCHRONIZE, FALSE, NAME) succeeds), else 0.
!macro MutexExists NAME OUT
  System::Call 'kernel32::OpenMutexW(i ${SYNCHRONIZE}, i 0, w "${NAME}") p .s'
  Pop ${OUT}
  ${If} ${OUT} P<> 0
    System::Call 'kernel32::CloseHandle(p ${OUT})'
    StrCpy ${OUT} 1
  ${Else}
    StrCpy ${OUT} 0
  ${EndIf}
!macroend

; IsshoniVersionInfo DESCRIPTION: the version resource of 06 section 6.2 (VIProductVersion and the strings).
!macro IsshoniVersionInfo DESCRIPTION
  VIProductVersion "${WINVER}"
  VIFileVersion "${WINVER}"
  VIAddVersionKey /LANG=${LANG_ENGLISH} "ProductName" "${PRODUCT}"
  VIAddVersionKey /LANG=${LANG_ENGLISH} "ProductVersion" "${VERSION}"
  VIAddVersionKey /LANG=${LANG_ENGLISH} "FileVersion" "${WINVER}"
  VIAddVersionKey /LANG=${LANG_ENGLISH} "CompanyName" "${PUBLISHER}"
  VIAddVersionKey /LANG=${LANG_ENGLISH} "LegalCopyright" "${COPYRIGHT}"
  VIAddVersionKey /LANG=${LANG_ENGLISH} "FileDescription" "${DESCRIPTION}"
!macroend

; IsshoniUninstallPages: the uninstaller's pages (06 section 6.3), a confirm page with the purge checkbox and the progress
; page; inserted before MUI_LANGUAGE by each script that includes uninstall.nsh.
!macro IsshoniUninstallPages
  UninstPage custom un.ConfirmPage un.ConfirmLeave
  !insertmacro MUI_UNPAGE_INSTFILES
!macroend

; The string helpers below exist once for the installer and once for the uninstaller (PREFIX "" or "un."), each
; inserted only by the script that calls it, so -WX sees no unreferenced function.

; TrimLineFunction: Pop a line, push it without its trailing CR and LF.
!macro TrimLineFunction PREFIX
  Function ${PREFIX}TrimLine
    Exch $0
    Push $1
    ${Do}
      StrCpy $1 $0 1 -1
      ${If} $1 == "$\r"
      ${OrIf} $1 == "$\n"
        StrCpy $0 $0 -1
      ${Else}
        ${ExitDo}
      ${EndIf}
    ${Loop}
    Pop $1
    Exch $0
  FunctionEnd
!macroend

; SafeRelFunction: Pop a path from install-files.txt, push 1 when it is a plain relative path under $INSTDIR: not
; empty, no "..", no drive letter or other ":", no leading "\" or "/" (06 section 6.6). Push 0 otherwise.
!macro SafeRelFunction PREFIX
  Function ${PREFIX}SafeRel
    Exch $0
    Push $1
    Push $2
    Push $3
    StrCpy $3 1
    StrCpy $1 $0 1
    ${If} $0 == ""
    ${OrIf} $1 == "\"
    ${OrIf} $1 == "/"
      StrCpy $3 0
    ${EndIf}
    StrCpy $2 0
    ${DoWhile} $3 = 1
      StrCpy $1 $0 1 $2
      ${If} $1 == ""
        ${ExitDo}
      ${EndIf}
      ${If} $1 == ":"
        StrCpy $3 0
      ${EndIf}
      StrCpy $1 $0 2 $2
      ${If} $1 == ".."
        StrCpy $3 0
      ${EndIf}
      IntOp $2 $2 + 1
    ${Loop}
    StrCpy $0 $3
    Pop $3
    Pop $2
    Pop $1
    Exch $0
  FunctionEnd
!macroend

; PointsIntoInstDirFunction: Pop a command or path as the registry holds it ("C:\x\isshoni.exe" -Embedding, or
; unquoted), push 1 when the program it names is inside $INSTDIR (06 section 6.8), else 0.
!macro PointsIntoInstDirFunction PREFIX
  Function ${PREFIX}PointsIntoInstDir
    Exch $0
    Push $1
    Push $2
    StrCpy $1 $0 1
    ${If} $1 == '"'
      StrCpy $0 $0 "" 1
    ${EndIf}
    StrLen $2 "$INSTDIR\"
    StrCpy $1 $0 $2
    ${If} $INSTDIR != ""
    ${AndIf} $1 == "$INSTDIR\"
      StrCpy $0 1
    ${Else}
      StrCpy $0 0
    ${EndIf}
    Pop $2
    Pop $1
    Exch $0
  FunctionEnd
!macroend

!endif ; ISSHONI_COMMON_NSH
