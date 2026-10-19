import 'dart:async';

import 'package:flutter/foundation.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:avora_ui/native_api.dart';
import 'package:avora_ui/profile_workspace.dart';
import 'package:avora_ui/ui_kit.dart';
import 'package:shadcn_flutter/shadcn_flutter.dart';

List<Map<String, dynamic>> modelCatalog(
  List<String> ids, {
  List<String>? efforts,
  String defaultEffort = '',
}) => [
  for (final id in ids)
    {
      'id': id,
      'reasoning_known': efforts != null,
      'default_reasoning_effort': defaultEffort,
      'reasoning_efforts': [
        for (final value in efforts ?? <String>[]) {'value': value},
      ],
    },
];

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();

  Future<void> pumpWorkspace(
    WidgetTester tester, {
    Map<String, dynamic>? connectionStatus,
    List<Map<String, dynamic>>? adapters,
  }) async {
    tester.view.physicalSize = const Size(1500, 900);
    tester.view.devicePixelRatio = 1;
    addTearDown(tester.view.resetPhysicalSize);
    addTearDown(tester.view.resetDevicePixelRatio);
    await tester.pumpWidget(
      ShadcnApp(
        debugShowCheckedModeBanner: false,
        home: SizedBox.expand(
          child: ProfileWorkspace(
            api: NativeApi.instance,
            status:
                connectionStatus ??
                const {
                  'paired': true,
                  'connected': true,
                  'device_name': '测试电脑',
                  'running_tasks': 0,
                },
            adapters:
                adapters ??
                const [
                  {
                    'id': 'deepseek',
                    'name': 'deepseek-v4-pro',
                    'type': 'openai_compatible',
                    'provider': 'deepseek',
                    'base_url': 'https://api.deepseek.com',
                    'models': ['deepseek-v4-pro'],
                    'enabled': true,
                    'is_default': true,
                    'has_api_key': true,
                  },
                  {
                    'id': 'claude',
                    'name': 'claude-sonnet',
                    'type': 'anthropic_compatible',
                    'provider': 'anthropic',
                    'base_url': 'https://api.anthropic.com',
                    'models': ['claude-sonnet'],
                    'enabled': true,
                    'has_api_key': true,
                  },
                ],
            onChanged: () async {},
          ),
        ),
      ),
    );
    await tester.pumpAndSettle();
  }

  Future<void> chooseModel(
    WidgetTester tester,
    String model, {
    String current = '选择模型',
  }) async {
    await tester.tap(find.widgetWithText(Select<String>, current));
    await tester.pump();
    await tester.pump(const Duration(milliseconds: 400));
    await tester.tap(find.text(model).last);
    await tester.pumpAndSettle();
  }

  testWidgets('方案列表和编辑表单在同一窗口显示', (tester) async {
    await pumpWorkspace(tester);

    expect(find.text('配置方案'), findsOneWidget);
    expect(find.text('服务'), findsWidgets);
    expect(find.text('方案名称'), findsWidgets);
    expect(find.text('方案详情'), findsOneWidget);
    expect(find.text('编辑'), findsOneWidget);
    expect(find.text('编辑方案'), findsNothing);
    expect(find.text('deepseek-v4-pro'), findsWidgets);
    expect(find.text('API 地址'), findsOneWidget);
    expect(find.text('测试连接'), findsNothing);
    expect(find.text('保存'), findsNothing);
    expect(find.text('测试电脑'), findsOneWidget);
    expect(find.text('连接详情 ›'), findsOneWidget);
  });

  testWidgets('保存配对信息不等于远端服务在线', (tester) async {
    await pumpWorkspace(
      tester,
      connectionStatus: const {
        'paired': true,
        'connected': false,
        'device_name': '测试电脑',
        'running_tasks': 0,
      },
    );

    expect(find.text('未连接'), findsOneWidget);
    expect(find.text('已连接'), findsNothing);
    expect(find.text('连接详情 ›'), findsOneWidget);
  });

  testWidgets('添加方案先放入列表并选中，编辑后才打开表单', (tester) async {
    await pumpWorkspace(tester);

    await tester.tap(find.text('添加方案').first);
    await tester.pumpAndSettle();

    // the new draft is listed, selected and opened straight in the editor
    expect(find.text('未命名方案'), findsWidgets);
    expect(find.text('编辑方案'), findsOneWidget);
    expect(find.text('deepseek-v4-pro'), findsOneWidget);
    expect(find.text('Codex CLI 路径'), findsOneWidget);
    expect(find.text('保存'), findsOneWidget);

    await tester.tap(find.text('取消'));
    await tester.pumpAndSettle();
    expect(find.text('方案详情'), findsOneWidget);
    expect(find.text('未命名方案'), findsWidgets);
  });

  testWidgets('选择方案只显示详情，点击编辑才进入编辑', (tester) async {
    await pumpWorkspace(tester);

    await tester.tap(find.text('claude-sonnet').first);
    await tester.pumpAndSettle();

    expect(find.text('编辑方案'), findsNothing);
    expect(find.text('方案详情'), findsOneWidget);
    expect(find.text('Anthropic 兼容协议'), findsWidgets);
    expect(find.byType(TextField), findsNothing);

    await tester.tap(find.text('编辑'));
    await tester.pumpAndSettle();
    expect(find.text('编辑方案'), findsOneWidget);
    expect(find.text('API 地址'), findsOneWidget);
    expect(find.text('API 密钥'), findsOneWidget);
  });

  testWidgets('服务支持六种类型，云端兼容协议均显示地址和密钥', (tester) async {
    debugDefaultTargetPlatformOverride = TargetPlatform.macOS;
    try {
      await pumpWorkspace(tester);
      await tester.tap(find.text('添加方案').first);
      await tester.pumpAndSettle();

      await tester.tap(find.widgetWithText(Select<String>, 'OpenAI Codex（订阅）'));
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 400));
      expect(find.text('OpenAI 兼容协议'), findsWidgets);
      expect(find.text('Anthropic 兼容协议'), findsWidgets);
      expect(find.text('DeepSeek'), findsNothing);
      expect(find.text('Grok Build（CLI）'), findsOneWidget);
      expect(find.text('Antigravity CLI'), findsOneWidget);
      expect(find.text('Ollama 协议（本地模型）'), findsOneWidget);

      await tester.tap(find.text('Anthropic 兼容协议').last);
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 400));
      expect(find.text('API 地址'), findsOneWidget);
      expect(find.text('API 密钥'), findsOneWidget);

      await tester.tap(find.widgetWithText(Select<String>, 'Anthropic 兼容协议'));
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 400));
      await tester.tap(find.text('OpenAI 兼容协议').last);
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 400));
      expect(find.text('API 地址'), findsOneWidget);
      expect(find.text('API 密钥'), findsOneWidget);
    } finally {
      debugDefaultTargetPlatformOverride = null;
    }
  });

  for (final cli in [
    ('Grok Build（CLI）', 'grok_build', 'grok'),
    ('Antigravity CLI', 'antigravity_cli', 'agy'),
  ]) {
    testWidgets(
      '${cli.$1} 可获取模型并保存自定义执行路径',
      (tester) async {
        final requests = <Map<String, dynamic>>[];
        NativeApi.debugTransport = (payload) {
          requests.add(payload);
          return switch (payload['action']) {
            'provider_catalog' =>
              payload['adapter']['type'] == 'codex_app_server'
                  ? []
                  : modelCatalog(['test-model']),
            'test_adapter' => {
              'health': 'ready',
              'models': ['test-model'],
            },
            'create_adapter' => payload['adapter'],
            _ => throw StateError('Unexpected action: ${payload['action']}'),
          };
        };
        addTearDown(() => NativeApi.debugTransport = null);
        await pumpWorkspace(tester);
        await tester.tap(find.text('添加方案').first);
        await tester.pumpAndSettle();
        await tester.tap(
          find.widgetWithText(Select<String>, 'OpenAI Codex（订阅）'),
        );
        await tester.pump();
        await tester.pump(const Duration(milliseconds: 400));
        await tester.tap(find.text(cli.$1).last);
        await tester.pumpAndSettle();
        expect(find.text('API 地址'), findsNothing);
        expect(find.text('API 密钥'), findsNothing);
        final path = find.byWidgetPredicate(
          (w) => w is TextField && w.controller?.text == cli.$3,
        );
        await tester.enterText(path, '/opt/my tools/${cli.$3}');
        await tester.tap(find.text('获取模型'));
        await tester.pumpAndSettle();
        final discovery = requests.singleWhere(
          (r) =>
              r['action'] == 'provider_catalog' &&
              r['adapter']['type'] == cli.$2,
        )['adapter'];
        expect(discovery, containsPair('type', cli.$2));
        expect(
          discovery,
          containsPair('executable', '/opt/my tools/${cli.$3}'),
        );
        expect(find.widgetWithText(Select<String>, '选择模型'), findsOneWidget);
        await chooseModel(tester, 'test-model');
        expect(
          find.widgetWithText(Select<String>, 'test-model'),
          findsOneWidget,
        );
        await tester.tap(find.text('测试连接'));
        await tester.pumpAndSettle();
        await tester.tap(find.text('保存'));
        await tester.pumpAndSettle();
        final saved = requests.singleWhere(
          (r) => r['action'] == 'create_adapter',
        )['adapter'];
        expect(saved, containsPair('type', cli.$2));
        expect(saved, containsPair('executable', '/opt/my tools/${cli.$3}'));
        expect(saved, containsPair('models', ['test-model']));
        expect(saved, isNot(contains('codex_bin')));
        expect(find.text('编辑方案'), findsNothing);
        expect(tester.takeException(), isNull);
      },
      variant: TargetPlatformVariant.only(TargetPlatform.macOS),
    );

    testWidgets('${cli.$1} 编辑时保留路径和多个模型', (tester) async {
      await pumpWorkspace(
        tester,
        adapters: [
          {
            'id': 'cli',
            'name': '自定义 CLI',
            'type': cli.$2,
            'executable': '/opt/my tools/${cli.$3}',
            'models': ['first', 'second'],
            'enabled': true,
          },
        ],
      );
      await tester.tap(find.text('编辑'));
      await tester.pumpAndSettle();
      final state = tester.state(find.byType(ProfileWorkspace)) as dynamic;
      expect(state.validate(), isNull);
      expect(state.body()['type'], cli.$2);
      expect(state.body()['executable'], '/opt/my tools/${cli.$3}');
      expect(state.body()['models'], ['first', 'second']);
      expect(tester.takeException(), isNull);
    });
  }

  testWidgets('切换 CLI 后丢弃之前的模型查询结果', (tester) async {
    NativeApi.debugTransport = (payload) =>
        payload['adapter']['type'] == 'codex_app_server'
        ? []
        : modelCatalog(['old-grok-model']);
    addTearDown(() {
      NativeApi.debugTransport = null;
      NativeApi.debugGate = null;
    });
    await pumpWorkspace(tester);
    await tester.tap(find.text('添加方案').first);
    await tester.pumpAndSettle();
    final state = tester.state(find.byType(ProfileWorkspace)) as dynamic;
    state.changeProvider('grok');
    await tester.pumpAndSettle();
    final gate = Completer<void>();
    NativeApi.debugGate = () => gate.future;
    final pending = state.fetchModels() as Future<void>;
    await tester.pump();
    state.changeProvider('antigravity');
    gate.complete();
    await pending;
    await tester.pumpAndSettle();
    expect(state.providerModels, isEmpty);
    expect(state.model.text, isEmpty);
    expect(find.text('old-grok-model'), findsNothing);
  });

  testWidgets(
    '新增 Ollama 方案可免密钥获取模型、测试连接并保存',
    (tester) async {
      final requests = <Map<String, dynamic>>[];
      NativeApi.debugTransport = (payload) {
        requests.add(payload);
        return switch (payload['action']) {
          'codex_models' => <String>[],
          'provider_models' => ['qwen3:8b', 'llama3:latest'],
          'test_adapter' => {
            'health': 'ready',
            'models': ['qwen3:8b'],
          },
          'create_adapter' => payload['adapter'],
          _ => throw StateError('Unexpected action: ${payload['action']}'),
        };
      };
      addTearDown(() => NativeApi.debugTransport = null);
      await pumpWorkspace(tester);
      await tester.tap(find.text('添加方案').first);
      await tester.pumpAndSettle();
      await tester.tap(find.widgetWithText(Select<String>, 'OpenAI Codex（订阅）'));
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 400));
      await tester.tap(find.text('Ollama 协议（本地模型）').last);
      await tester.pumpAndSettle();

      expect(find.text('API 密钥'), findsNothing);
      final address = find.byWidgetPredicate(
        (w) => w is TextField && w.controller?.text == 'http://127.0.0.1:11434',
      );
      expect(
        tester.widget<TextField>(address).controller!.text,
        'http://127.0.0.1:11434',
      );
      // The default can be changed for another locally running instance.
      await tester.enterText(address, 'http://127.0.0.1:11435');
      await tester.tap(find.text('获取模型'));
      await tester.pumpAndSettle();
      final discovery = requests.singleWhere(
        (r) => r['action'] == 'provider_models',
      );
      expect(discovery['adapter'], containsPair('type', 'ollama_http'));
      expect(
        discovery['adapter'],
        containsPair('base_url', 'http://127.0.0.1:11435'),
      );
      expect(discovery['adapter'], containsPair('api_key', ''));
      await chooseModel(tester, 'qwen3:8b');
      expect(find.widgetWithText(Select<String>, 'qwen3:8b'), findsOneWidget);
      expect(find.textContaining('列表来自 Ollama /api/tags'), findsOneWidget);

      await tester.tap(find.text('测试连接'));
      await tester.pumpAndSettle();
      expect(find.textContaining('连接测试通过'), findsOneWidget);
      await tester.tap(find.text('保存'));
      await tester.pumpAndSettle();
      final saved = requests.singleWhere(
        (r) => r['action'] == 'create_adapter',
      )['adapter'];
      expect(saved, containsPair('type', 'ollama_http'));
      expect(saved, containsPair('provider', 'ollama'));
      expect(saved, containsPair('base_url', 'http://127.0.0.1:11435'));
      expect(saved, containsPair('models', ['qwen3:8b']));
      expect(saved, containsPair('api_key', ''));
      expect(find.text('编辑方案'), findsNothing);
      expect(tester.takeException(), isNull);
    },
    variant: TargetPlatformVariant.only(TargetPlatform.macOS),
  );

  testWidgets('已有 Ollama 方案保留地址和模型，支持直接编辑', (tester) async {
    await pumpWorkspace(
      tester,
      adapters: [
        {
          'id': 'existing-ollama',
          'name': '本机模型',
          'type': 'ollama_http',
          'provider': 'legacy_ollama',
          'base_url': 'http://127.0.0.1:11435',
          'models': ['qwen3:8b', 'llama3:latest'],
          'enabled': true,
        },
      ],
    );
    expect(find.text('旧版 Ollama'), findsNothing);
    await tester.tap(find.text('编辑'));
    await tester.pumpAndSettle();
    expect(
      find.widgetWithText(Select<String>, 'Ollama 协议（本地模型）'),
      findsOneWidget,
    );
    expect(find.text('API 密钥'), findsNothing);
    final state = tester.state(find.byType(ProfileWorkspace)) as dynamic;
    expect(state.validate(), isNull);
    expect(state.body()['type'], 'ollama_http');
    expect(state.body()['base_url'], 'http://127.0.0.1:11435');
    expect(state.body()['models'], ['qwen3:8b', 'llama3:latest']);
  });

  testWidgets(
    '模型查询和登录不命名，首次选定模型后名称固定',
    (tester) async {
      NativeApi.debugTransport = (payload) => switch (payload['action']) {
        'provider_catalog' => modelCatalog(['gpt-6.1-sol', 'another-model']),
        'codex_login_check' => {
          'complete': true,
          'models': ['gpt-6.1-sol', 'another-model'],
        },
        _ => throw StateError('Unexpected action: ${payload['action']}'),
      };
      addTearDown(() => NativeApi.debugTransport = null);
      await pumpWorkspace(tester);
      await tester.tap(find.text('添加方案').first);
      await tester.pumpAndSettle();
      final state = tester.state(find.byType(ProfileWorkspace)) as dynamic;
      expect(state.name.text, isEmpty);
      expect(state.model.text, isEmpty);
      expect(state.dirty, isFalse);
      expect(state.body()['name'], isEmpty);
      expect(find.text('未命名方案'), findsWidgets);

      await tester.tap(find.text('获取模型'));
      await tester.pumpAndSettle();
      state.loginPending = true;
      await state.checkLogin();
      await tester.pumpAndSettle();
      expect(state.name.text, isEmpty);
      expect(state.model.text, isEmpty);
      expect(state.codexConnected, isTrue);

      await chooseModel(tester, 'gpt-6.1-sol');
      expect(state.name.text, 'gpt-6.1-sol');
      await chooseModel(tester, 'another-model', current: 'gpt-6.1-sol');
      expect(state.name.text, 'gpt-6.1-sol');
      expect(state.body()['models'], ['another-model']);
      state.changeProvider('grok');
      await tester.pumpAndSettle();
      expect(state.model.text, isEmpty);
      expect(state.name.text, 'gpt-6.1-sol');
      expect(tester.takeException(), isNull);
    },
    variant: TargetPlatformVariant.only(TargetPlatform.macOS),
  );

  testWidgets('手动填写的方案名不会被选择模型覆盖', (tester) async {
    NativeApi.debugTransport = (_) =>
        modelCatalog(['gpt-6.1-sol', 'another-model']);
    addTearDown(() => NativeApi.debugTransport = null);
    await pumpWorkspace(tester);
    await tester.tap(find.text('添加方案').first);
    await tester.pumpAndSettle();
    await tester.enterText(
      find.widgetWithText(TextField, '输入名称，或选定模型后自动填充'),
      '工作方案',
    );
    await chooseModel(tester, 'gpt-6.1-sol');
    await chooseModel(tester, 'another-model', current: 'gpt-6.1-sol');
    final state = tester.state(find.byType(ProfileWorkspace)) as dynamic;
    expect(state.name.text, '工作方案');
    expect(state.body()['name'], '工作方案');
  }, variant: TargetPlatformVariant.only(TargetPlatform.macOS));

  testWidgets('手动模型输入完成后只命名一次，重名仍提示', (tester) async {
    NativeApi.debugTransport = (_) => <String>[];
    addTearDown(() => NativeApi.debugTransport = null);
    await pumpWorkspace(tester);
    await tester.tap(find.text('添加方案').first);
    await tester.pumpAndSettle();
    final state = tester.state(find.byType(ProfileWorkspace)) as dynamic;
    final modelField = find.widgetWithText(TextField, '输入模型 ID');
    const nameHint = '输入名称，或选定模型后自动填充';
    await tester.enterText(modelField, 'claude');
    await tester.pump();
    expect(state.name.text, isEmpty);
    await tester.enterText(modelField, 'claude-sonnet');
    await tester.testTextInput.receiveAction(TextInputAction.done);
    await tester.pumpAndSettle();
    expect(state.name.text, 'claude-sonnet_2');
    await tester.enterText(modelField, 'changed-model');
    await tester.testTextInput.receiveAction(TextInputAction.done);
    await tester.pumpAndSettle();
    expect(state.name.text, 'claude-sonnet_2');

    await tester.enterText(
      find.widgetWithText(TextField, nameHint),
      ' Claude-Sonnet ',
    );
    await tester.pumpAndSettle();
    expect(find.textContaining('已被其他方案使用'), findsOneWidget);
    await tester.enterText(find.widgetWithText(TextField, nameHint), '');
    expect(state.validate(), '请输入方案名称。');
    expect(state.body()['name'], isEmpty);
  });

  testWidgets('自定义模型确认后命名，后续编辑模型保持方案名', (tester) async {
    NativeApi.debugTransport = (_) => modelCatalog(['gpt-6.1-sol']);
    addTearDown(() => NativeApi.debugTransport = null);
    await pumpWorkspace(tester);
    await tester.tap(find.text('添加方案').first);
    await tester.pumpAndSettle();
    final state = tester.state(find.byType(ProfileWorkspace)) as dynamic;
    for (final model in ['custom-model', 'changed-model']) {
      await tester.tap(find.widgetWithText(AppButton, '自定义模型'));
      await tester.pumpAndSettle();
      await tester.enterText(
        find.descendant(
          of: find.byKey(const ValueKey('app-dialog')),
          matching: find.byType(TextField),
        ),
        model,
      );
      await tester.tap(find.text('确定'));
      await tester.pumpAndSettle();
      expect(state.name.text, 'custom-model');
      expect(state.body()['models'], [model]);
    }
  });

  testWidgets(
    '新版编辑器可先获取模型，再选择或自定义模型',
    (tester) async {
      final requests = <Map<String, dynamic>>[];
      NativeApi.debugTransport = (payload) {
        requests.add(payload);
        if (payload['action'] == 'provider_models') {
          return ['deepseek-v4-pro', 'provider-choice'];
        }
        throw StateError('Unexpected action: ${payload['action']}');
      };
      addTearDown(() => NativeApi.debugTransport = null);
      await pumpWorkspace(tester);
      await tester.tap(find.text('编辑'));
      await tester.pumpAndSettle();

      // Discovery only needs the endpoint and saved key, even without a model ID.
      await tester.enterText(
        find.widgetWithText(TextField, '输入供应商提供的模型 ID'),
        '',
      );
      await tester.tap(find.text('获取模型'));
      await tester.pumpAndSettle();
      expect(requests, hasLength(1));
      expect(requests.single['action'], 'provider_models');
      expect(requests.single['adapter'], {
        'id': 'deepseek',
        'type': 'openai_compatible',
        'base_url': 'https://api.deepseek.com',
        'api_key': '',
      });

      final state = tester.state(find.byType(ProfileWorkspace)) as dynamic;
      expect(state.model.text, isEmpty);
      await chooseModel(tester, 'provider-choice');
      expect(state.name.text, 'deepseek-v4-pro');
      expect(state.body()['models'], ['provider-choice']);
      // Editing the model invalidates connection tests, but keeps discovered choices.
      expect(
        find.widgetWithText(Select<String>, 'provider-choice'),
        findsOneWidget,
      );

      await tester.tap(find.widgetWithText(AppButton, '自定义模型'));
      await tester.pumpAndSettle();
      await tester.enterText(
        find.descendant(
          of: find.byKey(const ValueKey('app-dialog')),
          matching: find.byType(TextField),
        ),
        'manual-model',
      );
      await tester.tap(find.text('确定'));
      await tester.pumpAndSettle();
      expect(state.body()['models'], ['manual-model']);
      expect(
        find.widgetWithText(Select<String>, 'manual-model'),
        findsOneWidget,
      );
      expect(find.byKey(const ValueKey('editor-action-bar')), findsOneWidget);
      expect(tester.takeException(), isNull);
    },
    variant: TargetPlatformVariant.only(TargetPlatform.macOS),
  );

  // Review9 P2-9: a new scheme for a service that can run inference but has
  // no /models endpoint must be able to skip the model list probe from the
  // editor (it was only kept for configs that already had it).
  testWidgets('兼容协议方案可在高级选项中勾选跳过模型列表探测', (tester) async {
    debugDefaultTargetPlatformOverride =
        TargetPlatform.macOS; // Select popups as on the desktop
    await pumpWorkspace(tester);
    await tester.tap(find.text('添加方案').first);
    await tester.pumpAndSettle();
    // a new scheme starts as Codex (no option there); pick a compatible protocol
    expect(find.byKey(const ValueKey('skip-model-probe')), findsNothing);
    await tester.tap(find.widgetWithText(Select<String>, 'OpenAI Codex（订阅）'));
    await tester.pump();
    await tester.pump(const Duration(milliseconds: 400));
    await tester.tap(find.text('OpenAI 兼容协议').last);
    await tester.pump();
    await tester.pump(const Duration(milliseconds: 400));
    // the option sits in the collapsed 高级选项 section
    expect(find.byKey(const ValueKey('skip-model-probe')), findsNothing);
    await tester.ensureVisible(find.byKey(const ValueKey('advanced-toggle')));
    await tester.tap(find.byKey(const ValueKey('advanced-toggle')));
    await tester.pumpAndSettle();
    final box = find.byKey(const ValueKey('skip-model-probe'));
    expect(
      box,
      findsOneWidget,
      reason: 'no editor option to skip the model list probe',
    );
    final state = tester.state(find.byType(ProfileWorkspace)) as dynamic;
    expect(state.body()['skip_model_probe'], isFalse);
    await tester.ensureVisible(box);
    await tester.pumpAndSettle();
    await tester.tap(box);
    await tester.pumpAndSettle();
    expect(state.body()['skip_model_probe'], isTrue);
    // also offered for the Anthropic compatible protocol
    await tester.tap(find.widgetWithText(Select<String>, 'OpenAI 兼容协议'));
    await tester.pump();
    await tester.pump(const Duration(milliseconds: 400));
    await tester.tap(find.text('Anthropic 兼容协议').last);
    await tester.pump();
    await tester.pump(const Duration(milliseconds: 400));
    expect(find.byKey(const ValueKey('skip-model-probe')), findsOneWidget);
    debugDefaultTargetPlatformOverride = null;
  });
  Future<void> openAdvanced(WidgetTester tester) async {
    final toggle = find.byKey(const ValueKey('advanced-toggle'));
    await tester.ensureVisible(toggle);
    await tester.tap(toggle);
    await tester.pumpAndSettle();
  }

  Future<void> chooseEffort(WidgetTester tester, String value) async {
    final option = find.byKey(ValueKey('effort-$value'));
    await tester.ensureVisible(option);
    await tester.tap(option);
    await tester.pumpAndSettle();
  }

  for (final service in [
    ('codex', 'codex_app_server'),
    ('grok', 'grok_build'),
  ]) {
    testWidgets(
      '${service.$1} 仅展示真实档位，默认不覆盖模型设置',
      (tester) async {
        final levels = service.$1 == 'codex'
            ? ['low', 'medium', 'high', 'xhigh', 'max', 'ultra']
            : ['low', 'medium', 'high', 'xhigh'];
        NativeApi.debugTransport = (_) => [
          ...modelCatalog(['full'], efforts: levels, defaultEffort: 'high'),
          ...modelCatalog(['limited'], efforts: ['low', 'high']),
          ...modelCatalog(['can-disable'], efforts: ['none', 'low']),
        ];
        addTearDown(() => NativeApi.debugTransport = null);
        await pumpWorkspace(
          tester,
          adapters: [
            {
              'id': 's',
              'name': '工作方案',
              'type': service.$2,
              'models': ['full'],
              'enabled': true,
            },
          ],
        );
        await tester.tap(find.text('编辑'));
        await tester.pumpAndSettle();
        final state = tester.state(find.byType(ProfileWorkspace)) as dynamic;
        expect(state.body()['reasoning_effort'], '');
        await openAdvanced(tester);
        expect(find.byKey(const ValueKey('effort-default')), findsOneWidget);
        expect(find.byKey(const ValueKey('effort-none')), findsNothing);
        expect(find.byKey(const ValueKey('effort-off')), findsNothing);
        for (final level in levels) {
          expect(find.byKey(ValueKey('effort-$level')), findsOneWidget);
        }
        await chooseEffort(tester, levels.last);
        expect(state.body()['reasoning_effort'], levels.last);
        await chooseEffort(tester, 'default');
        expect(state.body()['reasoning_effort'], '');
        await chooseModel(tester, 'limited', current: 'full');
        expect(find.byKey(const ValueKey('effort-xhigh')), findsNothing);
        await chooseModel(tester, 'can-disable', current: 'limited');
        expect(find.byKey(const ValueKey('effort-none')), findsOneWidget);
        await chooseEffort(tester, 'none');
        expect(state.body()['reasoning_effort'], 'none');
        await chooseModel(tester, 'limited', current: 'can-disable');
        expect(state.body()['reasoning_effort'], '');
        expect(state.body()['name'], '工作方案');
        expect(tester.takeException(), isNull);
      },
      variant: TargetPlatformVariant.only(TargetPlatform.macOS),
    );
  }

  List<Map<String, dynamic>> variantCatalog() => [
    for (final level in ['low', 'medium', 'high'])
      {
        'id': 'flash-$level',
        'reasoning_known': true,
        'default_reasoning_effort': level,
        'reasoning_efforts': [
          for (final e in ['low', 'medium', 'high'])
            {'value': e, 'model': 'flash-$e'},
        ],
      },
    ...modelCatalog(['gpt-oss-120b-medium'], efforts: []),
  ];

  testWidgets(
    'Antigravity 档位与模型双向联动，默认保留变体和方案名称',
    (tester) async {
      NativeApi.debugTransport = (_) => variantCatalog();
      addTearDown(() => NativeApi.debugTransport = null);
      await pumpWorkspace(
        tester,
        adapters: [
          {
            'id': 's',
            'name': '固定方案名',
            'type': 'antigravity_cli',
            'models': ['flash-high'],
            'enabled': true,
          },
        ],
      );
      await tester.tap(find.text('编辑'));
      await tester.pumpAndSettle();
      final state = tester.state(find.byType(ProfileWorkspace)) as dynamic;
      expect(state.body()['reasoning_effort'], '');
      await openAdvanced(tester);
      await chooseEffort(tester, 'low');
      expect(state.body()['models'], ['flash-low']);
      expect(state.body()['reasoning_effort'], 'low');
      expect(state.body()['name'], '固定方案名');
      await chooseModel(tester, 'flash-medium', current: 'flash-low');
      expect(state.body()['reasoning_effort'], 'medium');
      await chooseEffort(tester, 'default');
      expect(state.body()['models'], ['flash-medium']);
      expect(state.body()['reasoning_effort'], '');
      await chooseModel(tester, 'gpt-oss-120b-medium', current: 'flash-medium');
      expect(find.byKey(const ValueKey('effort-low')), findsNothing);
      expect(find.byKey(const ValueKey('effort-medium')), findsNothing);
      expect(find.byKey(const ValueKey('effort-none')), findsNothing);
      expect(find.byKey(const ValueKey('effort-default')), findsOneWidget);
      expect(state.body()['name'], '固定方案名');
      expect(tester.takeException(), isNull);
    },
    variant: TargetPlatformVariant.only(TargetPlatform.macOS),
  );

  testWidgets('已有 Antigravity 冲突配置映射到可用变体', (tester) async {
    NativeApi.debugTransport = (_) => variantCatalog();
    addTearDown(() => NativeApi.debugTransport = null);
    await pumpWorkspace(
      tester,
      adapters: [
        {
          'id': 's',
          'name': '旧方案',
          'type': 'antigravity_cli',
          'models': ['flash-high'],
          'reasoning_effort': 'low',
          'enabled': true,
        },
      ],
    );
    await tester.tap(find.text('编辑'));
    await tester.pumpAndSettle();
    final state = tester.state(find.byType(ProfileWorkspace)) as dynamic;
    expect(state.body()['models'], ['flash-low']);
    expect(state.body()['reasoning_effort'], 'low');
    expect(state.body()['name'], '旧方案');
  });

  testWidgets('旧 Codex 关闭配置必须显式修正，不显示不支持的关闭选项', (tester) async {
    NativeApi.debugTransport = (_) =>
        modelCatalog(['m'], efforts: ['low', 'high']);
    addTearDown(() => NativeApi.debugTransport = null);
    await pumpWorkspace(
      tester,
      adapters: [
        {
          'id': 's',
          'name': '旧方案',
          'type': 'codex_app_server',
          'models': ['m'],
          'reasoning_effort': 'off',
          'enabled': true,
        },
      ],
    );
    await tester.tap(find.text('编辑'));
    await tester.pumpAndSettle();
    final state = tester.state(find.byType(ProfileWorkspace)) as dynamic;
    expect(state.body()['reasoning_effort'], 'none');
    expect(state.validate(), contains('不支持'));
    await openAdvanced(tester);
    expect(find.byKey(const ValueKey('effort-none')), findsNothing);
    await chooseEffort(tester, 'default');
    expect(state.validate(), isNull);
    expect(state.body()['reasoning_effort'], '');
  });

  testWidgets('切换执行路径时丢弃过期能力，不填充名称', (tester) async {
    NativeApi.debugTransport = (_) =>
        modelCatalog(['old'], efforts: ['low', 'high']);
    addTearDown(() {
      NativeApi.debugTransport = null;
      NativeApi.debugGate = null;
    });
    await pumpWorkspace(tester);
    await tester.tap(find.text('添加方案').first);
    await tester.pumpAndSettle();
    final state = tester.state(find.byType(ProfileWorkspace)) as dynamic;
    final gate = Completer<void>();
    NativeApi.debugGate = () => gate.future;
    final pending = state.refreshCodex() as Future<void>;
    state.cliPath.text = '/different/codex';
    gate.complete();
    await pending;
    await tester.pumpAndSettle();
    expect(state.currentModelCapabilities, isNull);
    expect(state.modelChoices, isEmpty);
    expect(state.model.text, isEmpty);
    expect(state.name.text, isEmpty);
    expect(state.checkingCodex, isFalse);
  });
  testWidgets('同服务切换方案不接收上一个方案的能力', (tester) async {
    NativeApi.debugTransport = (payload) => modelCatalog([
      payload['adapter']['id'] == 'second' ? 'new-model' : 'old-model',
    ], efforts: payload['adapter']['id'] == 'second' ? ['low'] : ['high']);
    addTearDown(() {
      NativeApi.debugTransport = null;
      NativeApi.debugGate = null;
    });
    await pumpWorkspace(tester);
    await tester.tap(find.text('添加方案').first);
    await tester.pumpAndSettle();
    final state = tester.state(find.byType(ProfileWorkspace)) as dynamic;
    final gate = Completer<void>();
    NativeApi.debugGate = () => gate.future;
    final oldRequest = state.refreshCodex() as Future<void>;
    NativeApi.debugGate = null;
    state.load({
      'id': 'second',
      'name': '第二个方案',
      'type': 'codex_app_server',
      'models': ['new-model'],
    });
    await tester.pumpAndSettle();
    gate.complete();
    await oldRequest;
    await tester.pumpAndSettle();
    expect(state.modelChoices, ['new-model']);
    expect(state.currentModelCapabilities.option('high'), isNull);
    expect(state.currentModelCapabilities.option('low'), isNotNull);
    expect(state.name.text, '第二个方案');
  });

  testWidgets('目录获取失败保留已配置档位，旧 Grok 关闭显示默认', (tester) async {
    NativeApi.debugTransport = (_) => throw StateError('offline');
    addTearDown(() => NativeApi.debugTransport = null);
    await pumpWorkspace(
      tester,
      adapters: [
        {
          'id': 's',
          'name': '原配置',
          'type': 'grok_build',
          'models': ['m'],
          'reasoning_effort': 'high',
        },
      ],
    );
    await tester.tap(find.text('编辑'));
    await tester.pumpAndSettle();
    final state = tester.state(find.byType(ProfileWorkspace)) as dynamic;
    expect(state.body()['reasoning_effort'], 'high');
    await openAdvanced(tester);
    expect(find.byKey(const ValueKey('effort-high')), findsNothing);
    expect(find.textContaining('保留已配置档位'), findsOneWidget);
    state.load({
      'id': 's',
      'name': '原配置',
      'type': 'grok_build',
      'models': ['m'],
      'reasoning_effort': 'off',
    });
    await tester.pumpAndSettle();
    expect(state.body()['reasoning_effort'], '');
  });
}
