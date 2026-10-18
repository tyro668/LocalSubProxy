// audit3 P2-11.
//
// P2-11: every quit (tray 退出, and on macOS Cmd-Q / Dock / logout, which the
// native side now routes through the 'quit' channel call) runs one graceful
// kernel shutdown bounded by a timeout, then tells the native side
// ('quitReady') that termination may proceed. Was: a hung kernel stop blocked
// quit forever, and Cmd-Q bypassed the kernel stop entirely.
//
// The native halves cannot run under flutter test; their contract is checked
// on the sources (CI builds and packages both runners).
import 'dart:async';
import 'dart:io';

import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:localsubproxy_ui/tray.dart';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();

  test('a native quit request runs the timeout-protected kernel stop, then quitReady, then destroy', () async {
    final m = TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger;
    final calls = <String>[];
    m.setMockMethodCallHandler(const MethodChannel('window_manager'), (c) async {
      calls.add('wm.${c.method}');
      return null;
    });
    m.setMockMethodCallHandler(const MethodChannel('localsubproxy/tray'), (c) async {
      calls.add('tray.${c.method}');
      return null;
    });
    var stops = 0;
    AppTray.stopKernel = () {
      stops++;
      calls.add('kernel.stop');
      return Completer<void>().future; // a kernel that never finishes stopping
    };
    AppTray.quitTimeout = const Duration(milliseconds: 200);
    await AppTray.instance.init();
    // what AppDelegate.applicationShouldTerminate does on Cmd-Q
    const codec = StandardMethodCodec();
    final done = Completer<void>();
    await m.handlePlatformMessage('localsubproxy/tray', codec.encodeMethodCall(const MethodCall('quit')), (_) => done.complete());
    await done.future.timeout(const Duration(seconds: 3), onTimeout: () => fail('quit hung on a kernel that never stops'));
    await AppTray.instance.quit(); // a second quit (tray click meanwhile) shares the run
    expect(stops, 1);
    final i = calls.indexOf('kernel.stop'), r = calls.indexOf('tray.quitReady'), d = calls.indexOf('wm.destroy');
    expect(i >= 0 && r > i && d > r, isTrue, reason: 'order: $calls');
  });

  String src(String p) => File(p).readAsStringSync();

  test('macOS: Cmd-Q / native terminate is deferred to the Dart quit flow with a fallback', () {
    final s = src('macos/Runner/AppDelegate.swift');
    expect(s, contains('applicationShouldTerminate'));
    expect(s, contains('.terminateLater'));
    expect(s, contains('invokeMethod("quit"'));
    expect(s, contains('case "quitReady"'));
    expect(s, contains('reply(toApplicationShouldTerminate: true)'));
    expect(s, contains('asyncAfter'), reason: 'a hung Dart side must not block termination forever');
  });
}
