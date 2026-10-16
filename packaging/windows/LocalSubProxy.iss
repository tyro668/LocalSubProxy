; Inno Setup 6 安装脚本。由 scripts/build-windows.ps1 -Package 调用：
;   iscc /DAppVersion=1.2.3 /DAppVersionNumeric=1.2.3 /DSourceDir=<Release目录> /DOutputDir=<dist> LocalSubProxy.iss
#ifndef AppVersion
  #define AppVersion "0.0.0"
#endif
#ifndef AppVersionNumeric
  #define AppVersionNumeric AppVersion
#endif
#ifndef SourceDir
  #define SourceDir "..\..\desktop\build\windows\x64\runner\Release"
#endif
#ifndef OutputDir
  #define OutputDir "..\..\dist"
#endif

[Setup]
AppId={{6F1D2B57-3C4A-4E8B-9A51-0D7C2E9F4B13}
AppName=LocalSubProxy
AppVersion={#AppVersion}
VersionInfoVersion={#AppVersionNumeric}
AppPublisher=dev.localsubproxy
DefaultDirName={autopf}\LocalSubProxy
DefaultGroupName=LocalSubProxy
DisableProgramGroupPage=yes
PrivilegesRequired=lowest
PrivilegesRequiredOverridesAllowed=dialog
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
OutputDir={#OutputDir}
OutputBaseFilename=LocalSubProxy-{#AppVersion}-windows-x64-setup
SetupIconFile={#SourceDir}\..\..\..\..\..\windows\runner\resources\app_icon.ico
UninstallDisplayIcon={app}\LocalSubProxy.exe
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
CloseApplications=yes

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"; Flags: unchecked

[Files]
Source: "{#SourceDir}\*"; DestDir: "{app}"; Flags: ignoreversion recursesubdirs createallsubdirs

[Icons]
Name: "{autoprograms}\LocalSubProxy"; Filename: "{app}\LocalSubProxy.exe"
Name: "{autodesktop}\LocalSubProxy"; Filename: "{app}\LocalSubProxy.exe"; Tasks: desktopicon

[Run]
Filename: "{app}\LocalSubProxy.exe"; Description: "{cm:LaunchProgram,LocalSubProxy}"; Flags: nowait postinstall skipifsilent
