import 'dart:io';
import 'dart:ui' as ui;

import 'package:flutter/rendering.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:avora_ui/backend_catalog.dart';
import 'package:avora_ui/native_api.dart';
import 'package:avora_ui/profile_workspace.dart';
import 'package:shadcn_flutter/shadcn_flutter.dart';

import 'connection_dialog_scroll_test.dart' as conn;

// Token limits: only what the backend enforces is offered; the states
// unknown / vendor default / detected / user-set are shown apart.
void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  setUpAll(conn.loadFonts);

  // With UI_SHOTS_DIR set the states are also written as PNGs.
  Future<void> shot(WidgetTester t, String name) async {
    expect(t.takeException(), isNull, reason: '$name: no exception/overflow');
    final dir = Platform.environment['UI_SHOTS_DIR'];
    if (dir == null) return;
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

  Future<void> pump(WidgetTester tester, Map<String, dynamic> scheme) async {
    tester.view.physicalSize = const Size(1500, 1000);
    tester.view.devicePixelRatio = 1;
    addTearDown(tester.view.resetPhysicalSize);
    addTearDown(tester.view.resetDevicePixelRatio);
    await tester.pumpWidget(
      ShadcnApp(
        debugShowCheckedModeBanner: false,
        home: SizedBox.expand(
          child: ProfileWorkspace(
            api: NativeApi.instance,
            status: const {
              'paired': true,
              'connected': true,
              'running_tasks': 0,
            },
            adapters: [scheme],
            onChanged: () async {},
          ),
        ),
      ),
    );
    await tester.pumpAndSettle();
  }

  Future<void> editAdvanced(WidgetTester tester) async {
    await tester.tap(find.text('编辑'));
    await tester.pumpAndSettle();
    final toggle = find.byKey(const ValueKey('advanced-toggle'));
    await tester.ensureVisible(toggle);
    await tester.tap(toggle);
    await tester.pumpAndSettle();
  }

  testWidgets('Codex 不提供上下文窗口和最大输出，已存的值不会再发给内核', (tester) async {
    await pump(tester, {
      'id': 'cx',
      'name': 'Codex',
      'type': 'codex_app_server',
      'models': ['gpt-5-codex'],
      'enabled': true,
      'max_output_tokens': 4096,
      'context_window': 200000,
    });
    await editAdvanced(tester);
    expect(find.text('上下文窗口'), findsNothing);
    expect(find.text('最大输出 Token'), findsNothing);
    expect(find.text('自动检测'), findsNothing);
    expect(
      find.text('Codex 不支持限制上下文窗口和最大输出 Token，这里不提供设置，用量不受 Avora 限制。'),
      findsOneWidget,
    );
    await shot(tester, 'limits-codex-unsupported');
    final state = tester.state(find.byType(ProfileWorkspace)) as dynamic;
    expect(state.body()['max_output_tokens'], 0);
    expect(state.body()['context_window'], 0);
  });

  testWidgets('Grok Build 只提供最大输出，空值显示厂商默认', (tester) async {
    await pump(tester, {
      'id': 'gk',
      'name': 'Grok',
      'type': 'grok_build',
      'models': ['grok-build'],
      'enabled': true,
    });
    await editAdvanced(tester);
    expect(find.text('最大输出 Token'), findsOneWidget);
    expect(find.text('厂商默认'), findsOneWidget);
    expect(find.text('上下文窗口'), findsNothing);
    expect(find.byKey(const Key('limitsUnsupported')), findsOneWidget);
    await shot(tester, 'limits-grok-max-output-only');
  });

  testWidgets('Ollama 上下文窗口空值说明自动检测来源，Anthropic 说明 Avora 默认值', (tester) async {
    await pump(tester, {
      'id': 'ol',
      'name': '本机',
      'type': 'ollama_http',
      'base_url': 'http://127.0.0.1:11434',
      'models': ['qwen3:8b'],
      'enabled': true,
    });
    await editAdvanced(tester);
    expect(find.text('自动检测（Ollama /api/show）'), findsOneWidget);
    expect(find.text('厂商默认'), findsOneWidget);
    expect(find.byKey(const Key('limitsUnsupported')), findsNothing);
    expect(find.textContaining('按估算的输入 Token 预检'), findsOneWidget);
    await shot(tester, 'limits-ollama-auto-detect');

    await pump(tester, {
      'id': 'an',
      'name': 'Claude',
      'type': 'anthropic_compatible',
      'base_url': 'https://api.anthropic.com',
      'models': ['claude'],
      'enabled': true,
      'has_api_key': true,
    });
    await editAdvanced(tester);
    expect(find.text('Avora 默认 1024'), findsOneWidget);
    expect(find.text('未知，不预检'), findsOneWidget);
    await shot(tester, 'limits-anthropic-defaults');
  });

  testWidgets('方案详情按内核状态分别显示已设置、默认、未知和不支持', (tester) async {
    await pump(tester, {
      'id': 'oa',
      'name': 'DeepSeek',
      'type': 'openai_compatible',
      'base_url': 'https://api.deepseek.com',
      'models': ['deepseek-chat'],
      'enabled': true,
      'has_api_key': true,
      'context_window': 64000,
      'limits': {
        'context_window': {'state': 'user', 'value': 64000, 'estimate': true},
        'max_output_tokens': {'state': 'vendor_default'},
      },
    });
    expect(find.text('64000（已设置 · 按估算预检）'), findsOneWidget);
    expect(find.text('厂商默认（未设置）'), findsOneWidget);
    expect(find.text('自动检测'), findsNothing);
    await shot(tester, 'limits-detail-states');
  });

  test('limitStateLabel keeps the states apart', () {
    expect(
      limitStateLabel({
        'state': 'detected',
        'value': 4096,
        'source': 'Ollama /api/show qwen3.context_length',
        'estimate': true,
      }),
      '4096（自动检测，来源 Ollama /api/show qwen3.context_length · 按估算预检）',
    );
    expect(limitStateLabel({'state': 'unknown'}), '未知（未设置，不预检）');
    expect(
      limitStateLabel({'state': 'unknown', 'source': 'Ollama /api/show'}),
      '未设置 · 执行时从 Ollama /api/show 自动检测',
    );
    expect(limitStateLabel({'state': 'unsupported'}), '此方案不支持');
    expect(
      limitStateLabel({'state': 'avora_default', 'value': 1024}),
      'Avora 默认 1024（未设置）',
    );
    expect(limitStateLabel(null), '未知');
  });
}
