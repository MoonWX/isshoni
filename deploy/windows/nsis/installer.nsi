; installer.nsi: the isshoni Windows installer (docs/m2/06-windows-release.md, "06" below, section 6), per user, MUI2.
;
; Build it with tools/desktoppkg nsis (makensis -WX): it passes VERSION, WINVER (06 section 5.3), STAGE (the tree that
; desktoppkg stage wrote, 06 section 2.2), OUTFILE, WEBVIEW2_SETUP and, in CI, PREBUILT_UNINSTALLER (06 section 6.9). Without
; PREBUILT_UNINSTALLER (the Mac, Homebrew's makensis) the uninstaller is written with WriteUninstaller.
;
; Command line (06 section 6.4): /S, /UPDATE, /RELAUNCH, /LAUNCH, /SERVER=<url>, /DESKTOPSHORTCUT=0|1, /NOUPDATECHECKS,
; /ALLOWDOWNGRADE, /D=<dir> (last, fresh installs only). Exit codes: 0 installed, 1 cancelled, 2 other error,
; 3 isshoni still running, 4 a newer version is installed, 5 unsupported Windows, 6 a file could not be written,
; 7 no usable WebView2 runtime, 8 another isshoni setup is running.
;
; In every mode the first statement of .onInit creates Local\io.isshoni.desktop.installing (06 section 2.4, section 6.7), held
; until right before the installer starts the app, its last action. The /UPDATE order: the installing mutex, the
; banner, the wait for the running mutex (60 s), the install, closing the installing mutex, the relaunch.

Unicode true
SetCompressor /SOLID lzma
RequestExecutionLevel user
ManifestSupportedOS all
ManifestDPIAware true

!ifndef STAGE
  !error "STAGE is not defined: build the installer with tools/desktoppkg nsis"
!endif
!ifndef WEBVIEW2_SETUP
  !error "WEBVIEW2_SETUP is not defined: build the installer with tools/desktoppkg nsis"
!endif

!include common.nsh
!include Win\COM.nsh
!include Win\Propkey.nsh

Name "${PRODUCT}"
OutFile "${OUTFILE}"
InstallDir "$LOCALAPPDATA\Programs\isshoni"
BrandingText "$(Branding)"
ShowInstDetails show
!ifndef PREBUILT_UNINSTALLER
  ShowUninstDetails show
!endif

; ---- state ----
Var UpdateMode       ; /UPDATE
Var Relaunch         ; /RELAUNCH
Var Launch           ; /LAUNCH
Var AllowDowngrade   ; /ALLOWDOWNGRADE
Var NoUpdateChecks   ; /NOUPDATECHECKS
Var ShortcutArg      ; /DESKTOPSHORTCUT=0|1, "" when not given
Var ServerArg        ; /SERVER=<url>, "" when not given
Var PendingServer    ; ServerArg when it passed the filter
Var ServerIgnored    ; 1 when /SERVER= was given and ignored
Var BannerShown
Var Upgrade          ; 1 when an install exists (HKCU\Software\isshoni InstallDir)
Var OldVersion
Var DesktopShortcut  ; the choices that end in the registry (06 section 2.3)
Var UpdateChecks
Var OldBuild         ; Windows build 19041 to 19044
Var Arm64
Var LaunchArgs
Var Dialog
Var ChkDesktop
Var ChkUpdates
Var Fh               ; file handles and lines of the list passes
Var Line
Var Fh2
Var Line2
Var Found
Var Tries

; ---- pages (06 section 6.3); in /UPDATE mode only the progress page shows (and only without /S) ----
!define MUI_PAGE_CUSTOMFUNCTION_PRE SkipInUpdate
!define MUI_PAGE_CUSTOMFUNCTION_SHOW WelcomeShow
!define MUI_WELCOMEPAGE_TITLE "$(WelcomeTitle)"
!define MUI_WELCOMEPAGE_TEXT "$(WelcomeText)"
!insertmacro MUI_PAGE_WELCOME
Page custom CompatPage
Page custom OptionsPage OptionsLeave
!define MUI_PAGE_CUSTOMFUNCTION_SHOW InstFilesShow
!insertmacro MUI_PAGE_INSTFILES
!define MUI_PAGE_CUSTOMFUNCTION_PRE SkipInUpdate
!define MUI_FINISHPAGE_RUN
!define MUI_FINISHPAGE_RUN_TEXT "$(FinishRun)"
!define MUI_FINISHPAGE_RUN_FUNCTION FinishRun
!define MUI_FINISHPAGE_TEXT "$(FinishInstalled)$\r$\n$\r$\n$(FinishNext)"
!insertmacro MUI_PAGE_FINISH
!ifndef PREBUILT_UNINSTALLER
  !insertmacro IsshoniUninstallPages
!endif

!insertmacro MUI_LANGUAGE "English"
!include /CHARSET=UTF8 lang\English.nsh

!insertmacro IsshoniVersionInfo "isshoni setup"

!insertmacro TrimLineFunction ""
!insertmacro SafeRelFunction ""

; ---- .onInit ----

Function .onInit
  ; The installing mutex, before anything else (06 section 6.7). ?e pushes the last error.
  System::Call 'kernel32::CreateMutexW(p 0, i 0, w "${MUTEX_INSTALLING}") p .r0 ?e'
  Pop $1
  StrCpy $InstallingMutex $0
  ${If} $1 = ${ERROR_ALREADY_EXISTS}
    System::Call 'kernel32::CloseHandle(p $InstallingMutex)'
    StrCpy $InstallingMutex 0
    MessageBox MB_OK|MB_ICONEXCLAMATION "$(AnotherSetupRunning)" /SD IDOK
    SetErrorLevel ${EXIT_ANOTHER_SETUP}
    Quit
  ${EndIf}

  SetShellVarContext current
  SetRegView 64
  InitPluginsDir
  Call ParseFlags
  ${If} $UpdateMode = 1
  ${AndIf} ${Silent}
    Banner::show "$(UpdatingBanner)"
    StrCpy $BannerShown 1
  ${EndIf}

  ; 06 section 6.5: 64-bit Windows 10 2004 (build 19041) or later; 19041-19044 and ARM64 get a warning page.
  ${IfNot} ${RunningX64}
  ${OrIfNot} ${AtLeastBuild} 19041
    StrCpy $R9 ${EXIT_UNSUPPORTED}
    StrCpy $R8 "$(UnsupportedWindows)"
    Call Fail
  ${EndIf}
  ${If} ${AtMostBuild} 19044
    StrCpy $OldBuild 1
  ${EndIf}
  ${If} ${IsNativeARM64}
    StrCpy $Arm64 1
  ${EndIf}

  ; 06 section 6.6: an existing install decides the folder (/D= is for fresh installs only) and the version compare.
  ReadRegStr $R0 HKCU "${REG_APP}" "InstallDir"
  ${If} $R0 == ""
    ReadRegStr $R0 HKCU "${REG_UNINST}" "InstallLocation"
  ${EndIf}
  ${If} $R0 != ""
    StrCpy $INSTDIR $R0
    StrCpy $Upgrade 1
    ReadRegStr $OldVersion HKCU "${REG_APP}" "Version"
    ReadRegStr $R1 HKCU "${REG_APP}" "WinVer"
    ${If} $R1 != ""
      ${VersionCompare} $R1 "${WINVER}" $R2
      ${If} $R2 = 1
      ${AndIf} $AllowDowngrade <> 1
        StrCpy $R9 ${EXIT_NEWER}
        StrCpy $R8 "$(NewerInstalled)"
        Call Fail
      ${EndIf}
    ${EndIf}
  ${EndIf}

  ; The choices (06 section 2.3, section 6.4): an upgrade keeps the stored ones; a fresh silent install takes /DESKTOPSHORTCUT
  ; (default 0); a fresh interactive one starts with both boxes checked.
  StrCpy $DesktopShortcut 1
  ${If} ${Silent}
    StrCpy $DesktopShortcut 0
  ${EndIf}
  ${If} $ShortcutArg != ""
    StrCpy $DesktopShortcut $ShortcutArg
  ${EndIf}
  StrCpy $UpdateChecks 1
  ${If} $Upgrade = 1
    ClearErrors
    ReadRegDWORD $R0 HKCU "${REG_APP}" "DesktopShortcut"
    ${IfNot} ${Errors}
      StrCpy $DesktopShortcut $R0
    ${EndIf}
    ClearErrors
    ReadRegDWORD $R0 HKCU "${REG_APP}" "UpdateChecks"
    ${IfNot} ${Errors}
      StrCpy $UpdateChecks $R0
    ${EndIf}
  ${EndIf}
  ${If} $NoUpdateChecks = 1
    StrCpy $UpdateChecks 0
  ${EndIf}
  ${If} $DesktopShortcut <> 0
    StrCpy $DesktopShortcut 1
  ${EndIf}
  ${If} $UpdateChecks <> 0
    StrCpy $UpdateChecks 1
  ${EndIf}

  Call FilterServer
FunctionEnd

; ParseFlags reads the command line into the flag variables. It splits it at spaces outside double quotes itself:
; /SERVER=https://host/... holds slashes, which FileFunc's GetOptions would take for the next option.
Function ParseFlags
  StrCpy $UpdateMode 0
  StrCpy $Relaunch 0
  StrCpy $Launch 0
  StrCpy $AllowDowngrade 0
  StrCpy $NoUpdateChecks 0
  StrCpy $ShortcutArg ""
  StrCpy $ServerArg ""
  ${GetParameters} $R0
  StrLen $R2 $R0
  StrCpy $R1 0
  ${Do}
    ; skip spaces
    ${Do}
      StrCpy $R3 $R0 1 $R1
      ${If} $R3 != " "
        ${ExitDo}
      ${EndIf}
      IntOp $R1 $R1 + 1
    ${Loop}
    ${If} $R1 >= $R2
      ${ExitDo}
    ${EndIf}
    ; one token
    StrCpy $R4 ""
    StrCpy $R5 0
    ${Do}
      ${If} $R1 >= $R2
        ${ExitDo}
      ${EndIf}
      StrCpy $R3 $R0 1 $R1
      IntOp $R1 $R1 + 1
      ${If} $R3 == '"'
        IntOp $R5 1 - $R5
        ${Continue}
      ${EndIf}
      ${If} $R3 == " "
      ${AndIf} $R5 = 0
        ${ExitDo}
      ${EndIf}
      StrCpy $R4 "$R4$R3"
    ${Loop}
    ; the flags are case-insensitive (StrCmp); values keep their case
    StrCpy $R6 $R4 8
    StrCpy $R7 $R4 17
    ${If} $R4 == "/UPDATE"
      StrCpy $UpdateMode 1
    ${ElseIf} $R4 == "/RELAUNCH"
      StrCpy $Relaunch 1
    ${ElseIf} $R4 == "/LAUNCH"
      StrCpy $Launch 1
    ${ElseIf} $R4 == "/ALLOWDOWNGRADE"
      StrCpy $AllowDowngrade 1
    ${ElseIf} $R4 == "/NOUPDATECHECKS"
      StrCpy $NoUpdateChecks 1
    ${ElseIf} $R6 == "/SERVER="
      StrCpy $ServerArg $R4 "" 8
      StrCpy $ServerIgnored 1 ; until FilterServer accepts it
    ${ElseIf} $R7 == "/DESKTOPSHORTCUT="
      StrCpy $R7 $R4 "" 17
      ${If} $R7 == "0"
      ${OrIf} $R7 == "1"
        StrCpy $ShortcutArg $R7
      ${EndIf}
    ${EndIf}
  ${Loop}
FunctionEnd

; FilterServer: /SERVER= becomes PendingServer only when it is https://..., or http:// on loopback (127.0.0.1,
; localhost, [::1]), at most 2048 bytes and taken from a command line NSIS didn't cut at its string limit; the app
; validates it again (06 section 2.3, section 6.4). Anything else is ignored, with a line in the log.
Function FilterServer
  StrCpy $PendingServer ""
  ${If} $ServerArg == ""
    Return
  ${EndIf}
  StrLen $R0 $ServerArg
  StrLen $R1 $CMDLINE
  IntOp $R2 ${NSIS_MAX_STRLEN} - 1
  ${If} $R0 > 2048
  ${OrIf} $R1 >= $R2
    Return
  ${EndIf}
  StrCpy $R3 $ServerArg 8
  ${If} $R3 == "https://"
  ${AndIf} $R0 > 8
    StrCpy $PendingServer $ServerArg
  ${Else}
    ${ForEach} $R4 0 2 + 1
      ${If} $R4 = 0
        StrCpy $R5 "http://127.0.0.1"
      ${ElseIf} $R4 = 1
        StrCpy $R5 "http://localhost"
      ${Else}
        StrCpy $R5 "http://[::1]"
      ${EndIf}
      StrLen $R6 $R5
      StrCpy $R3 $ServerArg $R6
      StrCpy $R7 $ServerArg 1 $R6
      ${If} $R3 == $R5
        ${If} $R7 == ""
        ${OrIf} $R7 == ":"
        ${OrIf} $R7 == "/"
          StrCpy $PendingServer $ServerArg
        ${EndIf}
      ${EndIf}
    ${Next}
  ${EndIf}
  ${If} $PendingServer != ""
    StrCpy $ServerIgnored 0
  ${EndIf}
FunctionEnd

; Fail: exit with code $R9 after the message $R8 (interactive only); the banner goes first.
Function Fail
  Call BannerOff
  MessageBox MB_OK|MB_ICONSTOP "$R8" /SD IDOK
  SetErrorLevel $R9
  Quit
FunctionEnd

Function BannerOff
  ${If} $BannerShown = 1
    Banner::destroy
    StrCpy $BannerShown 0
  ${EndIf}
FunctionEnd

; ---- pages ----

Function SkipInUpdate
  ${If} $UpdateMode = 1
    Abort
  ${EndIf}
FunctionEnd

; InstFilesShow: an /UPDATE run without /S (06 section 6.4's fallback) shows only this page, without Cancel.
Function InstFilesShow
  ${If} $UpdateMode = 1
    GetDlgItem $R0 $HWNDPARENT 2
    EnableWindow $R0 0
  ${EndIf}
FunctionEnd

Function WelcomeShow
  ${NSD_CreateLink} 120u 175u 195u 10u "$(WelcomeLink)"
  Pop $R0
  SetCtlColors $R0 "" "${MUI_BGCOLOR}"
  ${NSD_OnClick} $R0 OpenDownloadPage
FunctionEnd

Function OpenDownloadPage
  Pop $R0
  ExecShell "open" "${SITE}/download#windows"
FunctionEnd

; CompatPage: shown only on Windows builds 19041-19044 and on ARM64 (06 section 6.5); the install goes on either way.
Function CompatPage
  ${If} $UpdateMode = 1
    Abort
  ${EndIf}
  ${If} $OldBuild <> 1
  ${AndIf} $Arm64 <> 1
    Abort
  ${EndIf}
  !insertmacro MUI_HEADER_TEXT "$(CompatTitle)" "$(CompatSubtitle)"
  nsDialogs::Create 1018
  Pop $Dialog
  ${If} $Dialog == error
    Abort
  ${EndIf}
  StrCpy $R1 "0u"
  ${If} $OldBuild = 1
    ${NSD_CreateLabel} 0 0 100% 24u "$(OldBuildWarning)"
    Pop $R0
    StrCpy $R1 "30u"
  ${EndIf}
  ${If} $Arm64 = 1
    ${NSD_CreateLabel} 0 $R1 100% 24u "$(Arm64Warning)"
    Pop $R0
  ${EndIf}
  nsDialogs::Show
FunctionEnd

Function OptionsPage
  ${If} $UpdateMode = 1
    Abort
  ${EndIf}
  !insertmacro MUI_HEADER_TEXT "$(OptionsTitle)" "$(OptionsSubtitle)"
  nsDialogs::Create 1018
  Pop $Dialog
  ${If} $Dialog == error
    Abort
  ${EndIf}
  ${NSD_CreateCheckbox} 0 0 100% 12u "$(OptionDesktopShortcut)"
  Pop $ChkDesktop
  ${If} $DesktopShortcut = 1
    ${NSD_Check} $ChkDesktop
  ${EndIf}
  ${NSD_CreateCheckbox} 0 18u 100% 20u "$(OptionUpdateChecks)"
  Pop $ChkUpdates
  ${If} $UpdateChecks = 1
    ${NSD_Check} $ChkUpdates
  ${EndIf}
  ${NSD_CreateLink} 12u 40u 100u 12u "$(OptionPrivacyLink)"
  Pop $R0
  ${NSD_OnClick} $R0 OpenPrivacyPage
  nsDialogs::Show
FunctionEnd

Function OptionsLeave
  ${NSD_GetState} $ChkDesktop $R0
  ${If} $R0 = ${BST_CHECKED}
    StrCpy $DesktopShortcut 1
  ${Else}
    StrCpy $DesktopShortcut 0
  ${EndIf}
  ${NSD_GetState} $ChkUpdates $R0
  ${If} $R0 = ${BST_CHECKED}
    StrCpy $UpdateChecks 1
  ${Else}
    StrCpy $UpdateChecks 0
  ${EndIf}
FunctionEnd

Function OpenPrivacyPage
  Pop $R0
  ExecShell "open" "${SITE}/privacy#desktop"
FunctionEnd

Function FinishRun
  StrCpy $LaunchArgs ""
  Call LaunchApp
FunctionEnd

; ---- the install ----

; WaitForApp (06 section 6.7): until Local\io.isshoni.desktop.running is gone. /UPDATE and /S: poll every 500 ms for up to
; 60 s; interactive: Retry/Cancel. Still running -> exit 3.
Function WaitForApp
  StrCpy $R1 0
  ${Do}
    !insertmacro MutexExists "${MUTEX_RUNNING}" $R0
    ${If} $R0 = 0
      ${ExitDo}
    ${EndIf}
    ${If} ${Silent}
    ${OrIf} $UpdateMode = 1
      ${If} $R1 >= ${WAIT_POLLS}
        Call BannerOff
        SetErrorLevel ${EXIT_RUNNING}
        Quit
      ${EndIf}
      Sleep ${WAIT_POLL_MS}
      IntOp $R1 $R1 + 1
    ${Else}
      MessageBox MB_RETRYCANCEL|MB_ICONEXCLAMATION "$(AppRunning)" /SD IDCANCEL IDRETRY wait_retry
      SetErrorLevel ${EXIT_RUNNING}
      Quit
      wait_retry:
    ${EndIf}
  ${Loop}
FunctionEnd

; WebView2Present: $R0 = 1 when the runtime's pv (HKLM WOW6432Node or HKCU, 06 section 6.5) is at least WEBVIEW2_MIN;
; 0.0.0.0, a missing value or an older version count as missing.
Function WebView2Present
  StrCpy $R0 0
  ReadRegStr $R1 HKLM "SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\${WEBVIEW2_GUID}" "pv"
  Call WebView2Check
  ReadRegStr $R1 HKCU "Software\Microsoft\EdgeUpdate\Clients\${WEBVIEW2_GUID}" "pv"
  Call WebView2Check
FunctionEnd

Function WebView2Check
  ${If} $R1 != ""
  ${AndIf} $R1 != "0.0.0.0"
    ${VersionCompare} $R1 "${WEBVIEW2_MIN}" $R2
    ${If} $R2 <> 2
      StrCpy $R0 1
    ${EndIf}
  ${EndIf}
FunctionEnd

; EnsureWebView2: when the runtime is missing, the embedded bootstrapper (per user when not elevated) with up to
; 15 minutes, then the check again; still missing -> Microsoft's download page (interactive) and exit 7.
Function EnsureWebView2
  Call WebView2Present
  ${If} $R0 = 1
    Return
  ${EndIf}
  DetailPrint "$(WebView2Installing)"
  SetOutPath "$PLUGINSDIR"
  File "/oname=$PLUGINSDIR\MicrosoftEdgeWebview2Setup.exe" "${WEBVIEW2_SETUP}"
  nsExec::Exec /TIMEOUT=${WEBVIEW2_TIMEOUT_MS} '"$PLUGINSDIR\MicrosoftEdgeWebview2Setup.exe" /silent /install'
  Pop $R3
  Delete "$PLUGINSDIR\MicrosoftEdgeWebview2Setup.exe"
  Call WebView2Present
  ${If} $R0 = 1
    Return
  ${EndIf}
  Call BannerOff
  ${IfNot} ${Silent}
    MessageBox MB_YESNO|MB_ICONSTOP "$(WebView2Missing)" /SD IDNO IDNO webview2_quit
    ExecShell "open" "${WEBVIEW2_PAGE}"
  ${EndIf}
  webview2_quit:
  SetErrorLevel ${EXIT_WEBVIEW2}
  Quit
FunctionEnd

; InNewList: $Found = 1 when $Line is a line of the new install-files.txt.
Function InNewList
  StrCpy $Found 0
  ClearErrors
  FileOpen $Fh2 "$PLUGINSDIR\new-files.txt" r
  ${If} ${Errors}
    Return
  ${EndIf}
  ${Do}
    ClearErrors
    FileRead $Fh2 $Line2
    ${If} ${Errors}
      ${ExitDo}
    ${EndIf}
    Push $Line2
    Call TrimLine
    Pop $Line2
    ${If} $Line2 == $Line
      StrCpy $Found 1
      ${ExitDo}
    ${EndIf}
  ${Loop}
  FileClose $Fh2
FunctionEnd

; IsImage: $Found = 1 when $Line names a PE file (.exe or .dll).
Function IsImage
  StrCpy $Found 0
  StrCpy $R0 $Line 4 -4
  ${If} $R0 == ".exe"
  ${OrIf} $R0 == ".dll"
    StrCpy $Found 1
  ${EndIf}
FunctionEnd

; MoveAsideImages (06 section 6.7): every PE file of the previous install-files.txt that this install overwrites is renamed
; to <file>.old, retried every 250 ms for up to 10 s (a mapped image can be renamed but not overwritten); a rename
; that still fails exits 6.
Function MoveAsideImages
  ClearErrors
  FileOpen $Fh "$PLUGINSDIR\old-files.txt" r
  ${If} ${Errors}
    Return
  ${EndIf}
  ${Do}
    ClearErrors
    FileRead $Fh $Line
    ${If} ${Errors}
      ${ExitDo}
    ${EndIf}
    Push $Line
    Call TrimLine
    Pop $Line
    Push $Line
    Call SafeRel
    Pop $R0
    ${If} $R0 <> 1
      ${Continue}
    ${EndIf}
    Call IsImage
    ${If} $Found <> 1
      ${Continue}
    ${EndIf}
    ${IfNot} ${FileExists} "$INSTDIR\$Line"
      ${Continue}
    ${EndIf}
    Call InNewList
    ${If} $Found <> 1
      ${Continue}
    ${EndIf}
    StrCpy $Tries 0
    ${Do}
      Delete "$INSTDIR\$Line.old"
      ClearErrors
      Rename "$INSTDIR\$Line" "$INSTDIR\$Line.old"
      ${IfNot} ${Errors}
        ${ExitDo}
      ${EndIf}
      IntOp $Tries $Tries + 1
      ${If} $Tries >= ${RENAME_POLLS}
        FileClose $Fh
        StrCpy $R9 ${EXIT_WRITE}
        StrCpy $R8 "$(WriteFailed)"
        Call Fail
      ${EndIf}
      Sleep ${RENAME_POLL_MS}
    ${Loop}
  ${Loop}
  FileClose $Fh
FunctionEnd

; RemoveStale (06 section 6.6): after the new files are written, every file of the previous list that the new one doesn't
; have (unsafe paths are skipped and logged), the .old files of both lists (06 section 6.7; one still mapped stays for
; the next run), and folders that end up empty.
Function RemoveStale
  ClearErrors
  FileOpen $Fh "$PLUGINSDIR\old-files.txt" r
  ${IfNot} ${Errors}
    ${Do}
      ClearErrors
      FileRead $Fh $Line
      ${If} ${Errors}
        ${ExitDo}
      ${EndIf}
      Push $Line
      Call TrimLine
      Pop $Line
      ${If} $Line == ""
        ${Continue}
      ${EndIf}
      Push $Line
      Call SafeRel
      Pop $R0
      ${If} $R0 <> 1
        DetailPrint "$(StaleSkipped)"
        ${Continue}
      ${EndIf}
      Delete "$INSTDIR\$Line.old"
      Call InNewList
      ${If} $Found = 1
        ${Continue}
      ${EndIf}
      Delete "$INSTDIR\$Line"
      ${GetParent} "$INSTDIR\$Line" $R1
      ${If} $R1 != $INSTDIR
        RMDir $R1
      ${EndIf}
    ${Loop}
    FileClose $Fh
  ${EndIf}
  ClearErrors
  FileOpen $Fh "$PLUGINSDIR\new-files.txt" r
  ${IfNot} ${Errors}
    ${Do}
      ClearErrors
      FileRead $Fh $Line
      ${If} ${Errors}
        ${ExitDo}
      ${EndIf}
      Push $Line
      Call TrimLine
      Pop $Line
      ${If} $Line != ""
        Delete "$INSTDIR\$Line.old"
      ${EndIf}
    ${Loop}
    FileClose $Fh
  ${EndIf}
FunctionEnd

; SetShortcutAUMID: Pop a .lnk path and set its System.AppUserModel.ID to io.isshoni.desktop (06 section 2.3) through
; IShellLinkW, IPersistFile and IPropertyStore (NSIS's Win\COM.nsh and Win\Propkey.nsh); $R0 = 0 on success.
Function SetShortcutAUMID
  Exch $R1 ; the .lnk
  Push $R2
  Push $R3
  Push $R4
  Push $R5
  Push $R6
  Push $R7
  StrCpy $R0 1
  ; CLSID_ShellLink, IID_IShellLinkW
  System::Call 'ole32::CoCreateInstance(g "{00021401-0000-0000-C000-000000000046}", p 0, i ${CLSCTX_INPROC_SERVER}, g "{000214F9-0000-0000-C000-000000000046}", *p .R2) i .R3'
  ${If} $R3 = 0
  ${AndIf} $R2 P<> 0
    ${IUnknown::QueryInterface} $R2 '("${IID_IPersistFile}",.R4).R3'
    ${If} $R3 = 0
      ${IPersistFile::Load} $R4 '("$R1",${STGM_READWRITE}).R3'
      ${If} $R3 = 0
        ${IUnknown::QueryInterface} $R2 '("${IID_IPropertyStore}",.R5).R3'
        ${If} $R3 = 0
          System::Call '*${SYSSTRUCT_PROPERTYKEY}(${PKEY_AppUserModel_ID})p.R6'
          System::Call '*(&w64 "${AUMID}")p.R7'
          System::Call '*${SYSSTRUCT_PROPVARIANT}(${VT_LPWSTR},,$R7)p.R3'
          ${IPropertyStore::SetValue} $R5 '($R6,$R3).R0'
          ${If} $R0 = 0
            ${IPropertyStore::Commit} $R5 '().R0'
          ${EndIf}
          System::Free $R3
          System::Free $R7
          System::Free $R6
          ${IUnknown::Release} $R5 ''
          ${If} $R0 = 0
            ${IPersistFile::Save} $R4 '("$R1",1).R0'
          ${EndIf}
        ${EndIf}
      ${EndIf}
      ${IUnknown::Release} $R4 ''
    ${EndIf}
    ${IUnknown::Release} $R2 ''
  ${EndIf}
  Pop $R7
  Pop $R6
  Pop $R5
  Pop $R4
  Pop $R3
  Pop $R2
  Pop $R1
FunctionEnd

Function MakeShortcut
  Exch $R7 ; the .lnk
  CreateShortcut "$R7" "$INSTDIR\${APP_EXE}" "" "$INSTDIR\${APP_EXE}" 0
  Push $R7
  Call SetShortcutAUMID
  ${If} $R0 <> 0
    DetailPrint "$(ShortcutIdFailed)"
  ${EndIf}
  Pop $R7
FunctionEnd

Function WriteRegistry
  ClearErrors
  WriteRegStr HKCU "${REG_APP}" "InstallDir" "$INSTDIR"
  WriteRegStr HKCU "${REG_APP}" "Version" "${VERSION}"
  WriteRegStr HKCU "${REG_APP}" "WinVer" "${WINVER}"
  WriteRegDWORD HKCU "${REG_APP}" "DesktopShortcut" $DesktopShortcut
  WriteRegDWORD HKCU "${REG_APP}" "UpdateChecks" $UpdateChecks
  ${If} $PendingServer != ""
    WriteRegStr HKCU "${REG_APP}" "PendingServer" "$PendingServer"
    DeleteRegValue HKCU "${REG_APP}" "PendingRoom"
  ${EndIf}

  WriteRegStr HKCU "${REG_UNINST}" "DisplayName" "${PRODUCT}"
  WriteRegStr HKCU "${REG_UNINST}" "DisplayVersion" "${VERSION}"
  WriteRegStr HKCU "${REG_UNINST}" "Publisher" "${PUBLISHER}"
  WriteRegStr HKCU "${REG_UNINST}" "DisplayIcon" '"$INSTDIR\${APP_EXE}",0'
  WriteRegStr HKCU "${REG_UNINST}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "${REG_UNINST}" "UninstallString" '"$INSTDIR\uninstall.exe"'
  WriteRegStr HKCU "${REG_UNINST}" "QuietUninstallString" '"$INSTDIR\uninstall.exe" /S'
  WriteRegStr HKCU "${REG_UNINST}" "URLInfoAbout" "${SITE}/"
  WriteRegStr HKCU "${REG_UNINST}" "HelpLink" "${SITE}/download#windows"
  WriteRegDWORD HKCU "${REG_UNINST}" "NoModify" 1
  WriteRegDWORD HKCU "${REG_UNINST}" "NoRepair" 1
  ${GetSize} "$INSTDIR" "/S=0K" $R0 $R1 $R2
  WriteRegDWORD HKCU "${REG_UNINST}" "EstimatedSize" $R0

  ; isshoni:// (02 section 12.2, 03 section 13.1): --open-url takes exactly the next argument and honours no other flag.
  WriteRegStr HKCU "${REG_PROTO}" "" "URL:isshoni"
  WriteRegStr HKCU "${REG_PROTO}" "URL Protocol" ""
  WriteRegStr HKCU "${REG_PROTO}\DefaultIcon" "" '"$INSTDIR\${APP_EXE}",0'
  WriteRegStr HKCU "${REG_PROTO}\shell\open\command" "" '"$INSTDIR\${APP_EXE}" --open-url "%1"'
  ${If} ${Errors}
    StrCpy $R9 ${EXIT_OTHER}
    StrCpy $R8 "$(RegistryFailed)"
    Call Fail
  ${EndIf}
FunctionEnd

Section "-isshoni"
  ${If} $UpdateMode = 1
    SetAutoClose true
  ${EndIf}
  ${If} $ServerIgnored = 1
    DetailPrint "$(ServerIgnored)"
  ${EndIf}
  Call WaitForApp
  Call EnsureWebView2

  ClearErrors
  CreateDirectory "$INSTDIR"
  ${If} ${Errors}
    StrCpy $R9 ${EXIT_WRITE}
    StrCpy $R8 "$(WriteFailed)"
    Call Fail
  ${EndIf}

  ; The previous and the new file lists (06 section 6.6).
  Delete "$PLUGINSDIR\old-files.txt"
  ${If} ${FileExists} "$INSTDIR\${FILE_LIST}"
    CopyFiles /SILENT "$INSTDIR\${FILE_LIST}" "$PLUGINSDIR\old-files.txt"
  ${EndIf}
  SetOutPath "$PLUGINSDIR"
  File "/oname=$PLUGINSDIR\new-files.txt" "${STAGE}/${FILE_LIST}"
  Call MoveAsideImages

  ; The staged tree (06 section 2.2) without its list, which is written last. "try" sets the error flag on a file that
  ; can't be written instead of asking.
  SetOutPath "$INSTDIR"
  SetOverwrite try
  ClearErrors
  File /r /x "${FILE_LIST}" "${STAGE}/*"
  ${If} ${Errors}
    StrCpy $R9 ${EXIT_WRITE}
    StrCpy $R8 "$(WriteFailed)"
    Call Fail
  ${EndIf}
  Call RemoveStale
  ClearErrors
  File "${STAGE}/${FILE_LIST}"
  !ifdef PREBUILT_UNINSTALLER
    File "/oname=uninstall.exe" "${PREBUILT_UNINSTALLER}"
  !else
    WriteUninstaller "$INSTDIR\uninstall.exe"
  !endif
  ${If} ${Errors}
    StrCpy $R9 ${EXIT_WRITE}
    StrCpy $R8 "$(WriteFailed)"
    Call Fail
  ${EndIf}
  SetOverwrite on

  Call WriteRegistry

  ; Shortcuts with the AppUserModelID of the app's toasts and taskbar group (06 section 2.3).
  Push "$SMPROGRAMS\isshoni.lnk"
  Call MakeShortcut
  ${If} $DesktopShortcut = 1
    Push "$DESKTOP\isshoni.lnk"
    Call MakeShortcut
  ${Else}
    Delete "$DESKTOP\isshoni.lnk"
  ${EndIf}
SectionEnd

; LaunchApp: start isshoni.exe$LaunchArgs, never elevated (06 section 6.4): from an elevated installer through explorer.exe,
; which drops the arguments. The installing mutex is closed right before (06 section 6.7): this is the installer's last
; action.
Function LaunchApp
  Call BannerOff
  System::Call 'kernel32::CloseHandle(p $InstallingMutex)'
  StrCpy $InstallingMutex 0
  System::Call 'shell32::IsUserAnAdmin() i .R0'
  ${If} $R0 <> 0
    Exec '"$WINDIR\explorer.exe" "$INSTDIR\${APP_EXE}"'
  ${Else}
    Exec '"$INSTDIR\${APP_EXE}"$LaunchArgs'
  ${EndIf}
FunctionEnd

; Silent and /UPDATE runs have no Finish page: /RELAUNCH and /LAUNCH start the app here.
Function .onInstSuccess
  ${If} ${Silent}
  ${OrIf} $UpdateMode = 1
    ${If} $Relaunch = 1
      StrCpy $LaunchArgs " --updated-from=$OldVersion"
      ${If} $OldVersion == ""
        StrCpy $LaunchArgs ""
      ${EndIf}
      Call LaunchApp
    ${ElseIf} $Launch = 1
      StrCpy $LaunchArgs ""
      Call LaunchApp
    ${EndIf}
  ${EndIf}
  Call BannerOff
FunctionEnd

Function .onInstFailed
  Call BannerOff
FunctionEnd

!ifndef PREBUILT_UNINSTALLER
  !include uninstall.nsh
!endif
