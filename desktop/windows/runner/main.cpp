#include <flutter/dart_project.h>
#include <flutter/flutter_view_controller.h>
#include <flutter/method_channel.h>
#include <flutter/standard_method_codec.h>
#include <windows.h>
#include <shellapi.h>

#include <memory>
#include <string>

#include "flutter_window.h"
#include "resource.h"
#include "utils.h"

namespace {
constexpr UINT kTrayCallbackMessage = WM_APP + 1;
constexpr UINT kTrayIconId = 1;
constexpr WORD kTrayCmdConfig = 1001;
constexpr WORD kTrayCmdQuit = 1002;

WNDPROC original_wnd_proc = nullptr;
std::unique_ptr<flutter::MethodChannel<flutter::EncodableValue>> tray_channel;

// Explorer broadcasts TaskbarCreated when it (re)starts: every notification
// icon is gone and must be added again, or the tray-only app becomes
// unreachable (audit3 P2-10).
const UINT kTaskbarCreatedMessage = ::RegisterWindowMessageW(L"TaskbarCreated");
// A second launch asks the running instance to show its window, then exits.
const UINT kActivateMessage =
    ::RegisterWindowMessageW(L"LocalSubProxy.Desktop.Activate");
constexpr wchar_t kSingleInstanceMutex[] =
    L"Local\\LocalSubProxy.Desktop.SingleInstance";

void ShellAddTrayIcon(HWND window);

void RemoveTrayIcon(HWND window) {
  NOTIFYICONDATAW nid = {};
  nid.cbSize = sizeof(nid);
  nid.hWnd = window;
  nid.uID = kTrayIconId;
  ::Shell_NotifyIconW(NIM_DELETE, &nid);
}

// 托盘消息处理：子类化主窗口，点击托盘图标弹出「配置 / 退出」菜单，
// 菜单选择经 MethodChannel 转发给 Dart 侧处理。
LRESULT CALLBACK TrayWndProc(HWND window, UINT message, WPARAM wparam,
                             LPARAM lparam) {
  if (kTaskbarCreatedMessage != 0 && message == kTaskbarCreatedMessage) {
    ShellAddTrayIcon(window);  // Explorer restarted: re-add the icon
    return 0;
  }
  if (kActivateMessage != 0 && message == kActivateMessage) {
    // another launch: show and focus the existing configuration window
    if (::IsIconic(window)) {
      ::ShowWindow(window, SW_RESTORE);
    }
    ::SetForegroundWindow(window);
    if (tray_channel) {
      tray_channel->InvokeMethod("config", nullptr);
    }
    return 0;
  }
  switch (message) {
    case kTrayCallbackMessage:
      if (LOWORD(lparam) == WM_LBUTTONUP || LOWORD(lparam) == WM_RBUTTONUP) {
        POINT cursor;
        ::GetCursorPos(&cursor);
        HMENU menu = ::CreatePopupMenu();
        ::AppendMenuW(menu, MF_STRING, kTrayCmdConfig, L"配置");
        ::AppendMenuW(menu, MF_SEPARATOR, 0, nullptr);
        ::AppendMenuW(menu, MF_STRING, kTrayCmdQuit, L"退出");
        ::SetForegroundWindow(window);
        ::TrackPopupMenu(menu, TPM_BOTTOMALIGN | TPM_LEFTALIGN, cursor.x,
                         cursor.y, 0, window, nullptr);
        ::PostMessage(window, WM_NULL, 0, 0);
        ::DestroyMenu(menu);
        return 0;
      }
      break;
    case WM_COMMAND:
      switch (LOWORD(wparam)) {
        case kTrayCmdConfig:
          tray_channel->InvokeMethod("config", nullptr);
          return 0;
        case kTrayCmdQuit:
          tray_channel->InvokeMethod("quit", nullptr);
          return 0;
      }
      break;
    case WM_NCDESTROY:
      RemoveTrayIcon(window);
      break;
  }
  return ::CallWindowProcW(original_wnd_proc, window, message, wparam, lparam);
}

void ShellAddTrayIcon(HWND window) {
  NOTIFYICONDATAW nid = {};
  nid.cbSize = sizeof(nid);
  nid.hWnd = window;
  nid.uID = kTrayIconId;
  nid.uFlags = NIF_MESSAGE | NIF_ICON | NIF_TIP | NIF_SHOWTIP;
  nid.uCallbackMessage = kTrayCallbackMessage;
  nid.hIcon = static_cast<HICON>(::LoadImageW(
      ::GetModuleHandleW(nullptr), MAKEINTRESOURCEW(IDI_APP_ICON), IMAGE_ICON,
      32, 32, LR_DEFAULTCOLOR | LR_SHARED));
  if (nid.hIcon == nullptr) {
    nid.hIcon = ::LoadIconW(nullptr, IDI_APPLICATION);
  }
  ::lstrcpyW(nid.szTip, L"LocalSubProxy");
  ::Shell_NotifyIconW(NIM_ADD, &nid);
}

void AddTrayIcon(HWND window) {
  original_wnd_proc = reinterpret_cast<WNDPROC>(::SetWindowLongPtrW(
      window, GWLP_WNDPROC, reinterpret_cast<LONG_PTR>(TrayWndProc)));
  // TaskbarCreated and the activation request are broadcast messages; a
  // window of an elevated process would not get them from Explorer or a
  // normal-integrity launch otherwise.
  ::ChangeWindowMessageFilterEx(window, kTaskbarCreatedMessage, MSGFLT_ALLOW,
                                nullptr);
  ::ChangeWindowMessageFilterEx(window, kActivateMessage, MSGFLT_ALLOW,
                                nullptr);
  ShellAddTrayIcon(window);
}
}  // namespace

int APIENTRY wWinMain(_In_ HINSTANCE instance, _In_opt_ HINSTANCE prev,
                      _In_ wchar_t *command_line, _In_ int show_command) {
  // Attach to console when present (e.g., 'flutter run') or create a
  // new console when running with a debugger.
  if (!::AttachConsole(ATTACH_PARENT_PROCESS) && ::IsDebuggerPresent()) {
    CreateAndAttachConsole();
  }

  // Single instance (audit3 P2-10): a second launch would start a second
  // kernel on the same data dir and a second tray icon. It asks the running
  // instance to show its window instead, and exits.
  HANDLE instance_mutex =
      ::CreateMutexW(nullptr, TRUE, kSingleInstanceMutex);
  if (instance_mutex != nullptr && ::GetLastError() == ERROR_ALREADY_EXISTS) {
    ::AllowSetForegroundWindow(ASFW_ANY);
    ::PostMessageW(HWND_BROADCAST, kActivateMessage, 0, 0);
    ::CloseHandle(instance_mutex);
    return EXIT_SUCCESS;
  }

  // Initialize COM, so that it is available for use in the library and/or
  // plugins.
  ::CoInitializeEx(nullptr, COINIT_APARTMENTTHREADED);

  flutter::DartProject project(L"data");

  std::vector<std::string> command_line_arguments =
      GetCommandLineArguments();

  project.set_dart_entrypoint_arguments(std::move(command_line_arguments));

  FlutterWindow window(project);
  Win32Window::Point origin(10, 10);
  Win32Window::Size size(1280, 800);
  if (!window.Create(L"", origin, size)) {
    return EXIT_FAILURE;
  }
  window.SetQuitOnClose(true);

  tray_channel =
      std::make_unique<flutter::MethodChannel<flutter::EncodableValue>>(
          window.view_controller()->engine()->messenger(), "localsubproxy/tray",
          &flutter::StandardMethodCodec::GetInstance());
  AddTrayIcon(window.GetHandle());

  ::MSG msg;
  while (::GetMessage(&msg, nullptr, 0, 0)) {
    ::TranslateMessage(&msg);
    ::DispatchMessage(&msg);
  }

  tray_channel.reset();
  ::CoUninitialize();
  if (instance_mutex != nullptr) {
    ::ReleaseMutex(instance_mutex);
    ::CloseHandle(instance_mutex);
  }
  return EXIT_SUCCESS;
}
