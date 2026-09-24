import 'package:flutter/foundation.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:localsubproxy_ui/native_api.dart';
import 'package:localsubproxy_ui/profile_workspace.dart';
import 'package:shadcn_flutter/shadcn_flutter.dart';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();

  Future<void> pumpWorkspace(
    WidgetTester tester, {
    Map<String, dynamic>? connectionStatus,
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
            adapters: const [
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
    expect(find.text('操作'), findsOneWidget);
    expect(find.text('方案详情'), findsOneWidget);
    expect(find.text('编辑'), findsOneWidget);
    expect(find.text('编辑方案'), findsNothing);
    expect(find.text('deepseek-v4-pro'), findsWidgets);
    expect(find.text('API 地址'), findsOneWidget);
    expect(find.text('测试连接'), findsNothing);
    expect(find.text('保存'), findsNothing);
    expect(find.text('测试电脑'), findsOneWidget);
    expect(find.text('连接详情  ›'), findsOneWidget);
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
    expect(find.text('连接详情  ›'), findsOneWidget);
  });

  testWidgets('添加方案先放入列表并选中，编辑后才打开表单', (tester) async {
    await pumpWorkspace(tester);

    await tester.tap(find.text('添加方案').first);
    await tester.pumpAndSettle();

    expect(find.text('未命名方案'), findsWidgets);
    expect(find.text('方案详情'), findsOneWidget);
    expect(find.text('保存'), findsNothing);
    expect(find.text('deepseek-v4-pro'), findsOneWidget);

    await tester.tap(find.text('编辑'));
    await tester.pumpAndSettle();
    expect(find.text('编辑方案'), findsOneWidget);
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
    expect(find.text('API 密钥'), findsNothing);

    await tester.tap(find.text('编辑'));
    await tester.pumpAndSettle();
    expect(find.text('编辑方案'), findsOneWidget);
    expect(find.text('API 地址'), findsOneWidget);
    expect(find.text('API 密钥'), findsOneWidget);
  });

  testWidgets('供应商仅提供三种类型，兼容协议均显示地址和密钥', (tester) async {
    debugDefaultTargetPlatformOverride = TargetPlatform.macOS;
    try {
      await pumpWorkspace(tester);
      await tester.tap(find.text('添加方案').first);
      await tester.pumpAndSettle();
      await tester.tap(find.text('编辑'));
      await tester.pumpAndSettle();

      await tester.tap(find.widgetWithText(Select<String>, 'OpenAI Codex（订阅）'));
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 400));
      expect(find.text('OpenAI 兼容协议'), findsWidgets);
      expect(find.text('Anthropic 兼容协议'), findsWidgets);
      expect(find.text('DeepSeek'), findsNothing);
      expect(find.text('本机 Ollama'), findsNothing);

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
}
