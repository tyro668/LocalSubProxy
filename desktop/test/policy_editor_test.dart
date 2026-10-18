import 'package:flutter_test/flutter_test.dart';
import 'package:avora_ui/main.dart';
import 'package:avora_ui/native_api.dart';
import 'package:avora_ui/profile_workspace.dart';
import 'package:avora_ui/status_contract.dart';
import 'package:shadcn_flutter/shadcn_flutter.dart';

const adapters = <Map<String, dynamic>>[
  {
    'id': 'ollama',
    'models': ['qwen3', 'llama3'],
    'enabled': true,
  },
  {
    'id': 'codex',
    'models': ['gpt-5'],
    'enabled': false,
  },
];

void main() {
  // Review #4: saving permissions must not drop disabled models, wildcards
  // or entries for models that no longer exist.
  test(
    'policy editor lists disabled models, wildcards and unknown entries',
    () {
      final e = policyEntries(adapters, ['*', 'codex/gpt-5', 'gone/old']);
      final ids = e.map((x) => x.id).toList();
      expect(ids, [
        '*',
        'ollama/qwen3',
        'ollama/llama3',
        'codex/gpt-5',
        'gone/old',
      ]);
      expect(e.firstWhere((x) => x.id == 'codex/gpt-5').note, contains('停用'));
      expect(e.firstWhere((x) => x.id == 'gone/old').note, contains('保留'));
      expect(e.first.isWildcard, isTrue);
      // unchanged selection == unchanged allow: nothing is lost or expanded
      final selected = {'*', 'codex/gpt-5', 'gone/old'};
      expect(ids.where(selected.contains).toSet(), selected);
    },
  );

  test('wildcards lock covered models instead of snapshotting them', () {
    expect(coveredByWildcard('ollama/qwen3', {'ollama/*'}), isTrue);
    expect(coveredByWildcard('codex/gpt-5', {'ollama/*'}), isFalse);
    expect(coveredByWildcard('codex/gpt-5', {'*'}), isTrue);
    expect(coveredByWildcard('ollama/*', {'*'}), isFalse);
    final e = policyEntries(adapters, ['ollama/*']);
    expect(e.first.id, 'ollama/*');
  });

  test('policy save sends allow only (max_concurrency untouched)', () {
    final r = NativeApi.policyRequest('b_1', allow: ['ollama/*']);
    expect(r['policy'], {
      'allow': ['ollama/*'],
    });
    expect((r['policy'] as Map).containsKey('max_concurrency'), isFalse);
    expect(NativeApi.policyRequest('b_1', maxConcurrency: 2)['policy'], {
      'max_concurrency': 2,
    });
  });

  testWidgets('locked rows are checked and read-only', (tester) async {
    final selected = <String>{'ollama/*'};
    await tester.pumpWidget(
      ShadcnApp(
        home: StatefulBuilder(
          builder: (context, setState) => ModelChecklist(
            models: const ['ollama/*', 'ollama/qwen3', 'codex/gpt-5'],
            notes: const {'codex/gpt-5': '方案已停用（授权保留）'},
            selected: selected,
            lockedBy: (m) => coveredByWildcard(m, selected),
            onChanged: (m, v) =>
                setState(() => v ? selected.add(m) : selected.remove(m)),
          ),
        ),
      ),
    );
    // notes render as badges next to the model id
    expect(find.text('ollama/qwen3'), findsOneWidget);
    expect(find.text('已由通配授权'), findsOneWidget);
    expect(find.text('已停用（授权保留）'), findsOneWidget);
    await tester.tap(find.text('ollama/qwen3'));
    await tester.pumpAndSettle();
    expect(selected, {
      'ollama/*',
    }, reason: 'covered row must not be snapshotted');
    await tester.tap(find.text('ollama/*'));
    await tester.pumpAndSettle();
    expect(selected, isEmpty);
    expect(
      find.text('已由通配授权'),
      findsNothing,
      reason: 'unlocked after the wildcard is removed',
    );
  });

  // Gap (a): a failed refresh must not keep showing connections as online.
  test('stale status shows unknown state', () {
    final s = DesktopStatus.parse({
      'stale': true,
      'connections': [
        {'id': 'b_1', 'state': 'online', 'connected': true},
      ],
    });
    expect(s.online, 0);
    expect(s.connections.single.state, 'unknown');
    expect(s.connections.single.stateLabel, '状态未知');
    expect(s.label, '状态未知');
  });

  testWidgets('status bar shows 状态未知 when stale', (tester) async {
    tester.view.physicalSize = const Size(1500, 900);
    tester.view.devicePixelRatio = 1;
    addTearDown(tester.view.resetPhysicalSize);
    addTearDown(tester.view.resetDevicePixelRatio);
    await tester.pumpWidget(
      ShadcnApp(
        home: SizedBox.expand(
          child: ProfileWorkspace(
            api: NativeApi.instance,
            status: const {
              'paired': true,
              'connected': false,
              'stale': true,
              'connections': [
                {'id': 'b_1', 'state': 'online', 'connected': true},
              ],
            },
            adapters: const [],
            onChanged: () async {},
          ),
        ),
      ),
    );
    await tester.pumpAndSettle();
    expect(find.text('状态未知'), findsOneWidget);
    expect(find.textContaining('已连接'), findsNothing);
  });
}
