; ASIAIR Commander — Windows installer.
;
; MIT License. Copyright (c) 2026 Marc-Antoine Courteau.
;
; Compiled by makensis on Linux CI (the stock Debian `nsis` package) — no
; Windows machine is involved in the build. Only standard NSIS features are
; used (MUI2, LogicLib, FileFunc, StrFunc, nsExec) so the stock package's
; makensis compiles this without extra plugins.
;
; There is no per-user component, so no elevation/profile-path relaunch
; dance is needed: `RequestExecutionLevel admin` alone is enough — every
; path this installer touches (HKLM, %ProgramData%, the service) is
; machine-wide, not per-user, so Windows'
; own UAC elevation prompt (triggered by the manifest, no manual runas) is
; sufficient.
;
; What it does:
;   - installs asiair-commander.exe into a chosen directory, remembering the
;     choice across upgrades in HKLM so the exe stays put, with the licence and
;     third-party notices (THIRD_PARTY_NOTICES\, from `make notices`) beside it;
;   - on upgrade, stops the ASIAIRCommander service before overwriting the
;     binary: sc.exe query -> sc.exe stop -> poll until STOPPED (bounded,
;     ~30s);
;   - registers the service on first install (the binary's own `install`
;     verb) and starts it via sc.exe start — a start failure is reported,
;     not fatal (a fresh install has no config yet);
;   - waits for the service to report healthy before the finish screen: the
;     binary's own `healthcheck` verb polls GET /healthz internally and
;     exits 0 once green, so the installer just polls that exit code in a
;     bounded loop (~30s), mirroring the stop-poll — no NSIS HTTP plugin
;     needed;
;   - finish page offers an "Open ASIAIR Commander" checkbox, checked by
;     default, that opens the web UI in the default browser — handing the
;     one-time setup token to that browser in the URL fragment when the
;     service is still unconfigured (see FinishShow / LaunchWebUI);
;   - uninstall stops + unregisters the service and removes the binary, the
;     notices and the install directory, but explicitly preserves
;     %ProgramData%\ASIAIRCommander\ (config, state, logs) — uninstalling
;     the program must not destroy the user's setup or history.
;
; Compile-time defines (all optional — sensible defaults below so a bare
; `make installer` / `makensis -DVERSION=... deploy/windows/installer.nsi`
; works):
;   VERSION  x.y.z, numeric, no leading "v" — display + VIProductVersion.
;   PAYLOAD  directory holding asiair-commander.exe and THIRD_PARTY_NOTICES\
;            (default: bin)
;   OUTFILE  output path of the setup executable — the default embeds
;            ${VERSION} so downloads are self-identifying and successive
;            releases don't collide into "… (1).exe" in the browser's
;            downloads folder
;            (default: dist/asiair-commander-setup-<VERSION>.exe)

; PAYLOAD/OUTFILE are compiler-side (host filesystem) paths, read/written by
; makensis itself while compiling — on Linux CI that filesystem only honors
; "/", so these stay forward-slash. $INSTDIR-rooted paths elsewhere in this
; script are target-side (resolved on the Windows machine at install time)
; and use "\" as usual.
; NSIS resolves relative File/OutFile paths against the SCRIPT's own
; directory (deploy/windows/), not the invoking shell's cwd, so the defaults
; below climb back to the repo root — where `make build-windows` and
; `make installer` put bin/ and dist/ — matching a bare
; `makensis -DVERSION=... deploy/windows/installer.nsi` run from there.
!define /ifndef VERSION "0.0.0"
!define /ifndef PAYLOAD "../../bin"
!define /ifndef OUTFILE "../../dist/asiair-commander-setup-${VERSION}.exe"

; Authenticode signing, off unless `make installer SIGN=1` passes SIGNSH (the
; absolute path of scripts/sign-windows.sh — absolute because !finalize runs
; through system() against makensis's working directory, not this script's).
; Gated so a bare `make installer` still builds unsigned on a laptop and in
; the CI installer job, neither of which has Azure credentials.
;
; The uninstaller is signed by !uninstfinalize while NSIS still has it as a
; standalone PE on disk; once it is compressed into the installer it can never
; be signed, and an unsigned uninstaller shows "Unknown publisher" on the UAC
; prompt at removal time. The `= 0` comparisons abort the build on a signing
; failure — that keeps a failed signature from reaching the release job's
; tag-and-publish steps, which run only after `make installer` succeeds.
!ifdef SIGN
  !ifndef SIGNSH
    !error "SIGN requires SIGNSH: the absolute path to scripts/sign-windows.sh"
  !endif
  !uninstfinalize '"${SIGNSH}" "%1"' = 0
  !finalize       '"${SIGNSH}" "%1"' = 0
!endif

!define PRODUCT "ASIAIR Commander"
!define BINARY "asiair-commander.exe"
!define SVC "ASIAIRCommander"
!define REGKEY "Software\ASIAIRCommander"
!define UNINSTKEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\ASIAIRCommander"
!define UNINSTALLER "uninstall-asiair-commander.exe"
!define WEBPORT "8686"
; NSIS has no built-in $PROGRAMDATA constant; the documented idiom is
; `SetShellVarContext all` (set in .onInit, below) plus $APPDATA, which then
; resolves to CSIDL_COMMON_APPDATA — %ProgramData% on any Windows version
; this targets — instead of the per-user roaming AppData folder.
!define DATADIR "$APPDATA\ASIAIRCommander"

Name "${PRODUCT} ${VERSION}"
OutFile "${OUTFILE}"
Unicode true
RequestExecutionLevel admin
InstallDir "$PROGRAMFILES64\ASIAIR Commander"
InstallDirRegKey HKLM "${REGKEY}" "InstallDir"

VIProductVersion "${VERSION}.0"
VIAddVersionKey "ProductName" "${PRODUCT}"
VIAddVersionKey "ProductVersion" "${VERSION}"
VIAddVersionKey "FileDescription" "ASIAIR Commander installer"
VIAddVersionKey "FileVersion" "${VERSION}"
VIAddVersionKey "LegalCopyright" "Copyright (c) 2026 Marc-Antoine Courteau"

!include "MUI2.nsh"
!include "LogicLib.nsh"
!include "FileFunc.nsh"
!include "StrFunc.nsh"
!include "WinMessages.nsh"
${StrStr}
${UnStrStr} ; StrFunc emits a separate uninstaller-side copy; ${StrStr} is install-only

; The one-time setup token file the service writes inside ${DATADIR} while no
; password is set: line 1 is the URL it actually bound, line 2 is the token.
!define TOKENFILE "${DATADIR}\setup-token"
; Control id of the finish page's "run" checkbox in MUI2's inner dialog.
!define FINISHPAGE_RUN_CHECKBOX 1203

Var SvcExisted ; "1" when the service was already registered before this run
Var SetupURL   ; line 1 of the token file, or "" when none was read
Var SetupToken ; line 2 of the token file, or "" when none was read

!define MUI_DIRECTORYPAGE_TEXT_TOP "ASIAIR Commander installs here. On upgrade, the remembered directory is reused so the service's image path stays stable."
!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES

!define MUI_FINISHPAGE_RUN
!define MUI_FINISHPAGE_RUN_FUNCTION "LaunchWebUI"
!define MUI_FINISHPAGE_RUN_TEXT "Open ASIAIR Commander"
!define MUI_FINISHPAGE_RUN_CHECKED
; The token poll runs when the finish page is SHOWN, not in LaunchWebUI: that
; function executes only after the page has already been drawn, so a checkbox
; decided there could do no more than a non-blocking existence check — which is
; exactly the race the poll exists to avoid.
!define MUI_PAGE_CUSTOMFUNCTION_SHOW "FinishShow"
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

Function .onInit
  ; The payload is amd64; read/write the 64-bit registry view, not
  ; WOW6432Node, so the remembered InstallDir and the uninstall entry agree
  ; with a 64-bit reader (and with themselves across runs).
  SetRegView 64
  ; All-users context so ${DATADIR} ($APPDATA\ASIAIRCommander) resolves to
  ; the machine-wide %ProgramData%\ASIAIRCommander, not the per-user one —
  ; correct for a machine-wide service everyone on the box shares.
  SetShellVarContext all
FunctionEnd

; TrimEOL strips trailing CR/LF from the string on top of the stack and leaves
; the result there. FileRead returns the line terminator, and an untrimmed
; "$\r$\n" spliced into "<url>#setup_token=<token>" produces a launch that fails
; silently — the browser opens a mangled address and the operator sees no error.
Function TrimEOL
  Exch $0
  Push $1
  Push $2
  trimLoop:
    StrLen $1 $0
    ${If} $1 == 0
      Goto trimDone
    ${EndIf}
    IntOp $1 $1 - 1
    StrCpy $2 $0 1 $1
    ${If} $2 == "$\r"
    ${OrIf} $2 == "$\n"
      StrCpy $0 $0 $1
      Goto trimLoop
    ${EndIf}
  trimDone:
  Pop $2
  Pop $1
  Exch $0
FunctionEnd

; FinishShow polls for the setup token file and, if it reads one, pins the
; "Open ASIAIR Commander" checkbox checked and disabled — the hand-off is the
; supported path, and an operator who skips it has to go find the token by hand
; for no benefit. MUI_FINISHPAGE_RUN_CHECKED is a compile-time define and cannot
; express "disabled", so the control is manipulated at runtime.
;
; The poll is required rather than an optimization: the service has only just
; been started, so on a fresh install the file may not exist yet and treating
; "absent" as "already configured" would race its creation. The service removes
; the file unconditionally before minting, so its mere existence proves the
; running process wrote it — no mtime arithmetic, and no stale token from a
; previous process on an upgrade. The bound is short because this blocks the
; finish page, and finishing the install is never blocked on the outcome: with
; no token read, the plain URL is launched and the wizard asks for the token.
Function FinishShow
  StrCpy $SetupURL ""
  StrCpy $SetupToken ""
  StrCpy $R2 0
  ${Do}
    ${If} ${FileExists} "${TOKENFILE}"
      ClearErrors
      FileOpen $0 "${TOKENFILE}" r
      ${IfNot} ${Errors}
        FileRead $0 $1 ; line 1: the URL the service actually bound
        FileRead $0 $2 ; line 2: the one-time token
        FileClose $0
        Push $1
        Call TrimEOL
        Pop $1
        Push $2
        Call TrimEOL
        Pop $2
        ${If} $1 != ""
        ${AndIf} $2 != ""
          StrCpy $SetupURL $1
          StrCpy $SetupToken $2
          Goto tokenRead
        ${EndIf}
      ${EndIf}
    ${EndIf}
    Sleep 250
    IntOp $R2 $R2 + 1
  ${LoopUntil} $R2 >= 40 ; 40 x 250 ms = 10 s
  Return

  tokenRead:
  FindWindow $0 "#32770" "" $HWNDPARENT ; the finish page's inner dialog
  GetDlgItem $1 $0 ${FINISHPAGE_RUN_CHECKBOX}
  SendMessage $1 ${BM_SETCHECK} 1 0 ; 1 = BST_CHECKED
  EnableWindow $1 0
FunctionEnd

; LaunchWebUI consumes what FinishShow stashed. The URL comes from the token
; file rather than being constructed here: this script cannot resolve the scheme
; from config.yaml (NSIS has no YAML parser) and the bound port is not reliably
; ${WEBPORT}, because the service falls back to an ephemeral port when the
; configured one is taken. The token rides in the fragment, which never reaches
; the server — so it cannot land in an access log or ride along in Referer.
Function LaunchWebUI
  ${If} $SetupURL != ""
  ${AndIf} $SetupToken != ""
    ExecShell "open" "$SetupURL#setup_token=$SetupToken"
  ${Else}
    ExecShell "open" "http://localhost:${WEBPORT}/"
  ${EndIf}
FunctionEnd

; stopService: if the service is registered and running, ask the SCM to stop
; it and poll until it reports STOPPED (the stop request is asynchronous).
; sc.exe prints state tokens (RUNNING/STOPPED) untranslated.
Function stopService
  StrCpy $SvcExisted "0"
  nsExec::ExecToStack 'sc.exe query "${SVC}"'
  Pop $0
  Pop $1
  ${If} $0 != "0"
    Return ; not registered — first install
  ${EndIf}
  StrCpy $SvcExisted "1"
  ${StrStr} $2 $1 "RUNNING"
  ${If} $2 == ""
    Return ; registered but not running
  ${EndIf}

  DetailPrint "Stopping the ${SVC} service..."
  nsExec::ExecToLog 'sc.exe stop "${SVC}"'
  Pop $0
  StrCpy $R2 0
  ${Do}
    Sleep 1000
    nsExec::ExecToStack 'sc.exe query "${SVC}"'
    Pop $0
    Pop $1
    ${StrStr} $2 $1 "STOPPED"
    ${If} $2 != ""
      DetailPrint "Service stopped."
      Return
    ${EndIf}
    IntOp $R2 $R2 + 1
  ${LoopUntil} $R2 >= 30
  ${IfNot} ${Silent}
    MessageBox MB_ICONEXCLAMATION "The ${SVC} service did not stop within 30 seconds. The installer will still try to replace the binary."
  ${EndIf}
FunctionEnd

; waitForHealth: poll the binary's own `healthcheck` verb (exits 0 once
; GET /healthz is green) instead of an NSIS HTTP plugin. Bounded the same as
; stopService so a slow-starting service can't hang the installer forever.
Function waitForHealth
  DetailPrint "Waiting for ASIAIR Commander to report healthy..."
  StrCpy $R2 0
  ${Do}
    nsExec::ExecToStack '"$INSTDIR\${BINARY}" healthcheck'
    Pop $0
    Pop $1
    ${If} $0 == "0"
      DetailPrint "ASIAIR Commander is healthy."
      Return
    ${EndIf}
    Sleep 1000
    IntOp $R2 $R2 + 1
  ${LoopUntil} $R2 >= 30
  ${IfNot} ${Silent}
    MessageBox MB_ICONEXCLAMATION "ASIAIR Commander did not report healthy within 30 seconds. It may still be starting — check ${DATADIR}\logs, or open http://localhost:${WEBPORT}/ once it is ready."
  ${EndIf}
FunctionEnd

Section "ASIAIR Commander" SecMain
  Call stopService

  SetOutPath "$INSTDIR"
  File "${PAYLOAD}/${BINARY}"
  ; Bundle the ASTAP plate-solver CLI beside our binary when the build placed it in
  ; the payload — the Tier-1 provisioner resolves astap_cli next to the executable
  ; first. Guarded by /FileExists so a build without the binary still compiles; plate
  ; solving then falls back to PATH, or stays paused until a binary is present.
  ; `make installer` fetches + verifies it into the payload via tools/fetch-astap;
  ; see deploy/astap/README.md.
  !if /FileExists "${PAYLOAD}/astap_cli.exe"
    File "${PAYLOAD}/astap_cli.exe"
  !else
    !warning "astap_cli.exe not in PAYLOAD; installer will not bundle the plate solver"
  !endif
  ; The licence and third-party notices are not optional: a build without them
  ; fails here rather than shipping binaries without their notices. The old copy
  ; is removed first so an upgrade never keeps the notice of a dependency the
  ; build it installs no longer contains.
  !if /FileExists "${PAYLOAD}/THIRD_PARTY_NOTICES/README.txt"
    RMDir /r "$INSTDIR\THIRD_PARTY_NOTICES"
    SetOutPath "$INSTDIR\THIRD_PARTY_NOTICES"
    File /r "${PAYLOAD}/THIRD_PARTY_NOTICES/*"
    SetOutPath "$INSTDIR"
  !else
    !error "THIRD_PARTY_NOTICES not in PAYLOAD; run make notices (make installer does)"
  !endif
SectionEnd

Section "-ProgramData" SecData
  ; Create the data directory the service reads/writes (config.yaml,
  ; state.db, logs\) and reset its ACL: %ProgramData% is world-readable by
  ; default, and this directory ends up holding the session secret and SMB
  ; credentials (design architecture §7). /inheritance:r drops the inherited
  ; world-readable ACEs; /grant:r then sets exactly SYSTEM + Administrators,
  ; recursively, so anything the service creates underneath inherits the
  ; restriction. SIDs (not names) keep this locale-independent:
  ; S-1-5-18 = SYSTEM, S-1-5-32-544 = Administrators. icacls is a stock
  ; Windows binary invoked the same way as sc.exe elsewhere in this script —
  ; unlike the NSIS AccessControl plugin, it needs nothing beyond the base
  ; NSIS install to compile, which keeps this script buildable by the stock
  ; Debian `nsis` package. The app applies the same restriction to
  ; individual files at runtime (internal/config) as a second line of
  ; defense. The service does NOT run as SYSTEM — it runs as the
  ; least-privilege virtual account NT SERVICE\${SVC} (see SecService) — so
  ; that account is granted Full control too, AFTER the service is registered
  ; (its per-service SID resolves reliably only once the service exists). That
  ; additive grant is a separate icacls invocation in SecService, so a
  ; name-resolution hiccup there can never undo the SYSTEM+Administrators
  ; lockdown set here.
  CreateDirectory "${DATADIR}"
  CreateDirectory "${DATADIR}\logs"
  nsExec::ExecToLog 'icacls "${DATADIR}" /inheritance:r /grant:r "*S-1-5-18:(OI)(CI)F" "*S-1-5-32-544:(OI)(CI)F"'
  Pop $0
  ${If} $0 != "0"
    DetailPrint "Warning: could not restrict permissions on ${DATADIR} (icacls exit $0)."
  ${EndIf}
SectionEnd

Section "-Service" SecService
  ${If} $SvcExisted != "1"
    DetailPrint "Registering the ${SVC} service..."
    nsExec::ExecToLog '"$INSTDIR\${BINARY}" install'
    Pop $0
    ${If} $0 != "0"
      DetailPrint "Service registration failed (exit $0) — register manually with: ${BINARY} install"
    ${EndIf}
  ${EndIf}

  ; Pin the logon account to the least-privilege virtual service account. The
  ; install verb already registers it this way on a fresh install, but running
  ; sc.exe config here is idempotent AND normalises an existing registration,
  ; whatever account it uses (e.g. LocalSystem), onto the virtual account (the
  ; install verb is skipped when the service already exists). The space after obj= is
  ; required by sc.exe; no password is passed because a per-service virtual account
  ; is passwordless (Windows auto-provisions it; passing password= "" has
  ; undocumented conversion semantics, so it is omitted).
  DetailPrint "Setting the ${SVC} service to run as NT SERVICE\${SVC}..."
  nsExec::ExecToLog 'sc.exe config "${SVC}" obj= "NT SERVICE\${SVC}"'
  Pop $0
  ${If} $0 != "0"
    DetailPrint "Warning: could not set the service logon account (sc config exit $0)."
  ${EndIf}

  ; Verify the logon account actually took: a silent failure could leave an
  ; existing service running as LocalSystem (full privilege), quietly defeating the whole
  ; hardening. Read the configured logon account back and alert loudly if it is
  ; not the virtual account, so the operator can act rather than unknowingly stay
  ; on LocalSystem. LocalSystem reports "LocalSystem"; only the virtual account
  ; carries the "NT SERVICE" domain here.
  nsExec::ExecToStack 'sc.exe qc "${SVC}"'
  Pop $0
  Pop $1
  ${StrStr} $2 $1 "NT SERVICE"
  ${If} $2 == ""
    MessageBox MB_ICONEXCLAMATION "Could not confirm the ${SVC} service runs as the least-privilege account NT SERVICE\${SVC} — it may still be running as LocalSystem. Set it manually with sc config and restart the service, or reinstall."
  ${EndIf}

  ; Now that the service (and thus its per-service SID) exists, additively grant
  ; that account Full control of the data dir so it can read/write config.yaml,
  ; state.db and logs\ — it is not an administrator and cannot self-grant this.
  ; /grant (not /grant:r) leaves the SYSTEM+Administrators lockdown from SecData
  ; intact. /T also applies the grant to files ALREADY present: a config.yaml written
  ; by a service running as LocalSystem carries a protected
  ; (inheritance-disabled) DACL of SYSTEM+Administrators only (set by
  ; internal/config restrictFile), so an inheritance-only grant on the directory
  ; would never reach it and the virtual account could not read its own config on first
  ; boot. Done BEFORE the first start below so the account can read/write on boot.
  ;
  ; This grant is by name (not SID like SecData's lockdown) because a per-service
  ; SID is derived from the name and not knowable here without an extra sc.exe
  ; showsid round-trip. The "NT SERVICE" well-known domain is not localized (unlike
  ; "NT AUTHORITY"), so the name resolves on non-English Windows.
  nsExec::ExecToLog 'icacls "${DATADIR}" /grant "NT SERVICE\${SVC}:(OI)(CI)F" /T'
  Pop $0
  ${If} $0 != "0"
    DetailPrint "Warning: could not grant ${SVC} access to ${DATADIR} (icacls exit $0)."
  ${EndIf}

  ; Allow inbound connections to the web UI so it is reachable from the LAN (e.g.
  ; a phone). A service runs non-interactively and cannot answer the Windows
  ; Firewall prompt, so add the rule explicitly. Delete any prior rule of the same
  ; name first so upgrades stay idempotent.
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="ASIAIR Commander"'
  nsExec::ExecToLog 'netsh advfirewall firewall add rule name="ASIAIR Commander" dir=in action=allow program="$INSTDIR\${BINARY}" enable=yes profile=any'

  DetailPrint "Starting the ${SVC} service..."
  nsExec::ExecToLog 'sc.exe start "${SVC}"'
  Pop $0
  ${If} $0 != "0"
    DetailPrint "Service start failed (exit $0)."
    ${IfNot} ${Silent}
      MessageBox MB_ICONEXCLAMATION "The ${SVC} service did not start. On a first install this usually means the setup wizard hasn't run yet — that's expected; open the web UI to finish setup. Otherwise check ${DATADIR}\logs, or start it manually with: sc start ${SVC}"
    ${EndIf}
  ${Else}
    Call waitForHealth
  ${EndIf}
SectionEnd

Section "-Bookkeeping"
  WriteRegStr HKLM "${REGKEY}" "InstallDir" $INSTDIR
  WriteUninstaller "$INSTDIR\${UNINSTALLER}"
  WriteRegStr HKLM "${UNINSTKEY}" "DisplayName" "${PRODUCT}"
  WriteRegStr HKLM "${UNINSTKEY}" "DisplayVersion" "${VERSION}"
  WriteRegStr HKLM "${UNINSTKEY}" "Publisher" "Marc-Antoine Courteau"
  WriteRegStr HKLM "${UNINSTKEY}" "InstallLocation" $INSTDIR
  WriteRegStr HKLM "${UNINSTKEY}" "UninstallString" '"$INSTDIR\${UNINSTALLER}"'
  WriteRegDWORD HKLM "${UNINSTKEY}" "NoModify" 1
  WriteRegDWORD HKLM "${UNINSTKEY}" "NoRepair" 1
SectionEnd

Function un.onInit
  SetRegView 64
  ; Same all-users context as .onInit: ${DATADIR} expands $APPDATA at RUNTIME, so
  ; without this the uninstaller resolves it to the per-user roaming AppData and
  ; every path under it silently misses %ProgramData%\ASIAIRCommander.
  SetShellVarContext all
FunctionEnd

; un.stopService mirrors the install-side stopService: sc.exe's stop request is
; asynchronous, so the service process still holds ${BINARY} open when the call
; returns. Deleting the image without waiting fails — silently, since NSIS Delete
; reports nothing — and leaves both the binary and $INSTDIR behind.
Function un.stopService
  nsExec::ExecToStack 'sc.exe query "${SVC}"'
  Pop $0
  Pop $1
  ${If} $0 != "0"
    Return ; not registered — nothing holds the binary
  ${EndIf}
  ${UnStrStr} $2 $1 "RUNNING"
  ${If} $2 == ""
    Return ; registered but not running
  ${EndIf}

  DetailPrint "Stopping the ${SVC} service..."
  nsExec::ExecToLog 'sc.exe stop "${SVC}"'
  Pop $0
  StrCpy $R2 0
  ${Do}
    Sleep 1000
    nsExec::ExecToStack 'sc.exe query "${SVC}"'
    Pop $0
    Pop $1
    ${If} $0 != "0"
      Return ; unregistered underneath us — equally fine
    ${EndIf}
    ${UnStrStr} $2 $1 "STOPPED"
    ${If} $2 != ""
      DetailPrint "Service stopped."
      Return
    ${EndIf}
    IntOp $R2 $R2 + 1
  ${LoopUntil} $R2 >= 30
  DetailPrint "The ${SVC} service did not stop within 30 seconds; removal of the binary may need a reboot."
FunctionEnd

; Uninstall removes what the installer placed — the binary, the service
; registration, and the registry entries. %ProgramData%\ASIAIRCommander\
; (config.yaml, state.db, logs\) is deliberately left alone: uninstalling
; the program must not destroy the user's setup or transfer history.
Section "Uninstall"
  Call un.stopService
  nsExec::ExecToLog '"$INSTDIR\${BINARY}" uninstall'
  Pop $0
  nsExec::ExecToLog 'netsh advfirewall firewall delete rule name="ASIAIR Commander"'
  Pop $0

  ; The SCM can report STOPPED a beat before the process image is released, and
  ; deleting the service marks a still-running one for deletion rather than
  ; killing it — so retry briefly before falling back to a reboot-scheduled
  ; delete. Without this the failure is invisible: plain Delete reports nothing.
  StrCpy $R2 0
  ${Do}
    ClearErrors
    Delete "$INSTDIR\${BINARY}"
    ${IfNot} ${Errors}
      Goto binaryRemoved
    ${EndIf}
    Sleep 1000
    IntOp $R2 $R2 + 1
  ${LoopUntil} $R2 >= 10
  Delete /REBOOTOK "$INSTDIR\${BINARY}"
  DetailPrint "${BINARY} was still in use; it will be removed on the next reboot."
  binaryRemoved:

  Delete "$INSTDIR\astap_cli.exe" ; bundled plate solver (no-op if it was never bundled)
  ; Recursive, unlike the install directory below: this subdirectory holds only
  ; the notices the installer wrote.
  RMDir /r "$INSTDIR\THIRD_PARTY_NOTICES"
  Delete "$INSTDIR\${UNINSTALLER}"
  ; Non-recursive: leaves anything unexpected behind rather than eating it.
  ; /REBOOTOK pairs with the binary's deferred delete — the directory cannot be
  ; empty until that has happened.
  RMDir /REBOOTOK "$INSTDIR"

  ; Offer to remove the downloaded star catalogs (fully reconstructible, up to ~1 GB),
  ; unlike config.yaml/state.db which are intentionally preserved. Skipped on a silent
  ; uninstall so unattended removals never block or delete reconstructible-but-large data.
  ${IfNot} ${Silent}
    MessageBox MB_YESNO|MB_ICONQUESTION "Also remove the downloaded star catalogs (plate-solving databases, up to ~1 GB)? They can be re-downloaded later." IDNO skipCatalogs
    DetailPrint "Removing star catalogs from ${DATADIR}\astap..."
    RMDir /r "${DATADIR}\astap"
    skipCatalogs:
  ${EndIf}

  DeleteRegKey HKLM "${UNINSTKEY}"
  DeleteRegKey HKLM "${REGKEY}"
SectionEnd
