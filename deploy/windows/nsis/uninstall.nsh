; uninstall.nsh: the uninstaller (docs/m2/06-windows-release.md section 6.3, section 6.8), included by installer.nsi when it writes
; the uninstaller itself (the Mac, WriteUninstaller) and by uninst-gen.nsi, the prebuilt-uninstaller generator of
; 06 section 6.9 (CI). Its pages come from the IsshoniUninstallPages macro, which the including script inserts before
; MUI_LANGUAGE; this file is included after the language.
;
; Flags: /S (silent), /PURGE (run isshoni.exe --purge-data instead of --forget-credentials, and remove the data root).
; Exit codes: 0, 1 (cancelled), 3 (isshoni is still running).
;
; Order (06 section 6.8): wait for the running mutex; ALWAYS run isshoni.exe --forget-credentials (or --purge-data) with a
; 30 s cap, logging a failure and going on; delete the files install-files.txt lists and their .old leftovers, then
; the list, uninstall.exe and the folders when empty (never RMDir /r in $INSTDIR); both shortcuts; the Uninstall key,
; the protocol key only when it points into $INSTDIR, the Run value; the toast registration (the CustomActivator's
; CLSID key only when its LocalServer32 points into $INSTDIR, then the AppUserModelId key); HKCU\Software\isshoni.

!ifndef ISSHONI_UNINSTALL_NSH
!define ISSHONI_UNINSTALL_NSH

Var Purge
Var UnDialog
Var UnPurgeCheckbox

!insertmacro TrimLineFunction "un."
!insertmacro SafeRelFunction "un."
!insertmacro PointsIntoInstDirFunction "un."

Function un.onInit
  SetShellVarContext current
  SetRegView 64
  StrCpy $Purge 0
  ${GetParameters} $R0
  ClearErrors
  ${GetOptions} $R0 "/PURGE" $R1
  ${IfNot} ${Errors}
    StrCpy $Purge 1
  ${EndIf}
FunctionEnd

; The confirm page: one paragraph and "[ ] Also remove my settings and logs on this PC" (unchecked unless /PURGE).
Function un.ConfirmPage
  !insertmacro MUI_HEADER_TEXT "$(UnConfirmTitle)" "$(UnConfirmSubtitle)"
  nsDialogs::Create 1018
  Pop $UnDialog
  ${If} $UnDialog == error
    Abort
  ${EndIf}
  ${NSD_CreateLabel} 0 0 100% 36u "$(UnConfirmText)"
  Pop $R0
  ${NSD_CreateCheckbox} 0 44u 100% 12u "$(UnPurgeOption)"
  Pop $UnPurgeCheckbox
  ${If} $Purge = 1
    ${NSD_Check} $UnPurgeCheckbox
  ${EndIf}
  nsDialogs::Show
FunctionEnd

Function un.ConfirmLeave
  ${NSD_GetState} $UnPurgeCheckbox $R0
  ${If} $R0 = ${BST_CHECKED}
    StrCpy $Purge 1
  ${Else}
    StrCpy $Purge 0
  ${EndIf}
FunctionEnd

; un.WaitForApp: until Local\io.isshoni.desktop.running is gone. Silent: poll every 500 ms for up to 60 s.
; Interactive: Retry/Cancel. Still running -> exit 3.
Function un.WaitForApp
  StrCpy $R1 0
  ${Do}
    !insertmacro MutexExists "${MUTEX_RUNNING}" $R0
    ${If} $R0 = 0
      ${ExitDo}
    ${EndIf}
    ${If} ${Silent}
      ${If} $R1 >= ${WAIT_POLLS}
        SetErrorLevel ${EXIT_RUNNING}
        Quit
      ${EndIf}
      Sleep ${WAIT_POLL_MS}
      IntOp $R1 $R1 + 1
    ${Else}
      MessageBox MB_RETRYCANCEL|MB_ICONEXCLAMATION "$(AppRunning)" /SD IDCANCEL IDRETRY un_wait_retry
      SetErrorLevel ${EXIT_RUNNING}
      Quit
      un_wait_retry:
    ${EndIf}
  ${Loop}
FunctionEnd

; un.RemoveListed: every file install-files.txt lists (plain relative paths only) and its .old leftover (06 section 6.7);
; their folders when they end up empty. Without a list (a damaged install) only the fixed names of 06 section 2.2.
Function un.RemoveListed
  ClearErrors
  FileOpen $R5 "$INSTDIR\${FILE_LIST}" r
  ${If} ${Errors}
    Delete "$INSTDIR\${APP_EXE}"
    Delete "$INSTDIR\isshoni-media.dll"
    Delete "$INSTDIR\licenses\LICENSE.txt"
    Delete "$INSTDIR\licenses\NOTICE.txt"
    Delete "$INSTDIR\licenses\THIRD_PARTY_NOTICES.txt"
    Delete "$INSTDIR\licenses\ffmpeg\COPYING.LGPLv2.1.txt"
    Delete "$INSTDIR\licenses\ffmpeg\LICENSE.md"
    Delete "$INSTDIR\licenses\ffmpeg\README-LGPL.txt"
    Return
  ${EndIf}
  ${Do}
    ClearErrors
    FileRead $R5 $R6
    ${If} ${Errors}
      ${ExitDo}
    ${EndIf}
    Push $R6
    Call un.TrimLine
    Pop $R6
    ${If} $R6 == ""
      ${Continue}
    ${EndIf}
    Push $R6
    Call un.SafeRel
    Pop $R7
    ${If} $R7 <> 1
      ${Continue}
    ${EndIf}
    Delete "$INSTDIR\$R6"
    Delete "$INSTDIR\$R6.old"
    ${GetParent} "$INSTDIR\$R6" $R7
    ${If} $R7 != $INSTDIR
      RMDir $R7
    ${EndIf}
  ${Loop}
  FileClose $R5
FunctionEnd

Section "Uninstall"
  Call un.WaitForApp

  ; 2. The sign-ins, always (also under /S): every linked device revoked where reachable and every isshoni:v1:*
  ; credential deleted; with /PURGE or the checkbox also the data root (06 section 2.4). A 30 s cap; failures are logged.
  ${If} ${FileExists} "$INSTDIR\${APP_EXE}"
    DetailPrint "$(UnForgetting)"
    ${If} $Purge = 1
      nsExec::Exec /TIMEOUT=${FORGET_TIMEOUT_MS} '"$INSTDIR\${APP_EXE}" --purge-data'
    ${Else}
      nsExec::Exec /TIMEOUT=${FORGET_TIMEOUT_MS} '"$INSTDIR\${APP_EXE}" --forget-credentials'
    ${EndIf}
    Pop $R1
    ${If} $R1 != 0
      DetailPrint "$(UnForgetFailed)"
    ${EndIf}
  ${EndIf}
  ; The data root of 06 section 2.2 is never $INSTDIR: when --purge-data couldn't remove it, the uninstaller does.
  ${If} $Purge = 1
  ${AndIf} ${FileExists} "$LOCALAPPDATA\isshoni\*.*"
    RMDir /r "$LOCALAPPDATA\isshoni"
  ${EndIf}

  ; 3. The files, never recursively.
  Call un.RemoveListed
  Delete "$INSTDIR\${FILE_LIST}"
  Delete "$INSTDIR\uninstall.exe"
  RMDir "$INSTDIR\licenses\ffmpeg"
  RMDir "$INSTDIR\licenses"
  RMDir "$INSTDIR"
  Delete "$SMPROGRAMS\isshoni.lnk"
  Delete "$DESKTOP\isshoni.lnk"

  ; 4. The Apps & features entry, the protocol handler when it is this install's, the Start with Windows value.
  DeleteRegKey HKCU "${REG_UNINST}"
  ReadRegStr $R0 HKCU "${REG_PROTO}\shell\open\command" ""
  Push $R0
  Call un.PointsIntoInstDir
  Pop $R1
  ${If} $R1 = 1
    DeleteRegKey HKCU "${REG_PROTO}"
  ${EndIf}
  DeleteRegValue HKCU "${REG_RUN}" "isshoni"

  ; 5. The toast registration that the app's notifications service wrote (03 section 15). The CLSID key goes only when it
  ; is a {GUID} whose LocalServer32 points into $INSTDIR, so an empty or odd CustomActivator can never name another
  ; key (or the whole CLSID tree).
  ReadRegStr $R0 HKCU "${REG_AUMID}" "CustomActivator"
  StrLen $R1 $R0
  StrCpy $R2 $R0 1
  StrCpy $R3 $R0 1 37
  ${If} $R1 = 38
  ${AndIf} $R2 == "{"
  ${AndIf} $R3 == "}"
    ReadRegStr $R4 HKCU "${REG_CLSID}\$R0\LocalServer32" ""
    ${If} $R4 != ""
      Push $R4
      Call un.PointsIntoInstDir
      Pop $R5
      ${If} $R5 = 1
        DeleteRegKey HKCU "${REG_CLSID}\$R0"
      ${EndIf}
    ${EndIf}
  ${EndIf}
  DeleteRegKey HKCU "${REG_AUMID}"

  ; 6. The app's key, values and subkeys.
  DeleteRegKey HKCU "${REG_APP}"
SectionEnd

!endif ; ISSHONI_UNINSTALL_NSH
