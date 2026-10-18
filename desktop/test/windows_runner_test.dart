// audit3 P2-10 (Windows runner): Explorer broadcasts TaskbarCreated when it
// restarts and every notification icon is gone; the tray-only app must add
// its icon again. A second launch must not start a second kernel and tray
// icon: it activates the running instance and exits. The runner cannot run
// under flutter test; its contract is checked on the source (the CI Windows
// job compiles and packages it).
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';

void main() {
  String src(String p) => File(p).readAsStringSync();

  test('Windows: the tray icon is re-added on TaskbarCreated; a second launch activates the first', () {
    final s = src('windows/runner/main.cpp');
    expect(s, contains('RegisterWindowMessageW(L"TaskbarCreated")'));
    final h = s.indexOf('message == kTaskbarCreatedMessage');
    expect(h, greaterThan(0));
    expect(s.substring(h, h + 200), contains('ShellAddTrayIcon(window)'));
    expect(s, contains('CreateMutexW'));
    expect(s, contains('ERROR_ALREADY_EXISTS'));
    expect(s, contains('PostMessageW(HWND_BROADCAST, kActivateMessage'));
    final a = s.indexOf('message == kActivateMessage');
    expect(a, greaterThan(0));
    expect(s.substring(a, a + 400), contains('SetForegroundWindow'));
  });
}
