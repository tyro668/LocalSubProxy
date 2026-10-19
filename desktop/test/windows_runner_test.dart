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

  // v2.1.2: the tray menu read 'é…ç½®' / 'é€€å‡º'. main.cpp is UTF-8 (no
  // BOM) and has L"配置" / L"退出"; without /utf-8 MSVC reads the source in
  // the build machine's ANSI code page (1252 on the runner), so each UTF-8
  // byte became its own wchar_t. The CI Windows job also checks the built
  // Avora.exe for the UTF-16 strings.
  test('Windows: native sources are compiled as UTF-8 (/utf-8)', () {
    final cmake = src('windows/CMakeLists.txt');
    final start = cmake.indexOf('function(APPLY_STANDARD_SETTINGS');
    final end = cmake.indexOf('endfunction()', start);
    expect(start, greaterThanOrEqualTo(0));
    expect(cmake.substring(start, end), contains('/utf-8'),
        reason: 'APPLY_STANDARD_SETTINGS must pass /utf-8 to MSVC');
    final nonAscii = RegExp(r'[^\x00-\x7F]');
    final withText = Directory('windows/runner')
        .listSync()
        .whereType<File>()
        .where((f) => RegExp(r'\.(cpp|h)$').hasMatch(f.path))
        .where((f) => nonAscii.hasMatch(f.readAsStringSync()
            .split('\n')
            .where((l) => !l.trimLeft().startsWith('//'))
            .join('\n')))
        .map((f) => f.path.split(Platform.pathSeparator).last)
        .toList();
    expect(withText, contains('main.cpp'),
        reason: 'the tray menu text is in main.cpp; this test guards it');
  });
}
