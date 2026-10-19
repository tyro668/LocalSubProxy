import 'dart:io';
import 'dart:ui' as ui;

import 'package:flutter/rendering.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:avora_ui/main.dart';
import 'package:avora_ui/native_api.dart';
import 'package:shadcn_flutter/shadcn_flutter.dart';

import 'connection_dialog_scroll_test.dart' as conn;

// Screenshot tests for the 14 redesigned screens (docs: 客户端设计稿说明),
// driven through the real HomePage with the kernel replaced by a fake that
// serves the same sample data as the mockups. Each screen asserts its key
// structure and that no exception / overflow happened. With UI_SHOTS_DIR set
// the screens are also written as PNGs (1200x780) for visual comparison;
// pixel goldens are not committed because CJK font rendering differs per OS.

const device = 'MacBook-Pro-张伟';

List<Map<String, dynamic>> designAdapters() => [
  {
    'id': 'codex',
    'name': 'gpt-5-codex',
    'type': 'codex_app_server',
    'codex_bin': 'codex',
    'models': ['gpt-5-codex'],
    'reasoning_effort': 'medium',
    'enabled': true,
    'is_default': true,
    'health': 'ready',
  },
  {
    'id': 'deepseek',
    'name': 'deepseek-chat',
    'type': 'openai_compatible',
    'base_url': 'https://api.deepseek.com/v1',
    'models': ['deepseek-chat'],
    'context_window': 65536,
    'max_output_tokens': 8192,
    'enabled': true,
    'has_api_key': true,
    'health': 'ready',
  },
  {
    'id': 'claude',
    'name': 'claude-sonnet-4-5',
    'type': 'anthropic_compatible',
    'base_url': 'https://api.anthropic.com',
    'models': ['claude-sonnet-4-5'],
    'enabled': true,
    'has_api_key': true,
    'health': 'unavailable',
  },
  {
    'id': 'qwen',
    'name': 'qwen-max_2',
    'type': 'openai_compatible',
    'base_url': 'https://dashscope.aliyuncs.com/compatible-mode/v1',
    'models': ['qwen-max'],
    'enabled': false,
    'has_api_key': true,
  },
];

Map<String, dynamic> c(
  String id,
  String name,
  String state, {
  List<String> allow = const ['*'],
  int tasks = 0,
  String error = '',
}) => {
  'id': id,
  'saas_name': name,
  'tenant': '$id-prod',
  'state': state,
  'connected': state == 'online',
  'paused': state == 'paused',
  'remote_url': 'https://$id.example.com',
  'server_url': 'https://gw.$id.example.com',
  'running_tasks': tasks,
  'max_concurrency': 4,
  'allow': allow,
  'transport': 'wss',
  'e2ee': true,
  if (error.isNotEmpty) 'last_error': error,
};

Map<String, dynamic> designStatus() => {
  'paired': true,
  'connected': true,
  'device_name': device,
  'running_tasks': 3,
  'connections': [
    c(
      'acme',
      'Acme 智能客服',
      'online',
      allow: ['codex/gpt-5-codex', 'deepseek/deepseek-chat'],
      tasks: 1,
    ),
    c('globex', 'Globex 研发平台', 'paused'),
    c('initech', 'Initech 文档助手', 'error', error: '网关已吊销此设备令牌（401）'),
    c('umbrella', 'Umbrella BI', 'connecting'),
  ],
};

Map<String, dynamic> emptyStatus() => {
  'paired': false,
  'connected': false,
  'device_name': device,
  'running_tasks': 0,
  'connections': <Map<String, dynamic>>[],
};

class DesignKernel {
  DesignKernel({required this.status, required this.adapters});
  Map<String, dynamic> status;
  List<Map<String, dynamic>> adapters;
  Object? startError;
  final calls = <String>[];

  dynamic call(Map<String, dynamic> p) {
    final a = p['action'] as String;
    calls.add(a);
    switch (a) {
      case 'start':
        if (startError != null) throw StateError('$startError');
        return <String, dynamic>{};
      case 'status':
        return status;
      case 'list_adapters':
        return {'adapters': adapters};
      case 'usage_stats':
        return {
          'buckets': [
            {
              'adapter': 'codex',
              'prompt_tokens': 1280000,
              'completion_tokens': 210400,
              'total_tokens': 1490400,
            },
            {
              'adapter': 'deepseek',
              'prompt_tokens': 402100,
              'completion_tokens': 88200,
              'total_tokens': 490300,
            },
            {
              'adapter': 'claude',
              'prompt_tokens': 9200,
              'completion_tokens': 1600,
              'total_tokens': 10800,
            },
          ],
        };
      case 'test_adapter':
        final body = (p['adapter'] as Map?) ?? const {};
        return {
          'health': 'ready',
          'models': body['type'] == 'openai_compatible'
              ? ['deepseek-chat', 'deepseek-reasoner']
              : <String>[],
        };
      case 'pair_prepare':
        return {
          'state': 'consent',
          'pair_id': 'p1',
          'consent': {
            'saas': {
              'name': 'Acme 智能客服',
              'homepage_uri': 'https://acme.example.com',
              'policy_uri': 'https://acme.example.com/privacy',
            },
            'gateway': 'https://gw1.example.com',
            'tenant': 'acme-prod',
            'requested_scopes': ['chat.completions', 'models.list'],
            'gateway_key_id': 'gk_2026_10_a1f3',
            'verified': true,
          },
          'available_models': [
            'codex/gpt-5-codex',
            'deepseek/deepseek-chat',
            'claude/claude-sonnet-4-5',
          ],
        };
      case 'pair_approve':
        return {
          'user_code': 'K7QF-3H9X',
          'link_url': 'https://acme.example.com/link',
        };
      case 'pair_status':
        return {'state': 'pending'};
      case 'delete_adapter':
        adapters = [
          for (final x in adapters)
            if (x['id'] != p['id']) x,
        ];
        return null;
      default:
        return <String, dynamic>{};
    }
  }
}

Finder dialogs() => find.byKey(const ValueKey('app-dialog'));

// desktop popovers (the ⋮ menu is a bottom sheet on mobile platforms)
final desktop = TargetPlatformVariant.only(TargetPlatform.macOS);

void main() {
  setUpAll(conn.loadFonts);
  late DesignKernel kernel;

  setUp(() {
    kernel = DesignKernel(status: designStatus(), adapters: designAdapters());
    NativeApi.debugTransport = kernel.call;
    final m = TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger;
    for (final ch in ['window_manager', 'avora/tray']) {
      m.setMockMethodCallHandler(MethodChannel(ch), (_) async => null);
    }
  });
  tearDown(() => NativeApi.debugTransport = null);

  Future<void> pumpApp(WidgetTester t, {bool dark = false}) async {
    t.view.physicalSize = const Size(1200, 780);
    t.view.devicePixelRatio = 1;
    t.platformDispatcher.platformBrightnessTestValue = dark
        ? Brightness.dark
        : Brightness.light;
    addTearDown(t.view.resetPhysicalSize);
    addTearDown(t.view.resetDevicePixelRatio);
    addTearDown(t.platformDispatcher.clearPlatformBrightnessTestValue);
    await t.pumpWidget(const ProxyApp());
    await t.pumpAndSettle();
  }

  Future<void> shot(WidgetTester t, String name) async {
    expect(t.takeException(), isNull, reason: '$name: no exception/overflow');
    final dir = Platform.environment['UI_SHOTS_DIR'];
    if (dir == null) return;
    // decode asset images (the logo) before capturing
    for (final e in find.byType(Image).evaluate()) {
      await t.runAsync(() => precacheImage((e.widget as Image).image, e));
    }
    await t.pump();
    final ro = t.renderObject<RenderRepaintBoundary>(
      find.byType(RepaintBoundary).first,
    );
    final png = await t.runAsync(() async {
      final i = await ro.toImage();
      return (await i.toByteData(format: ui.ImageByteFormat.png))!;
    });
    File('$dir/$name.png')
      ..createSync(recursive: true)
      ..writeAsBytesSync(png!.buffer.asUint8List());
  }

  Future<void> unmount(WidgetTester t) async {
    await t.pump(const Duration(seconds: 6)); // let toasts expire
    await t.pumpWidget(const SizedBox());
    await t.pump(const Duration(seconds: 3));
  }

  Future<void> tap(WidgetTester t, Finder f) async {
    await t.tap(f);
    await t.pumpAndSettle();
  }

  // an open desktop popover keeps scheduling frames: pump fixed steps
  Future<void> tapMenu(WidgetTester t, Finder f) async {
    await t.tap(f);
    for (var i = 0; i < 6; i++) {
      await t.pump(const Duration(milliseconds: 100));
    }
  }

  testWidgets('01 main empty', (t) async {
    kernel
      ..adapters = []
      ..status = emptyStatus();
    await pumpApp(t);
    expect(find.byKey(const ValueKey('empty-add-scheme')), findsOneWidget);
    expect(find.byKey(const ValueKey('status-pill-off')), findsOneWidget);
    expect(find.text('立即连接 ›'), findsOneWidget);
    await shot(t, '01_main_empty');
    await unmount(t);
  }, variant: desktop);

  testWidgets('02 main list + detail', (t) async {
    await pumpApp(t);
    // record a 最近测试 for the selected scheme via its ⋮ menu
    await tapMenu(t, find.byKey(const ValueKey('scheme-menu-codex')));
    await tap(t, find.byKey(const ValueKey('menu-test')));
    expect(find.byKey(const ValueKey('scheme-row-codex')), findsOneWidget);
    expect(find.text('方案详情'), findsOneWidget);
    expect(find.text('默认'), findsOneWidget);
    expect(find.text('不可用'), findsWidgets);
    expect(find.text('已停用'), findsOneWidget);
    expect(find.text('部分在线 1/4 · 1 个错误'), findsOneWidget);
    expect(find.text('3 个任务'), findsOneWidget);
    expect(find.text('1.49M tokens'), findsOneWidget);
    await shot(t, '02_main_list_detail');
    await unmount(t);
  }, variant: desktop);

  testWidgets('03 connections + 04 models', (t) async {
    await pumpApp(t);
    await tap(t, find.byKey(const ValueKey('status-pill-warn')));
    expect(find.text('远端服务连接'), findsOneWidget);
    expect(find.textContaining('认证失败', findRichText: true), findsOneWidget);
    expect(find.text('已暂停'), findsOneWidget);
    expect(find.text('连接中'), findsWidgets);
    expect(find.text('在线'), findsOneWidget);
    await shot(t, '03_connections');
    await tap(
      t,
      find.descendant(of: dialogs(), matching: find.text('模型')).first,
    );
    expect(dialogs(), findsNWidgets(2));
    expect(find.text('可用模型'), findsOneWidget);
    expect(find.text('已选 2 / 5'), findsNothing);
    await shot(t, '04_connection_models');
    await unmount(t);
  }, variant: desktop);

  testWidgets('05-07 pairing steps', (t) async {
    kernel.status = emptyStatus();
    await pumpApp(t);
    await tap(t, find.byKey(const ValueKey('status-pill-off')));
    expect(find.text('输入地址'), findsOneWidget);
    await t.enterText(find.byType(TextField).last, 'https://acme.example.com');
    await t.pumpAndSettle();
    await shot(t, '05_pair_step1_input');
    await tap(t, find.text('连接').last);
    expect(find.text('「Acme 智能客服」请求使用本机模型'), findsOneWidget);
    expect(find.text('签名已校验'), findsOneWidget);
    await tap(t, find.byKey(const ValueKey('model-codex/gpt-5-codex')));
    await tap(t, find.byKey(const ValueKey('model-deepseek/deepseek-chat')));
    expect(find.textContaining('已选 2 / 3'), findsOneWidget);
    await shot(t, '06_pair_step2_consent');
    await tap(t, find.text('同意并配对'));
    expect(find.byKey(const ValueKey('pair-code')), findsOneWidget);
    expect(find.text('K7QF-3H9X'), findsOneWidget);
    await shot(t, '07_pair_step3_code');
    await unmount(t);
  }, variant: desktop);

  testWidgets('08 edit openai tested', (t) async {
    await pumpApp(t);
    await tap(t, find.byKey(const ValueKey('scheme-row-deepseek')));
    await tap(t, find.text('编辑'));
    // an edit (the user re-typed the API address) marks the form unsaved
    final url = find.byType(TextField).at(1); // 方案名称, API 地址, API 密钥
    expect(
      t.widget<TextField>(url).controller!.text,
      'https://api.deepseek.com/v1',
    );
    await t.enterText(url, 'https://api.deepseek.com/v1 ');
    await t.pumpAndSettle();
    await t.enterText(url, 'https://api.deepseek.com/v1');
    await t.pumpAndSettle();
    expect(find.byKey(const ValueKey('unsaved-badge')), findsOneWidget);
    await tap(t, find.text('测试连接'));
    expect(find.byKey(const ValueKey('test-result')), findsOneWidget);
    expect(find.textContaining('deepseek-reasoner'), findsWidgets);
    expect(find.byKey(const ValueKey('editor-action-bar')), findsOneWidget);
    // models returned by the test feed the designed model dropdown
    expect(
      find.widgetWithText(Select<String>, 'deepseek-chat'),
      findsOneWidget,
    );
    expect(find.byKey(const ValueKey('unsaved-badge')), findsOneWidget);
    await shot(t, '08_edit_openai_tested');
    await unmount(t);
  }, variant: desktop);

  testWidgets('09 scheme menu + 10 delete confirm', (t) async {
    await pumpApp(t);
    await tapMenu(t, find.byKey(const ValueKey('scheme-menu-codex')));
    expect(find.byKey(const ValueKey('menu-delete')), findsOneWidget);
    await shot(t, '09_scheme_menu');
    await t.tapAt(const Offset(300, 600)); // outside the menu closes it
    for (var i = 0; i < 6; i++) {
      await t.pump(const Duration(milliseconds: 100));
    }
    expect(find.byKey(const ValueKey('scheme-menu')), findsNothing);
    await tapMenu(t, find.byKey(const ValueKey('scheme-menu-deepseek')));
    await tap(t, find.byKey(const ValueKey('menu-delete')));
    expect(find.text('删除方案'), findsWidgets);
    expect(find.textContaining('Acme 智能客服'), findsWidgets);
    await shot(t, '10_delete_confirm');
    await unmount(t);
  }, variant: desktop);

  testWidgets('11 usage', (t) async {
    await pumpApp(t);
    await tap(t, find.text('用量统计'));
    expect(find.text('近 7 天 · 本机'), findsOneWidget);
    expect(find.text('1.99M'), findsWidgets);
    expect(find.text('合计'), findsOneWidget);
    await shot(t, '11_usage');
    await unmount(t);
  }, variant: desktop);

  testWidgets('12 kernel start failed', (t) async {
    kernel.startError =
        'AVORA_Start: open ~/.avora/state.db: database is locked';
    await pumpApp(t);
    expect(find.byKey(const ValueKey('kernel-error')), findsOneWidget);
    expect(find.text('本机数据库被占用'), findsOneWidget);
    expect(find.textContaining('Bad state'), findsNothing);
    await shot(t, '12_kernel_start_failed');
    await unmount(t);
  }, variant: desktop);

  testWidgets('13 warning bar', (t) async {
    kernel.status = {
      ...designStatus(),
      'warning': 'remove bindings/b_old: permission denied',
      'cleanup_pending': ['bindings/b_old'],
    };
    await pumpApp(t);
    expect(find.byKey(const ValueKey('cleanup-warning')), findsOneWidget);
    await shot(t, '13_warning_bar');
    await tap(t, find.byKey(const ValueKey('cleanup-warning-close')));
    expect(find.byKey(const ValueKey('cleanup-warning')), findsNothing);
    await unmount(t);
  }, variant: desktop);

  testWidgets('14 dark main follows the system', (t) async {
    await pumpApp(t, dark: true);
    final ctx = t.element(find.byKey(const ValueKey('status-bar')));
    expect(Theme.of(ctx).brightness, Brightness.dark);
    expect(AppTokens.of(ctx).bg, AppTokens.dark.bg);
    await shot(t, '14_dark_main');
    await unmount(t);
  }, variant: desktop);

  testWidgets('15 add scheme: choose the service', (t) async {
    kernel
      ..adapters = []
      ..status = emptyStatus();
    await pumpApp(t);
    await tap(t, find.byKey(const ValueKey('empty-add-scheme')));
    await shot(t, '15_add_scheme_new');
    await tapMenu(t, find.byType(Select<String>).first);
    expect(find.text('Anthropic 兼容协议'), findsWidgets);
    expect(find.text('OpenAI Codex（订阅）'), findsWidgets);
    await shot(t, '15b_add_scheme_service');
    await unmount(t);
  }, variant: desktop);

  testWidgets('16 data directory in use', (t) async {
    kernel.startError =
        'data directory in use: ~/.config/avora is used by another Avora process (the desktop app or "avora serve"); stop it first';
    await pumpApp(t);
    expect(find.text('数据目录正被另一个 Avora 使用'), findsOneWidget);
    await shot(t, '16_data_dir_in_use');
    await unmount(t);
  }, variant: desktop);
}
