import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:avora_ui/main.dart';
import 'package:avora_ui/native_api.dart';
import 'package:avora_ui/status_contract.dart';
import 'package:shadcn_flutter/shadcn_flutter.dart';

import 'screens_test.dart' as screens;

// New logic of the UI redesign: 4-colour status bar aggregation, Chinese
// state labels (never the raw kernel string), kernel error hints, and the
// ⋮ menu → destructive confirm → delete_adapter flow.

DesktopStatus st(List<Map<String, dynamic>> cs, {bool stale = false}) =>
    DesktopStatus.parse({
      'paired': cs.isNotEmpty,
      'connections': cs,
      if (stale) 'stale': true,
    });

Map<String, dynamic> conn(String state) => screens.c(state, state, state);

void main() {
  group('barStatus: green only when every connection is online', () {
    final cases = <String, (DesktopStatus, StatusLevel, String)>{
      'all online': (
        st([conn('online'), conn('online')]),
        StatusLevel.ok,
        '全部在线 2/2',
      ),
      'partial + error': (
        st([conn('online'), conn('error'), conn('paused')]),
        StatusLevel.warn,
        '部分在线 1/3 · 1 个错误',
      ),
      'partial, no error': (
        st([conn('online'), conn('paused')]),
        StatusLevel.warn,
        '部分在线 1/2',
      ),
      'none online, errors': (
        st([conn('error'), conn('revoked')]),
        StatusLevel.error,
        '连接异常 0/2 · 2 个错误',
      ),
      'connecting': (st([conn('connecting')]), StatusLevel.warn, '连接中 0/1'),
      'all paused': (st([conn('paused')]), StatusLevel.off, '未连接 0/1'),
      'no connections': (st([]), StatusLevel.off, '未连接'),
      'stale': (st([conn('online')], stale: true), StatusLevel.off, '状态未知'),
      'legacy connected': (
        DesktopStatus.parse(const {'paired': true, 'connected': true}),
        StatusLevel.ok,
        '已连接',
      ),
    };
    cases.forEach((name, c) {
      test(name, () {
        final (ds, level, label) = c;
        final b = barStatus(ds);
        expect(b.level, level);
        expect(b.label, label);
      });
    });
  });

  test('state labels are Chinese, never the raw kernel state', () {
    ConnectionInfo info(
      String state, {
      bool connected = false,
      bool paused = false,
    }) => ConnectionInfo.fromJson({
      'id': 'x',
      'state': state,
      'connected': connected,
      'paused': paused,
    });
    expect(info('online', connected: true).stateLabel, '在线');
    expect(info('connecting').stateLabel, '连接中');
    expect(info('paused', paused: true).stateLabel, '已暂停');
    expect(info('disconnected', paused: true).stateLabel, '已暂停');
    expect(info('disconnected').stateLabel, '已断开');
    expect(info('revoked').stateLabel, '已吊销');
    expect(info('error').stateLabel, '错误');
    for (final raw in ['handshaking', 'draining', 'weird_state']) {
      final l = info(raw).stateLabel;
      expect(l, isNot(raw));
      expect(l, '状态未知');
      expect(RegExp(r'[a-z_]').hasMatch(l), isFalse);
    }
    // green / online only with a live session
    expect(info('online').level, isNot(StatusLevel.ok));
    expect(info('online', connected: true).level, StatusLevel.ok);
  });

  test('kernel start errors: cleaned text and a human hint', () {
    expect(
      cleanError(StateError('AVORA_Start: open state.db: database is locked')),
      'AVORA_Start: open state.db: database is locked',
    );
    expect(cleanError(Exception('boom')), 'boom');
    expect(kernelErrorHint('open x: database is locked')!.title, '本机数据库被占用');
    expect(
      kernelErrorHint(
        'data directory in use: /d is used by another Avora process',
      )!.title,
      contains('另一个一线牵'),
    );
    // 改名兼容：旧版应用仍在运行时给出专门提示（内核 ErrLegacyDirInUse 的原文）
    expect(
      kernelErrorHint(
        'data directory in use: the old LocalSubProxy app (or "localsubproxy serve") is still running; quit it and start Avora again so its connections can be moved over',
      )!.title,
      '旧版 LocalSubProxy 仍在运行',
    );
    expect(kernelErrorHint('mkdir: permission denied')!.title, contains('权限'));
    expect(kernelErrorHint('write: no space left on device')!.title, '磁盘空间不足');
    expect(kernelErrorHint('something else'), isNull);
  });

  testWidgets('⋮ menu → 删除方案 → destructive confirm → delete_adapter', (
    t,
  ) async {
    final kernel = screens.DesignKernel(
      status: screens.designStatus(),
      adapters: screens.designAdapters(),
    );
    NativeApi.debugTransport = kernel.call;
    addTearDown(() => NativeApi.debugTransport = null);
    final m = TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger;
    for (final ch in ['window_manager', 'avora/tray']) {
      m.setMockMethodCallHandler(MethodChannel(ch), (_) async => null);
    }
    t.view.physicalSize = const Size(1200, 780);
    t.view.devicePixelRatio = 1;
    addTearDown(t.view.resetPhysicalSize);
    addTearDown(t.view.resetDevicePixelRatio);
    await t.pumpWidget(const ProxyApp());
    await t.pumpAndSettle();

    Future<void> steps() async {
      for (var i = 0; i < 6; i++) {
        await t.pump(const Duration(milliseconds: 100));
      }
    }

    await t.tap(find.byKey(const ValueKey('scheme-menu-deepseek')));
    await steps();
    await t.tap(find.byKey(const ValueKey('menu-delete')));
    await t.pumpAndSettle();
    // the confirm names the scheme and the connections that lose access
    expect(find.byKey(const ValueKey('app-dialog')), findsOneWidget);
    expect(
      find.textContaining('deepseek-chat', findRichText: true),
      findsWidgets,
    );
    expect(find.textContaining('Acme 智能客服', findRichText: true), findsWidgets);
    expect(kernel.calls, isNot(contains('delete_adapter')));

    // 取消 keeps the scheme
    await t.tap(find.text('取消'));
    await t.pumpAndSettle();
    expect(kernel.calls, isNot(contains('delete_adapter')));
    expect(find.byKey(const ValueKey('scheme-row-deepseek')), findsOneWidget);

    await t.tap(find.byKey(const ValueKey('scheme-menu-deepseek')));
    await steps();
    await t.tap(find.byKey(const ValueKey('menu-delete')));
    await t.pumpAndSettle();
    await t.tap(find.widgetWithText(AppButton, '删除方案'));
    await t.pumpAndSettle();
    expect(kernel.calls, contains('delete_adapter'));
    expect(find.byKey(const ValueKey('scheme-row-deepseek')), findsNothing);
    await t.pump(const Duration(seconds: 6));
    await t.pumpWidget(const SizedBox());
    await t.pump(const Duration(seconds: 3));
  }, variant: TargetPlatformVariant.only(TargetPlatform.macOS));

  testWidgets('cleanup banner: count, details with raw text, dismiss', (
    t,
  ) async {
    Widget page(Map<String, dynamic> s) => ShadcnApp(
      home: AppShell(status: s, child: const Text('body')),
    );
    await t.pumpWidget(
      page({
        'connections': [],
        'warning': 'remove bindings/a: permission denied',
        'cleanup_pending': ['bindings/a', 'bindings/b'],
      }),
    );
    expect(find.textContaining('有 2 个已撤销连接'), findsOneWidget);
    expect(find.textContaining('permission denied'), findsNothing);
    await t.tap(find.byKey(const ValueKey('cleanup-warning-details')));
    await t.pumpAndSettle();
    expect(find.textContaining('permission denied'), findsOneWidget);
    await t.tap(find.text('知道了'));
    await t.pumpAndSettle();
    await t.tap(find.byKey(const ValueKey('cleanup-warning-close')));
    await t.pump();
    expect(find.byKey(const ValueKey('cleanup-warning')), findsNothing);
    expect(find.text('body'), findsOneWidget);
  });
}
