import 'package:flutter_test/flutter_test.dart';
import 'package:avora_ui/main.dart';
import 'package:avora_ui/status_contract.dart';
import 'package:shadcn_flutter/shadcn_flutter.dart';

Map<String, dynamic> conn(String id, String state, {bool? connected}) => {
  'id': id,
  'saas_name': id,
  'state': state,
  'connected': connected ?? state == 'online',
  'gateway_connected': state == 'online',
  'remote_connected': false,
  'discovery': 'unavailable',
  'running_tasks': 0,
  'max_concurrency': 4,
  'allow': ['*'],
};

DesktopStatus st(List<Map<String, dynamic>> cs, {bool stale = false}) =>
    DesktopStatus.parse({
      'paired': true,
      'connections': cs,
      'connections_total': cs.length,
      'connections_online': cs.where((c) => c['connected'] == true).length,
      'connected': cs.any((c) => c['connected'] == true),
      if (stale) 'stale': true,
    });

Future<void> pump(WidgetTester t, DesktopStatus s) => t.pumpWidget(
  ShadcnApp(
    home: Scaffold(child: ConnectionSummaryHeader(status: s)),
  ),
);

void main() {
  final cases = <String, (DesktopStatus, String, IconData, String)>{
    'online single': (
      st([conn('a', 'online')]),
      '已连接',
      LucideIcons.circleCheck,
      '已连接 1/1',
    ),
    'online all': (
      st([conn('a', 'online'), conn('b', 'online')]),
      '已连接 · 2/2 在线',
      LucideIcons.circleCheck,
      '已连接 2/2',
    ),
    'partial': (
      st([conn('a', 'online'), conn('b', 'disconnected')]),
      '部分在线 · 1/2 在线',
      LucideIcons.triangleAlert,
      '已连接 1/2',
    ),
    'connecting': (
      st([conn('a', 'connecting')]),
      '连接中',
      LucideIcons.loader,
      '未连接 0/1',
    ),
    'disconnected': (
      st([conn('a', 'disconnected'), conn('b', 'disconnected')]),
      '已断开 · 0/2 在线',
      LucideIcons.unplug,
      '未连接 0/2',
    ),
    'revoked': (
      st([conn('a', 'revoked')]),
      '已被吊销 · 1 个错误',
      LucideIcons.ban,
      '未连接 0/1',
    ),
    'error': (
      st([conn('a', 'error'), conn('b', 'disconnected')]),
      '连接异常 · 0/2 在线 · 1 个错误',
      LucideIcons.triangleAlert,
      '未连接 0/2',
    ),
    'stale': (
      st([conn('a', 'online')], stale: true),
      '状态未知',
      LucideIcons.unplug,
      '状态未知',
    ),
  };
  cases.forEach((name, c) {
    testWidgets('dialog header: $name', (t) async {
      final (s, title, icon, bar) = c;
      await pump(t, s);
      expect(find.text(title), findsOneWidget);
      expect(find.byIcon(icon), findsOneWidget);
      expect(find.textContaining('已成功连接'), findsNothing);
      expect(s.label, bar); // status bar wording agrees
      if (!s.anyConnected) {
        expect(find.byIcon(LucideIcons.circleCheck), findsNothing);
      }
    });
  });
}
