import 'package:flutter/foundation.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:avora_ui/native_api.dart';
import 'package:avora_ui/profile_workspace.dart';
import 'package:avora_ui/ui_kit.dart';
import 'package:shadcn_flutter/shadcn_flutter.dart';

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

  testWidgets('服务支持四种类型，云端兼容协议均显示地址和密钥', (tester) async {
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

  testWidgets('新方案名称默认为模型名，重名时加编号 并随模型变化，重名时提示', (tester) async {
    await pumpWorkspace(tester);
    await tester.tap(find.text('添加方案').first);
    await tester.pumpAndSettle();

    TextField field(String hint) => tester.widget<TextField>(
      find.ancestor(of: find.text(hint), matching: find.byType(TextField)),
    );
    final nameHint = '默认：模型名，重名时加 _2、_3';
    await tester.enterText(
      find.ancestor(of: find.text('输入模型 ID'), matching: find.byType(TextField)),
      'claude-sonnet',
    );
    await tester.pumpAndSettle();
    expect(field(nameHint).controller!.text, 'claude-sonnet_2');

    // a user-typed duplicate (trimmed, case-insensitive) is flagged inline
    await tester.enterText(find.byWidget(field(nameHint)), ' Claude-Sonnet ');
    await tester.pumpAndSettle();
    expect(find.textContaining('已被其他方案使用'), findsOneWidget);
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
      expect(state.body()['models'], ['deepseek-v4-pro']);
      await tester.tap(find.widgetWithText(Select<String>, 'deepseek-v4-pro'));
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 400));
      await tester.tap(find.text('provider-choice').last);
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 400));
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
}
