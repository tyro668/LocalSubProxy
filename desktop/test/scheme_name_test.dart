import 'dart:convert';
import 'package:flutter_test/flutter_test.dart';
import 'package:localsubproxy_ui/main.dart';
import 'package:localsubproxy_ui/status_contract.dart';
import 'package:shadcn_flutter/shadcn_flutter.dart';

// Shaped like real ListAdapters output (AdapterView JSON).
final listAdapters = <Map<String, dynamic>>[
  {
    'id': 'draft_1791957193873254',
    'type': 'codex_app_server',
    'name': 'gpt-6.1-sol',
    'models': ['gpt-6.1-sol'],
    'is_default': true,
    'enabled': true,
    'has_api_key': false,
    'health': 'ready',
    'auth_state': 'ok',
  },
  {
    'id': 'profile_1791957300000000',
    'type': 'openai_compatible',
    'name': 'deepseek-flash',
    'provider': 'openai',
    'base_url': 'https://api.deepseek.com/v1',
    'models': ['deepseek-flash'],
    'is_default': false,
    'enabled': true,
    'has_api_key': true,
    'health': 'ready',
    'auth_state': 'ok',
  },
];

void main() {
  test('allow-list rows show scheme names, ids stay stored values', () {
    final e = policyEntries(listAdapters, ['draft_1791957193873254/*']);
    expect(e.map((x) => x.id).toList(), [
      'draft_1791957193873254/*',
      'draft_1791957193873254/gpt-6.1-sol',
      'profile_1791957300000000/deepseek-flash',
    ]);
    expect(e.map((x) => x.label).toList(), [
      'gpt-6.1-sol · 全部模型',
      'gpt-6.1-sol',
      'deepseek-flash',
    ]);
    expect(e[1].detail, 'gpt-6.1-sol');
    expect(
      allowSummary(['profile_1791957300000000/deepseek-flash'], listAdapters),
      'deepseek-flash',
    );
  });

  test('legacy duplicate names are disambiguated for display', () {
    final dup = [
      {...listAdapters[0], 'name': 'same'},
      {...listAdapters[1], 'name': 'Same '},
    ];
    final l = schemeLabels(dup);
    expect(
      l['draft_1791957193873254/gpt-6.1-sol'],
      'same (draft_1791957193873254)',
    );
    expect(
      l['profile_1791957300000000/deepseek-flash'],
      'Same (profile_1791957300000000)',
    );
  });

  test('default name is model, then <model>_<smallest free n>=2>', () {
    final taken = listAdapters.map((a) => '${a['name']}');
    expect(defaultSchemeName('gpt-6.1-sol', taken), 'gpt-6.1-sol_2');
    expect(defaultSchemeName('gpt-6.1-sol', ['x']), 'gpt-6.1-sol');
    expect(defaultSchemeName('gpt-6.1-sol', ['GPT-6.1-SOL ']), 'gpt-6.1-sol_2');
    expect(defaultSchemeName('m', ['M', 'm_2', 'm_4']), 'm_3');
  });

  test('review9 P2-3: default names fit 100 UTF-8 bytes, suffix kept whole', () {
    final m = '模' * 60; // 180 bytes
    final first = defaultSchemeName(m, const []);
    final second = defaultSchemeName(m, [first]);
    expect(utf8.encode(first).length, lessThanOrEqualTo(maxSchemeNameBytes));
    expect(utf8.encode(second).length, lessThanOrEqualTo(maxSchemeNameBytes));
    expect(second.endsWith('_2'), isTrue);
    expect(schemeNameError('名' * 34, const []), isNotNull); // 102 bytes
    expect(schemeNameError('名' * 33, const []), isNull); // 99 bytes
  });

  test('duplicate names are rejected inline (trimmed, case-insensitive)', () {
    expect(schemeNameError(' GPT-6.1-SOL ', listAdapters), isNotNull);
    expect(
      schemeNameError(
        'gpt-6.1-sol',
        listAdapters,
        exceptId: 'draft_1791957193873254',
      ),
      isNull,
    );
    expect(schemeNameError('new', listAdapters), isNull);
  });

  testWidgets('model dialog shows the scheme name, not the raw id', (
    tester,
  ) async {
    final entries = policyEntries(listAdapters, const []);
    await tester.pumpWidget(
      ShadcnApp(
        home: ModelChecklist(
          models: [for (final e in entries) e.id],
          labels: {for (final e in entries) e.id: e.label},
          details: {for (final e in entries) e.id: e.detail},
          selected: const {},
          onChanged: (_, _) {},
        ),
      ),
    );
    expect(find.text('gpt-6.1-sol'), findsOneWidget);
    expect(find.text('deepseek-flash'), findsOneWidget);
    expect(find.text('draft_1791957193873254/gpt-6.1-sol'), findsNothing);
  });
}
