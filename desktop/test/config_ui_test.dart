import 'package:flutter/foundation.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:localsubproxy_ui/native_api.dart';
import 'package:localsubproxy_ui/profile_workspace.dart';
import 'package:shadcn_flutter/shadcn_flutter.dart';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();

  Future<void> pumpWorkspace(WidgetTester tester) async {
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
            ],
            onChanged: () async {},
          ),
        ),
      ),
    );
    await tester.pump();
  }

  testWidgets('方案列表不直接展开配置表单', (tester) async {
    await pumpWorkspace(tester);

    expect(find.text('配置方案'), findsOneWidget);
    expect(find.text('添加方案'), findsOneWidget);
    expect(find.text('编辑'), findsOneWidget);
    expect(find.text('服务与认证'), findsNothing);
    expect(find.text('确定'), findsNothing);
  });

  testWidgets('添加按钮在弹窗中配置新方案', (tester) async {
    await pumpWorkspace(tester);

    await tester.tap(find.text('添加方案'));
    await tester.pumpAndSettle();

    expect(find.text('添加方案'), findsWidgets);
    expect(find.text('服务与认证'), findsOneWidget);
    expect(find.text('模型与推理'), findsOneWidget);
    expect(find.text('启用方案'), findsNothing);
    expect(find.text('测试连接'), findsOneWidget);
    expect(find.text('确定'), findsOneWidget);
  });

  testWidgets('列表上方的编辑按钮打开配置弹窗', (tester) async {
    await pumpWorkspace(tester);

    await tester.tap(find.text('编辑'));
    await tester.pumpAndSettle();

    expect(find.text('编辑方案'), findsOneWidget);
    expect(find.text('API 密钥'), findsOneWidget);
    expect(find.text('上下文窗口'), findsOneWidget);
    expect(find.text('最大输出 Token'), findsOneWidget);
    expect(find.text('测试连接'), findsOneWidget);
    expect(find.text('确定'), findsOneWidget);
  });

  testWidgets('供应商仅提供三种类型，兼容协议均显示地址和密钥', (tester) async {
    debugDefaultTargetPlatformOverride = TargetPlatform.macOS;
    try {
      await pumpWorkspace(tester);

      await tester.tap(find.text('添加方案'));
      await tester.pumpAndSettle();

      await tester.tap(find.widgetWithText(Select<String>, 'OpenAI Codex（订阅）'));
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 400));
      expect(find.text('OpenAI 兼容协议'), findsOneWidget);
      expect(find.text('Anthropic 兼容协议'), findsOneWidget);
      expect(find.text('DeepSeek'), findsNothing);
      expect(find.text('本机 Ollama'), findsNothing);

      await tester.tap(find.text('Anthropic 兼容协议'));
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 400));
      expect(find.text('API 地址'), findsOneWidget);
      expect(find.text('API 密钥'), findsOneWidget);
      final modelField = find.widgetWithText(TextField, '输入供应商提供的模型 ID');
      await tester.enterText(modelField, 'claude-test');
      await tester.pump();
      expect(
        tester.widget<TextField>(modelField).controller?.text,
        'claude-test',
      );

      await tester.tap(find.widgetWithText(Select<String>, 'Anthropic 兼容协议'));
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 400));
      await tester.tap(find.text('OpenAI 兼容协议'));
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 400));
      expect(find.text('API 地址'), findsOneWidget);
      expect(find.text('API 密钥'), findsOneWidget);
    } finally {
      debugDefaultTargetPlatformOverride = null;
    }
  });
}
