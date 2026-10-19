import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:avora_ui/main.dart';
import 'package:avora_ui/native_api.dart';
import 'package:avora_ui/status_contract.dart';
import 'package:shadcn_flutter/shadcn_flutter.dart';

// 允许全部模型: the switch above the model list in the 可用模型 dialog and
// on the pairing consent page, driven through the real HomePage with the
// kernel replaced by NativeApi.debugTransport.

class Kernel {
  Kernel(List<String> allow)
    : status = {
        'paired': true,
        'device_name': '测试电脑',
        'connections': [
          {
            'id': 'b_1',
            'saas_name': 'Acme SaaS',
            'tenant': 't1',
            'state': 'online',
            'connected': true,
            'paused': false,
            'running_tasks': 0,
            'max_concurrency': 4,
            'allow': allow,
          },
        ],
      };
  Map<String, dynamic> status;
  final adapters = <Map<String, dynamic>>[
    {
      'id': 'codex',
      'name': 'Codex',
      'type': 'codex_app_server',
      'models': ['m1', 'm2'],
      'enabled': true,
    },
    {
      'id': 'ollama',
      'name': 'Ollama',
      'type': 'ollama_http',
      'models': ['q1'],
      'enabled': true,
    },
  ];
  List<String>? saved, approved;

  dynamic call(Map<String, dynamic> p) {
    switch (p['action'] as String) {
      case 'status':
        return status;
      case 'list_adapters':
        return {'adapters': adapters};
      case 'connection_policy_set':
        saved = ((p['policy'] as Map)['allow'] as List).cast<String>();
        return <String, dynamic>{};
      case 'pair_prepare':
        return {
          'state': 'consent',
          'pair_id': 'p1',
          'consent': {
            'saas': {
              'name': 'Acme Chat',
              'homepage_uri': 'https://chat.acme.example',
            },
            'gateway': 'https://gw.acme.example',
            'tenant': 'acme',
            'gateway_key_id': 'gk_1',
            'verified': true,
          },
          'available_models': ['codex/m1', 'codex/m2', 'ollama/q1'],
        };
      case 'pair_approve':
        approved = (p['allow'] as List).cast<String>();
        return {'user_code': 'ABCD-1234', 'link_url': 'https://acme/link'};
      case 'pair_status':
        return {'state': 'pending'};
      default:
        return <String, dynamic>{};
    }
  }
}

final allowSwitch = find.byKey(const ValueKey('allow-all-switch'));
final locked = find.byKey(const ValueKey('model-locked'));

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  late Kernel kernel;

  setUp(() {
    final m = TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger;
    for (final ch in ['window_manager', 'avora/tray']) {
      m.setMockMethodCallHandler(MethodChannel(ch), (_) async => null);
    }
  });
  tearDown(() => NativeApi.debugTransport = null);

  Future<void> pumpHome(WidgetTester t, Kernel k) async {
    kernel = k;
    NativeApi.debugTransport = k.call;
    t.view.physicalSize = const Size(1400, 900);
    t.view.devicePixelRatio = 1;
    addTearDown(t.view.resetPhysicalSize);
    addTearDown(t.view.resetDevicePixelRatio);
    await t.pumpWidget(
      const ShadcnApp(debugShowCheckedModeBanner: false, home: HomePage()),
    );
    await t.pumpAndSettle();
  }

  Future<void> tap(WidgetTester t, Finder f) async {
    await t.tap(f);
    await t.pumpAndSettle();
  }

  Future<void> unmount(WidgetTester t) async {
    await t.pump(const Duration(seconds: 6));
    await t.pumpWidget(const SizedBox());
    await t.pump(const Duration(seconds: 3));
  }

  Future<void> openModels(WidgetTester t, List<String> allow) async {
    await pumpHome(t, Kernel(allow));
    await tap(t, find.byKey(const ValueKey('status-pill-ok')));
    await tap(
      t,
      find
          .descendant(
            of: find.byKey(const ValueKey('app-dialog')),
            matching: find.text('模型'),
          )
          .first,
    );
    expect(find.text('可用模型'), findsOneWidget);
    expect(find.text('Acme SaaS · 选择这个连接能调用的模型'), findsOneWidget);
  }

  bool checked(WidgetTester t, String id) => t
      .widget<AppCheckbox>(
        find.descendant(
          of: find.byKey(ValueKey('model-$id')),
          matching: find.byType(AppCheckbox),
        ),
      )
      .checked;

  testWidgets('switch on saves exactly ["*"] and locks the list', (t) async {
    await openModels(t, ['codex/m1']);
    expect(find.text('允许全部模型'), findsOneWidget);
    expect(find.byKey(const ValueKey('allow-all-tag')), findsNothing);
    expect(find.text('已选 1 / 3'), findsOneWidget);
    expect(locked, findsNothing);
    await tap(t, allowSwitch);
    expect(find.byKey(const ValueKey('allow-all-tag')), findsOneWidget);
    expect(find.text('已选 3 / 3'), findsOneWidget);
    expect(locked, findsOneWidget);
    expect(checked(t, 'codex/m2'), isTrue);
    // rows are read-only while the switch is on
    await tap(t, find.byKey(const ValueKey('model-codex/m2')));
    expect(checked(t, 'codex/m2'), isTrue);
    await tap(t, find.text('保存'));
    expect(kernel.saved, ['*']);
    await unmount(t);
  });

  testWidgets('switch off restores the selection made before', (t) async {
    await openModels(t, ['codex/m1']);
    await tap(t, find.byKey(const ValueKey('model-ollama/q1')));
    await tap(t, allowSwitch);
    expect(find.text('已选 3 / 3'), findsOneWidget);
    await tap(t, allowSwitch);
    expect(locked, findsNothing);
    expect(find.text('已选 2 / 3'), findsOneWidget);
    expect(checked(t, 'codex/m2'), isFalse);
    await tap(t, find.text('保存'));
    expect(kernel.saved, ['codex/m1', 'ollama/q1']);
    await unmount(t);
  });

  testWidgets('a "*" policy opens with the switch on and no "*" row', (
    t,
  ) async {
    await openModels(t, ['*']);
    expect(t.widget<AppSwitch>(allowSwitch).value, isTrue);
    expect(locked, findsOneWidget);
    expect(find.byKey(const ValueKey('model-*')), findsNothing);
    expect(find.text('全部本机模型'), findsNothing);
    // turning it off starts with every model checked
    await tap(t, allowSwitch);
    expect(find.text('已选 3 / 3'), findsOneWidget);
    expect(locked, findsNothing);
    await tap(t, find.byKey(const ValueKey('model-codex/m2')));
    await tap(t, find.text('保存'));
    expect(kernel.saved, ['codex/m1', 'ollama/q1']);
    await unmount(t);
  });

  testWidgets('scheme wildcards are kept and lock the rows they cover', (
    t,
  ) async {
    await openModels(t, ['codex/m1', 'ollama/*']);
    expect(checked(t, 'ollama/q1'), isTrue);
    expect(find.text('已由通配授权'), findsOneWidget);
    await tap(t, find.byKey(const ValueKey('model-codex/m2')));
    await tap(t, find.text('保存'));
    expect(kernel.saved, ['codex/m1', 'codex/m2', 'ollama/*']);
    await unmount(t);
  });

  testWidgets('consent page: 允许 with the switch on sends allow ["*"]', (
    t,
  ) async {
    final k = Kernel(const [])
      ..status = {'paired': false, 'device_name': '测试电脑'};
    await pumpHome(t, k);
    await tap(t, find.text('未连接'));
    await t.enterText(find.byType(TextField).last, 'https://chat.acme.example');
    await tap(t, find.text('连接'));
    expect(find.text('允许 Acme Chat 连接'), findsOneWidget);
    expect(find.text('chat.acme.example'), findsOneWidget);
    expect(find.text('可调用的模型'), findsOneWidget);
    expect(find.text('已选 0 / 3'), findsOneWidget);
    await tap(t, find.byKey(const ValueKey('model-codex/m1')));
    await tap(t, allowSwitch);
    expect(find.text('已选 3 / 3'), findsOneWidget);
    expect(locked, findsOneWidget);
    await tap(t, find.text('允许'));
    expect(kernel.approved, ['*']);
    expect(find.textContaining('ABCD-1234'), findsWidgets);
    await unmount(t);
  });

  test('ModelAccess keeps the per-model selection under "*"', () {
    final a = ModelAccess(['codex/m1'], models: ['codex/m1', 'codex/m2']);
    a.all = true;
    expect(a.allow, ['*']);
    expect(a.covers('codex/m2'), isTrue);
    a.all = false;
    expect(a.allow, ['codex/m1']);
    final b = ModelAccess(['*'], models: ['codex/m1', 'ollama/*']);
    expect(b.all, isTrue);
    b.all = false;
    expect(b.allow, ['codex/m1']);
  });
}
