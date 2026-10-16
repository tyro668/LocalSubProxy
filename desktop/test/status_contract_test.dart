import 'package:flutter_test/flutter_test.dart';
import 'package:localsubproxy_ui/main.dart';
import 'package:localsubproxy_ui/native_api.dart';
import 'package:localsubproxy_ui/profile_workspace.dart';
import 'package:localsubproxy_ui/status_contract.dart';
import 'package:shadcn_flutter/shadcn_flutter.dart';

// Sample from docs/desktop-status-contract.md (kernel `status`, multi-SaaS).
const multi = <String, dynamic>{
  'protocol': 'lmbp',
  'paired': true,
  'connected': true,
  'device_name': '测试电脑',
  'running_tasks': 3,
  'max_concurrency': 4,
  'connections_total': 2,
  'connections_online': 1,
  'tenant': 'acme',
  'connections': [
    {
      'id': 'b_7f3a91',
      'saas_name': 'Acme Chat',
      'issuer': 'https://gw.acme.example',
      'tenant': 'acme',
      'device_id': 'dev_1',
      'state': 'online',
      'connected': true,
      'running_tasks': 2,
      'max_concurrency': 2,
      'allow': ['ollama/*', 'openai/gpt-4o-mini'],
      'paused': false,
      'last_error': '',
      'transport': 'wss',
      'e2ee': true,
    },
    {
      'id': 'b_c20e44',
      'saas_name': 'Beta Notes',
      'issuer': 'https://lmbp.beta.example',
      'tenant': 't_9921',
      'device_id': 'dev_1',
      'state': 'revoked',
      'connected': false,
      'running_tasks': 1,
      'max_concurrency': 1,
      'allow': <String>[],
      'last_error':
          'device credential rejected (revoked or unknown); pair again',
    },
  ],
};

void main() {
  test('parses connections[] and counts online/total', () {
    final s = DesktopStatus.parse(multi);
    expect(s.multi, isTrue);
    expect(s.total, 2);
    expect(s.online, 1);
    expect(s.label, '已连接 1/2');
    final a = s.connections.first;
    expect(a.title, 'Acme Chat');
    expect(a.allows('ollama/qwen3'), isTrue);
    expect(a.allows('openai/gpt-4o-mini'), isTrue);
    expect(a.allows('openai/gpt-5'), isFalse);
    final b = s.connections.last;
    expect(b.stateLabel, '已吊销');
    expect(
      b.allows('ollama/qwen3'),
      isFalse,
      reason: 'empty allow-list denies all',
    );
  });

  test('legacy flat status maps to one connection', () {
    final s = DesktopStatus.parse(const {
      'paired': true,
      'connected': true,
      'saas_name': 'Old',
      'tenant': 'acme',
    });
    expect(s.multi, isFalse);
    expect(s.total, 1);
    expect(s.label, '已连接');
    expect(s.connections.single.allows('x/y'), isTrue);
    expect(DesktopStatus.parse(null).label, '未连接');
    expect(DesktopStatus.parse(const {'paired': false}).paired, isFalse);
  });

  test('no connections yet', () {
    final s = DesktopStatus.parse(const {'paired': false, 'connections': []});
    expect(s.multi, isTrue);
    expect(s.label, '未连接');
  });

  testWidgets('status bar shows 已连接 n/m', (tester) async {
    tester.view.physicalSize = const Size(1500, 900);
    tester.view.devicePixelRatio = 1;
    addTearDown(tester.view.resetPhysicalSize);
    addTearDown(tester.view.resetDevicePixelRatio);
    await tester.pumpWidget(
      ShadcnApp(
        home: SizedBox.expand(
          child: ProfileWorkspace(
            api: NativeApi.instance,
            status: multi,
            adapters: const [],
            onChanged: () async {},
          ),
        ),
      ),
    );
    await tester.pumpAndSettle();
    expect(find.text('部分在线 1/2 · 1 个错误'), findsOneWidget);
    expect(find.byKey(const ValueKey('status-pill-warn')), findsOneWidget);
    expect(find.text('3 个任务'), findsOneWidget);
  });

  testWidgets('consent model checklist toggles models', (tester) async {
    final selected = <String>{};
    await tester.pumpWidget(
      ShadcnApp(
        home: StatefulBuilder(
          builder: (context, setState) => ModelChecklist(
            models: const ['local/qwen3-8b', 'codex/gpt-5'],
            selected: selected,
            onChanged: (m, v) =>
                setState(() => v ? selected.add(m) : selected.remove(m)),
          ),
        ),
      ),
    );
    expect(find.text('local/qwen3-8b'), findsOneWidget);
    await tester.tap(find.text('codex/gpt-5'));
    await tester.pumpAndSettle();
    expect(selected, {'codex/gpt-5'});
    await tester.tap(find.text('codex/gpt-5'));
    await tester.pumpAndSettle();
    expect(selected, isEmpty);
  });
}
