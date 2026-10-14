// Pins the status contract between the native kernel and this UI
// (docs/desktop-status-contract.json; the Go side is pinned by
// cmd/localsubproxy_native/status_contract_test.go). Fails when the UI reads
// a status field, or compares a transport / pairing state value, that the
// kernel does not promise.
import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';

void main() {
  final contract =
      jsonDecode(File('../docs/desktop-status-contract.json').readAsStringSync())
          as Map<String, dynamic>;
  final status = (contract['status'] as Map).cast<String, dynamic>();
  final optional = (contract['optional'] as Map).cast<String, dynamic>();
  final pairStates = (contract['pair_status_state'] as List).cast<String>();
  final sources = Directory('lib')
      .listSync(recursive: true)
      .whereType<File>()
      .where((f) => f.path.endsWith('.dart'))
      .map((f) => MapEntry(f.path, f.readAsStringSync()))
      .toList();

  test('every status field the UI reads is in the contract', () {
    final read = RegExp(r"status\??\[\s*'([a-z_]+)'\s*\]");
    final missing = <String>{};
    var count = 0;
    for (final src in sources) {
      for (final m in read.allMatches(src.value)) {
        count++;
        final f = m.group(1)!;
        if (!status.containsKey(f) && !optional.containsKey(f)) {
          missing.add('${src.key}: $f');
        }
      }
    }
    expect(count, greaterThan(5), reason: 'scanner found no status reads');
    expect(missing, isEmpty);
  });

  test('connection state comes from the boolean "connected" field', () {
    expect(status['connected'], 'bool');
    expect(status['paired'], 'bool');
    final ui = sources.map((e) => e.value).join('\n');
    expect(ui, contains("status?['connected'] == true"));
  });

  test('transport labels match the transport values', () {
    final transports = (status['transport'] as List).cast<String>();
    final main = File('lib/main.dart').readAsStringSync();
    final fn = RegExp(
      r'_transportLabel\(Object\? transport\) => switch \(transport\) \{([^}]*)\}',
    ).firstMatch(main);
    expect(fn, isNotNull);
    final cases = RegExp(r"'([a-z]+)' =>")
        .allMatches(fn!.group(1)!)
        .map((m) => m.group(1)!)
        .toList();
    expect(cases, isNotEmpty);
    for (final c in cases) {
      expect(transports, contains(c));
    }
  });

  test('pairing states compared in the UI are ones the kernel reports', () {
    final cmp = RegExp(r"\['state'\]\s*==\s*'([a-z_]+)'");
    for (final src in sources) {
      for (final m in cmp.allMatches(src.value)) {
        expect(pairStates, contains(m.group(1)), reason: src.key);
      }
    }
    for (final s in status['pairing_state'] as List) {
      expect(pairStates, contains(s));
    }
  });
}
