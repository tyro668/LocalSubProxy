import 'dart:async';

import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:avora_ui/main.dart';
import 'package:avora_ui/native_api.dart';
import 'package:shadcn_flutter/shadcn_flutter.dart';

// HomePage dialog flows driven through the real widgets, with the kernel
// replaced by NativeApi.debugTransport (same JSON actions as AVORA_Call).

class FakeKernel {
  Map<String, dynamic> status = {'paired': false, 'device_name': '测试电脑'};
  List<Map<String, dynamic>> adapters = [];
  final calls = <String>[];
  String pairState = 'pending';

  dynamic call(Map<String, dynamic> p) {
    final a = p['action'] as String;
    calls.add(a);
    switch (a) {
      case 'start':
        return <String, dynamic>{};
      case 'status':
        return status;
      case 'list_adapters':
        return {'adapters': adapters};
      case 'pair_prepare':
        return {
          'state': 'consent',
          'pair_id': 'p1',
          'consent': {
            'saas': {'name': 'Acme SaaS'},
            'verified': true,
          },
          'available_models': <String>[],
        };
      case 'pair_approve':
        return {'user_code': 'ABCD-1234', 'link_url': 'https://acme/link'};
      case 'pair_status':
        return {'state': pairState};
      case 'adapter_recovery_ack':
        status = {...status}..remove('adapter_recovery');
        return null;
      case 'create_adapter':
        adapters = [...adapters, (p['adapter'] as Map).cast<String, dynamic>()];
        return p['adapter'];
      default:
        return <String, dynamic>{};
    }
  }
}

Map<String, dynamic> connection() => {
  'id': 'b_1',
  'saas_name': 'Acme SaaS',
  'tenant': 't1',
  'state': 'online',
  'connected': true,
  'paused': false,
  'remote_url': 'https://acme.example.com',
  'server_url': 'https://gw.example.com',
  'running_tasks': 0,
  'max_concurrency': 4,
  'allow': ['*'],
  'transport': 'wss',
  'e2ee': true,
};

Finder dialogs() => find.byKey(const ValueKey('app-dialog'));

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  late FakeKernel kernel;

  setUp(() {
    kernel = FakeKernel();
    NativeApi.debugTransport = kernel.call;
    final m = TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger;
    for (final ch in ['window_manager', 'avora/tray']) {
      m.setMockMethodCallHandler(MethodChannel(ch), (_) async => null);
    }
  });
  tearDown(() {
    NativeApi.debugTransport = null;
    NativeApi.debugGate = null;
  });

  Future<void> pumpHome(WidgetTester tester) async {
    tester.view.physicalSize = const Size(1400, 900);
    tester.view.devicePixelRatio = 1;
    addTearDown(tester.view.resetPhysicalSize);
    addTearDown(tester.view.resetDevicePixelRatio);
    await tester.pumpWidget(
      const ShadcnApp(debugShowCheckedModeBanner: false, home: HomePage()),
    );
    await tester.pumpAndSettle();
  }

  // dispose HomePage (cancels its 2 s poll timer)
  Future<void> unmount(WidgetTester tester) async {
    await tester.pump(const Duration(seconds: 6)); // let toasts expire
    await tester.pumpWidget(const SizedBox());
    await tester.pump(const Duration(seconds: 3));
  }

  testWidgets('pairing switches steps 0→1→2→3 inside the open dialog', (
    tester,
  ) async {
    await pumpHome(tester);
    await tester.tap(find.text('未连接'));
    await tester.pumpAndSettle();
    expect(find.text('远端服务连接'), findsOneWidget);
    expect(find.text('输入地址'), findsOneWidget); // step rail visible

    await tester.enterText(
      find.byType(TextField).last,
      'https://acme.example.com',
    );
    await tester.tap(find.text('连接'));
    await tester.pumpAndSettle();
    expect(kernel.calls, contains('pair_prepare'));
    expect(find.text('允许'), findsOneWidget); // step 1 (consent)
    expect(find.textContaining('Acme SaaS'), findsWidgets);

    await tester.tap(find.text('允许'));
    await tester.pumpAndSettle();
    expect(find.text('允许'), findsNothing);
    expect(find.textContaining('ABCD-1234'), findsWidgets); // step 2 (code)

    kernel.pairState = 'paired';
    kernel.status = {
      'paired': true,
      'device_name': '测试电脑',
      'connections': [connection()],
    };
    await tester.pump(const Duration(seconds: 2)); // poll timer
    await tester.pumpAndSettle();
    expect(find.textContaining('ABCD-1234'), findsNothing);
    expect(find.text('输入地址'), findsNothing); // step 3: connection list
    // 撤销 sits in the collapsed card details
    expect(find.text('Acme SaaS'), findsWidgets);
    await tester.tap(find.byKey(const ValueKey('conn-expand-b_1')));
    await tester.pumpAndSettle();
    expect(find.text('撤销'), findsOneWidget);
    expect(dialogs(), findsOneWidget, reason: 'same dialog, content switched');
    await unmount(tester);
  });

  testWidgets('nested dialog: inner close only closes the inner dialog', (
    tester,
  ) async {
    kernel.status = {
      'paired': true,
      'device_name': '测试电脑',
      'connections': [connection()],
    };
    await pumpHome(tester);
    await tester.tap(find.byKey(const ValueKey('status-pill-ok')));
    await tester.pumpAndSettle();
    expect(dialogs(), findsOneWidget);
    await tester.tap(
      find.descendant(of: dialogs(), matching: find.text('模型')).first,
    );
    await tester.pumpAndSettle();
    expect(dialogs(), findsNWidgets(2));
    expect(find.text('可用模型'), findsOneWidget);
    expect(find.textContaining('Acme SaaS · 选择这个连接能调用的模型'), findsOneWidget);

    await tester.tap(find.byKey(const ValueKey('app-dialog-close')).last);
    await tester.pumpAndSettle();
    expect(dialogs(), findsOneWidget);
    expect(find.text('远端服务连接'), findsOneWidget);

    await tester.tap(find.byKey(const ValueKey('app-dialog-close')));
    await tester.pumpAndSettle();
    expect(dialogs(), findsNothing);
    await unmount(tester);
  });

  testWidgets('adapter recovery prompt shows once and is acknowledged', (
    tester,
  ) async {
    kernel.status = {
      'paired': false,
      'device_name': '测试电脑',
      'adapter_recovery': {'backup': '/tmp/adapters.bak'},
    };
    await pumpHome(tester);
    await tester.tap(find.text('未连接'));
    await tester.pumpAndSettle();
    expect(find.text('旧的模型方案配置已备份'), findsOneWidget);
    expect(find.textContaining('/tmp/adapters.bak'), findsOneWidget);

    await tester.tap(find.text('知道了'));
    await tester.pumpAndSettle();
    expect(find.text('旧的模型方案配置已备份'), findsNothing);
    expect(kernel.calls, contains('adapter_recovery_ack'));

    // later status polls do not bring it back
    await tester.pump(const Duration(seconds: 2));
    await tester.pumpAndSettle();
    expect(find.text('旧的模型方案配置已备份'), findsNothing);
    await unmount(tester);
  });

  testWidgets('adapter dialog cannot be closed while saving', (tester) async {
    tester.view.physicalSize = const Size(1400, 900);
    tester.view.devicePixelRatio = 1;
    addTearDown(tester.view.resetPhysicalSize);
    addTearDown(tester.view.resetDevicePixelRatio);
    final gate = Completer<void>();
    bool? result;
    await tester.pumpWidget(
      ShadcnApp(
        debugShowCheckedModeBanner: false,
        home: Builder(
          builder: (context) => Center(
            child: PrimaryButton(
              onPressed: () async {
                result = await showOverlay<bool>(
                  context,
                  DialogConfiguration<bool>(
                    builder: (_) => AdapterDialog(api: NativeApi.instance),
                  ),
                ).future;
              },
              child: const Text('open'),
            ),
          ),
        ),
      ),
    );
    await tester.tap(find.text('open'));
    await tester.pumpAndSettle();
    await tester.enterText(find.byType(TextField).first, 'cx');

    NativeApi.debugGate = () => gate.future; // hold create_adapter in flight
    await tester.tap(find.text('保存'));
    await tester.pump();
    expect(find.text('保存中…'), findsOneWidget);

    // X, Escape and barrier taps must all be ignored while saving
    await tester.tap(find.byKey(const ValueKey('app-dialog-close')));
    await tester.pump();
    await tester.sendKeyEvent(LogicalKeyboardKey.escape);
    await tester.pump();
    await tester.tapAt(const Offset(5, 5));
    await tester.pumpAndSettle();
    expect(dialogs(), findsOneWidget, reason: 'dialog closed mid-save');

    gate.complete();
    await tester.pumpAndSettle();
    expect(kernel.calls, contains('create_adapter'));
    expect(dialogs(), findsNothing);
    expect(result, isTrue, reason: 'caller must learn the save succeeded');
  });
}
