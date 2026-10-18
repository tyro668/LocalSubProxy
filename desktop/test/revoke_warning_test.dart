import 'package:flutter_test/flutter_test.dart';
import 'package:avora_ui/main.dart';
import 'package:avora_ui/status_contract.dart';
import 'package:shadcn_flutter/shadcn_flutter.dart';

void main() {
  shellTests();
  // Review2 #2: a revoke whose key cleanup failed reports two results.
  test('revoke with cleanup failure shows removed + cleanup failed', () {
    final r = revokeOutcome({
      'removed': true,
      'connections': [],
      'warning': 'some private key files ... permission denied',
      'cleanup_errors': ['remove bindings/b_a: permission denied'],
      'cleanup_pending': ['bindings/b_a'],
    }, 'Acme');
    expect(r, hasLength(2));
    expect(r[0].text, '已移除连接「Acme」');
    expect(r[0].error, isFalse);
    expect(r[1].error, isTrue);
    expect(r[1].text, contains('私钥清理失败'));
    expect(r[1].text, contains('permission denied'));
  });

  test('clean revoke shows a single result', () {
    final r = revokeOutcome({'removed': true, 'connections': []}, 'Acme');
    expect(r.map((e) => e.text), ['已移除连接「Acme」']);
  });

  test('pending cleanup warning stays in status until retried', () {
    expect(
      DesktopStatus.parse({
        'connections': [],
        'warning': 'pending',
        'cleanup_pending': ['bindings/b_a'],
      }).cleanupWarning,
      'pending',
    );
    expect(DesktopStatus.parse({'connections': []}).cleanupWarning, isNull);
  });
}

// Review3 #4: the warning lives in the shared layout, so it stays visible
// when revoking the last connection switches the UI to the pairing page.
void shellTests() {
  testWidgets('last connection revoke cleanup failure stays visible', (
    tester,
  ) async {
    Widget page(Map<String, dynamic> status, Widget child) => ShadcnApp(
      home: AppShell(status: status, child: child),
    );
    await tester.pumpWidget(
      page({
        'paired': true,
        'connections': [
          {'id': 'b_a', 'state': 'online', 'connected': true},
        ],
      }, const Text('连接列表')),
    );
    expect(find.byKey(const ValueKey('cleanup-warning')), findsNothing);
    // kernel reply to revoking the last connection: nothing paired, cleanup failed
    await tester.pumpWidget(
      page({
        'paired': false,
        'connections': [],
        'removed': true,
        'warning': 'remove bindings/b_a: permission denied',
        'cleanup_pending': ['bindings/b_a'],
      }, const Text('输入地址')),
    );
    expect(find.text('输入地址'), findsOneWidget, reason: 'pairing page shown');
    expect(find.byKey(const ValueKey('cleanup-warning')), findsOneWidget);
    // banner counts cleanup_pending; the raw kernel text is in 查看详情
    expect(find.textContaining('有 1 个已撤销连接'), findsOneWidget);
    await tester.tap(find.byKey(const ValueKey('cleanup-warning-details')));
    await tester.pumpAndSettle();
    expect(find.textContaining('permission denied'), findsOneWidget);
    await tester.tap(find.text('知道了'));
    await tester.pumpAndSettle();
    // cleared once a retry at start succeeds
    await tester.pumpWidget(
      page({'paired': false, 'connections': []}, const Text('输入地址')),
    );
    expect(find.byKey(const ValueKey('cleanup-warning')), findsNothing);
  });

  // Review4 A: a warning appearing must not rebuild the page below.
  testWidgets('draft text survives the warning bar appearing', (tester) async {
    Widget page(Map<String, dynamic> status) => ShadcnApp(
      home: AppShell(
        status: status,
        child: const Center(
          child: SizedBox(width: 300, child: TextField(key: ValueKey('draft'))),
        ),
      ),
    );
    await tester.pumpWidget(page({'connections': []}));
    await tester.enterText(
      find.byKey(const ValueKey('draft')),
      'unsaved model',
    );
    await tester.pump();
    await tester.pumpWidget(
      page({'connections': [], 'warning': 'cleanup failed'}),
    );
    await tester.pump();
    expect(find.byKey(const ValueKey('cleanup-warning')), findsOneWidget);
    expect(find.text('unsaved model'), findsOneWidget);
    await tester.pumpWidget(page({'connections': []}));
    expect(find.text('unsaved model'), findsOneWidget);
  });

  // Review4 C: approve failure → retry on the same consent page, or restart.
  test('approve failure: retry while the request is kept, else restart', () {
    expect(approveRetryable({'state': 'consent', 'pair_id': 'p1'}), isTrue);
    expect(approveRetryable({'state': 'failed'}), isFalse);
    expect(approveRetryable({'state': 'idle'}), isFalse);
    expect(approveRetryable(null), isFalse);
  });
}
