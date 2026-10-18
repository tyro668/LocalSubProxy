; Inno Setup 6 安装脚本（UTF-8 带 BOM，快捷方式名含中文）。由 scripts/build-windows.ps1 -Package 调用：
;   iscc /DAppVersion=1.2.3 /DAppVersionNumeric=1.2.3 /DSourceDir=<Release目录> /DOutputDir=<dist> Avora.iss
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
AppName=Avora
AppVersion={#AppVersion}
VersionInfoVersion={#AppVersionNumeric}
AppPublisher=dev.avora
DefaultDirName={autopf}\Avora
DefaultGroupName=Avora
DisableProgramGroupPage=yes
PrivilegesRequired=lowest
PrivilegesRequiredOverridesAllowed=dialog
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
OutputDir={#OutputDir}
OutputBaseFilename=Avora-{#AppVersion}-windows-x64-setup
SetupIconFile={#SourceDir}\..\..\..\..\..\windows\runner\resources\app_icon.ico
UninstallDisplayIcon={app}\Avora.exe
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
CloseApplications=yes
; 改名兼容：AppId 不变，从 LocalSubProxy 2.0.x 升级时替换原卸载项；
; 新版装到 Avora 目录，并删除旧的默认安装目录和快捷方式（见 [InstallDelete]）
UsePreviousAppDir=no

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"; Flags: unchecked

[Files]
Source: "{#SourceDir}\*"; DestDir: "{app}"; Flags: ignoreversion recursesubdirs createallsubdirs

[InstallDelete]
; 改名兼容（保留一个版本）：LocalSubProxy 2.0.x 的默认安装目录与快捷方式
Type: filesandordirs; Name: "{autopf}\LocalSubProxy"
Type: files; Name: "{autoprograms}\LocalSubProxy.lnk"
Type: files; Name: "{autodesktop}\LocalSubProxy.lnk"

[Icons]
Name: "{autoprograms}\Avora"; Filename: "{app}\Avora.exe"
Name: "{autodesktop}\Avora"; Filename: "{app}\Avora.exe"; Tasks: desktopicon

[Run]
Filename: "{app}\Avora.exe"; Description: "{cm:LaunchProgram,Avora}"; Flags: nowait postinstall skipifsilent

[Code]
// 2.1.1 改名兼容（保留一个版本）：2.1.0 的开始菜单/桌面快捷方式使用中文名（U+4E00 U+7EBF U+7275），升级时删除
procedure CurStepChanged(CurStep: TSetupStep);
var
  OldName: String;
begin
  if CurStep = ssInstall then
  begin
    OldName := #$4E00#$7EBF#$7275 + '.lnk';
    DeleteFile(ExpandConstant('{autoprograms}\') + OldName);
    DeleteFile(ExpandConstant('{autodesktop}\') + OldName);
  end;
end;
