; ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
; Windows installer (NSIS 3). Built by build-gui.sh:
;   makensis -DVERSION=0.7.5 -DARCH=amd64 -DSRC=dist-gui/windows-amd64 -DOUT=dist-gui/x.exe ptscry-gui.nsi
; The user chooses: only for me (no administrator, %LOCALAPPDATA%\Programs) or
; for everybody (administrator, Program Files). Start menu entry, optional
; desktop icon and PATH, "Apps and features" entry with the uninstaller.
; The settings (%AppData%\ptscry) are never removed.

Unicode true
SetCompressor /SOLID lzma

!ifndef VERSION
  !define VERSION "0.0.0"
!endif
!ifndef ARCH
  !define ARCH "amd64"
!endif
!ifndef SRC
  !define SRC "."
!endif
!ifndef OUT
  !define OUT "ptscry-gui-setup.exe"
!endif

!define APP "ptscry-gui"
!define PUBLISHER "Guido Cauli <guido.cauli@gmail.com>"
!define UNINSTKEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APP}"

Name "${APP} ${VERSION}"
OutFile "${OUT}"
BrandingText "${APP} - ${PUBLISHER}"
VIProductVersion "${VERSION}.0"
VIAddVersionKey /LANG=0 "ProductName" "${APP}"
VIAddVersionKey /LANG=0 "FileDescription" "${APP} installer - ${PUBLISHER}"
VIAddVersionKey /LANG=0 "CompanyName" "${PUBLISHER}"
VIAddVersionKey /LANG=0 "LegalCopyright" "${PUBLISHER}"
VIAddVersionKey /LANG=0 "FileVersion" "${VERSION}"
VIAddVersionKey /LANG=0 "ProductVersion" "${VERSION}"

; per-user or per-machine, chosen on a page
!define MULTIUSER_EXECUTIONLEVEL Highest
!define MULTIUSER_MUI
!define MULTIUSER_INSTALLMODE_COMMANDLINE
!define MULTIUSER_INSTALLMODE_INSTDIR "${APP}"
!define MULTIUSER_INSTALLMODE_DEFAULT_CURRENTUSER
!if "${ARCH}" != "386"
  !define MULTIUSER_USE_PROGRAMFILES64
!endif
!define MULTIUSER_INSTALLMODE_INSTALL_REGISTRY_KEY "Software\${APP}"
!define MULTIUSER_INSTALLMODE_INSTALL_REGISTRY_VALUENAME "InstallMode"
!define MULTIUSER_INSTALLMODE_UNINSTALL_REGISTRY_KEY "Software\${APP}"
!define MULTIUSER_INSTALLMODE_UNINSTALL_REGISTRY_VALUENAME "InstallMode"
!include MultiUser.nsh
!include MUI2.nsh
!include x64.nsh

!define MUI_ICON "${SRC}\ptscry-gui.ico"
!define MUI_UNICON "${SRC}\ptscry-gui.ico"
!define MUI_ABORTWARNING
!define MUI_FINISHPAGE_RUN "$INSTDIR\ptscry-gui.exe"

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_LICENSE "${SRC}\LICENSE.txt"
!insertmacro MULTIUSER_PAGE_INSTALLMODE
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_COMPONENTS
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

!insertmacro MUI_LANGUAGE "Italian"
!insertmacro MUI_LANGUAGE "English"

LangString SecMain ${LANG_ITALIAN} "ptscry-gui (necessario)"
LangString SecMain ${LANG_ENGLISH} "ptscry-gui (required)"
LangString SecDesk ${LANG_ITALIAN} "Icona sul desktop"
LangString SecDesk ${LANG_ENGLISH} "Desktop icon"
LangString SecPath ${LANG_ITALIAN} "Aggiungi al PATH (ptscry-gui e ptscry dal prompt)"
LangString SecPath ${LANG_ENGLISH} "Add to PATH (ptscry-gui and ptscry from a prompt)"
LangString WrongArch ${LANG_ITALIAN} "Questo installer e' per Windows ${ARCH}."
LangString WrongArch ${LANG_ENGLISH} "This installer is for Windows ${ARCH}."

Function .onInit
  !insertmacro MULTIUSER_INIT
  !insertmacro MUI_LANGDLL_DISPLAY
  !if "${ARCH}" == "arm64"
    ${IfNot} ${IsNativeARM64}
      MessageBox MB_ICONSTOP "$(WrongArch)"
      Abort
    ${EndIf}
  !else if "${ARCH}" == "amd64"
    ${IfNot} ${RunningX64}
      MessageBox MB_ICONSTOP "$(WrongArch)"
      Abort
    ${EndIf}
  !endif
FunctionEnd

Function un.onInit
  !insertmacro MULTIUSER_UNINIT
FunctionEnd

Section "$(SecMain)" SecMainId
  SectionIn RO
  ; a running copy would keep the files locked
  nsExec::Exec 'taskkill /F /IM ptscry-gui.exe'
  SetOutPath "$INSTDIR"
  File "${SRC}\ptscry-gui.exe"
  File "${SRC}\ptscry.exe"
  File "${SRC}\ptscry-gui.ico"
  File /nonfatal "${SRC}\GUIDA-GUI.md"
  File /nonfatal "${SRC}\GUI.md"
  File /nonfatal "${SRC}\README.md"
  File /nonfatal "${SRC}\LICENSE.txt"
  WriteUninstaller "$INSTDIR\uninstall.exe"

  CreateShortCut "$SMPROGRAMS\${APP}.lnk" "$INSTDIR\ptscry-gui.exe" "" "$INSTDIR\ptscry-gui.exe" 0 SW_SHOWNORMAL "" "${APP} - ${PUBLISHER}"

  WriteRegStr SHCTX "${UNINSTKEY}" "DisplayName" "${APP}"
  WriteRegStr SHCTX "${UNINSTKEY}" "DisplayVersion" "${VERSION}"
  WriteRegStr SHCTX "${UNINSTKEY}" "Publisher" "${PUBLISHER}"
  WriteRegStr SHCTX "${UNINSTKEY}" "DisplayIcon" "$INSTDIR\ptscry-gui.exe,0"
  WriteRegStr SHCTX "${UNINSTKEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr SHCTX "${UNINSTKEY}" "UninstallString" '"$INSTDIR\uninstall.exe" /$MultiUser.InstallMode'
  WriteRegStr SHCTX "${UNINSTKEY}" "QuietUninstallString" '"$INSTDIR\uninstall.exe" /$MultiUser.InstallMode /S'
  WriteRegDWORD SHCTX "${UNINSTKEY}" "NoModify" 1
  WriteRegDWORD SHCTX "${UNINSTKEY}" "NoRepair" 1
SectionEnd

Section /o "$(SecDesk)" SecDeskId
  CreateShortCut "$DESKTOP\${APP}.lnk" "$INSTDIR\ptscry-gui.exe" "" "$INSTDIR\ptscry-gui.exe" 0
SectionEnd

Section /o "$(SecPath)" SecPathId
  ; through PowerShell: no plug-in needed and no 1024-character limit
  ${If} $MultiUser.InstallMode == "AllUsers"
    StrCpy $0 "Machine"
  ${Else}
    StrCpy $0 "User"
  ${EndIf}
  nsExec::Exec `powershell -NoProfile -Command "$$s='$0'; $$d='$INSTDIR'; $$p=[Environment]::GetEnvironmentVariable('Path',$$s); if (-not (($$p -split ';') -contains $$d)) { [Environment]::SetEnvironmentVariable('Path', ($$p.TrimEnd(';') + ';' + $$d), $$s) }"`
  WriteRegDWORD SHCTX "Software\${APP}" "AddedToPath" 1
SectionEnd

Section "Uninstall"
  nsExec::Exec 'taskkill /F /IM ptscry-gui.exe'
  ReadRegDWORD $1 SHCTX "Software\${APP}" "AddedToPath"
  ${If} $1 == 1
    ${If} $MultiUser.InstallMode == "AllUsers"
      StrCpy $0 "Machine"
    ${Else}
      StrCpy $0 "User"
    ${EndIf}
    nsExec::Exec `powershell -NoProfile -Command "$$s='$0'; $$d='$INSTDIR'; $$p=[Environment]::GetEnvironmentVariable('Path',$$s); [Environment]::SetEnvironmentVariable('Path', (($$p -split ';' | Where-Object { $$_ -and ($$_.TrimEnd('\') -ine $$d.TrimEnd('\')) }) -join ';'), $$s)"`
  ${EndIf}
  Delete "$SMPROGRAMS\${APP}.lnk"
  Delete "$DESKTOP\${APP}.lnk"
  Delete "$INSTDIR\ptscry-gui.exe"
  Delete "$INSTDIR\ptscry.exe"
  Delete "$INSTDIR\ptscry-gui.ico"
  Delete "$INSTDIR\GUIDA-GUI.md"
  Delete "$INSTDIR\GUI.md"
  Delete "$INSTDIR\README.md"
  Delete "$INSTDIR\LICENSE.txt"
  Delete "$INSTDIR\uninstall.exe"
  RMDir "$INSTDIR"
  DeleteRegKey SHCTX "${UNINSTKEY}"
  DeleteRegKey SHCTX "Software\${APP}"
SectionEnd
