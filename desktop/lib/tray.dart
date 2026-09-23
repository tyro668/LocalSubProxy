import 'dart:io';

import 'package:flutter/services.dart';
import 'package:window_manager/window_manager.dart';

import 'native_api.dart';

/// 系统托盘（macOS/Windows 原生实现，经 MethodChannel 接收菜单事件）：
/// 托盘菜单提供「配置 / 退出」；「配置」显示并聚焦主窗口，「退出」先停止本机内核再终止进程。
/// 窗口关闭按钮被拦截为隐藏到托盘，只有通过托盘「退出」才会真正退出。
class AppTray with WindowListener {
  AppTray._();
  static final AppTray instance = AppTray._();

  static const _channel = MethodChannel('localsubproxy/tray');
  bool _initialized = false;
  Future<void> _closeFuture = Future.value();

  Future<void> init() async {
    if (_initialized) return;
    _initialized = true;
    _channel.setMethodCallHandler((call) async {
      switch (call.method) {
        case 'config':
          await showConfig();
        case 'quit':
          await quit();
      }
    });
    windowManager.addListener(this);
    await windowManager.setPreventClose(true);
  }

  Future<void> showConfig() async {
    await _closeFuture;
    if (Platform.isMacOS) {
      await _channel.invokeMethod<void>('showDock');
    }
    await windowManager.show();
    await windowManager.focus();
  }

  Future<void> quit() async {
    await _closeFuture;
    try {
      NativeApi.instance.stop();
    } catch (_) {}
    await windowManager.setPreventClose(false);
    await windowManager.destroy();
  }

  @override
  void onWindowClose() {
    _closeFuture = _hideConfig();
  }

  Future<void> _hideConfig() async {
    await windowManager.hide();
    if (Platform.isMacOS) {
      await _channel.invokeMethod<void>('hideDock');
    }
  }
}
