// Drives the real native kernel library (LSP_NATIVE_LIB) against the mock
// gateways (LSP_ACCEPT_MOCK_DIR, LSP_ACCEPT_PORT). Skipped when not set.
//
//   LSP_NATIVE_LIB=/tmp/liblsp.so LSP_ACCEPT_MOCK_DIR=/tmp/msmock \
//   LSP_ACCEPT_PORT=18480 flutter test test/native_real_test.dart
import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:avora_ui/native_api.dart';
import 'package:avora_ui/profile_workspace.dart';
import 'package:avora_ui/status_contract.dart';

final env = Platform.environment;
final enabled =
    (env['LSP_NATIVE_LIB'] ?? '').isNotEmpty &&
    (env['LSP_ACCEPT_MOCK_DIR'] ?? '').isNotEmpty;

Future<void> approve(String gw, String key, String code) async {
  final c = HttpClient();
  final r = await c.postUrl(Uri.parse('$gw/lmbp/v1/tenant/devices/approvals'));
  r.headers
    ..set('Authorization', 'Bearer $key')
    ..set('X-End-User', 'alice')
    ..contentType = ContentType.json;
  r.write(jsonEncode({'user_code': code}));
  final resp = await r.close();
  await resp.drain<void>();
  c.close();
  expect(resp.statusCode, 200);
}

Future<Map<String, dynamic>> until(
  NativeApi api,
  bool Function(Map<String, dynamic>) ok,
) async {
  for (var i = 0; i < 150; i++) {
    final s = api.status();
    if (ok(s)) return s;
    await Future<void>.delayed(const Duration(milliseconds: 200));
  }
  fail('timeout: ${api.status()}');
}

void main() {
  test(
    'real kernel: pair two SaaS, edit permissions, revoke with cleanup failure, recover',
    () async {
      final mock = env['LSP_ACCEPT_MOCK_DIR']!;
      final port = int.parse(env['LSP_ACCEPT_PORT'] ?? '18480');
      final kv = {
        for (final l in File('$mock/env').readAsLinesSync())
          if (l.contains('='))
            l.split('=').first: l.split('=').skip(1).join('='),
      };
      final saas = [
        ('acme', 'http://127.0.0.1:$port', kv['LSP_TENANT_KEY_ACME']!),
        ('beta', 'http://127.0.0.1:${port + 1}', kv['LSP_TENANT_KEY_BETA']!),
      ];
      final data = Directory.systemTemp.createTempSync('lsp-flutter-').path;
      final api = NativeApi.instance;
      api.start(dataDir: data);
      api.createAdapter({
        'id': 'local',
        'type': 'ollama_http',
        'name': 'Local',
        'base_url':
            'http://127.0.0.1:${env['LMBP_MOCK_MODEL_PORT'] ?? '11500'}',
        'models': ['qwen3-8b'],
        'enabled': true,
      });
      api.createAdapter({
        'id': 'off',
        'type': 'ollama_http',
        'name': 'Off',
        'base_url': 'http://127.0.0.1:1',
        'models': ['m'],
        'enabled': false,
        'skip_model_probe': true,
      });
      // Review7 #7/#8: rename + set-default built from the real list_adapters
      // view must not persist computed caps nor flip enabled/concurrency.
      Map<String, dynamic> offView() => api
          .listAdapters()
          .cast<Map<String, dynamic>>()
          .firstWhere((a) => a['id'] == 'off');
      final offBefore = offView();
      api.updateAdapter(adapterEditBody(offBefore, {'name': 'Off2'}));
      final offAfter = offView();
      expect(offAfter['name'], 'Off2');
      expect(offAfter['enabled'], false);
      expect(
        offAfter['configured_capabilities'],
        offBefore['configured_capabilities'],
      );
      expect(
        offAfter['configured_concurrency'],
        offBefore['configured_concurrency'],
      );
      expect(offAfter['capabilities'], offBefore['capabilities']);
      // raw view copy (old behaviour) is also safe at the kernel
      api.updateAdapter({...offView(), 'name': 'Off'});
      expect(
        offView()['configured_capabilities'],
        offBefore['configured_capabilities'],
      );
      expect(offView()['enabled'], false);
      // Review4 B: status stays responsive while a slow model test runs
      final slow = await HttpServer.bind('127.0.0.1', 0);
      slow.listen((r) async {
        await Future<void>.delayed(const Duration(seconds: 5));
        r.response.statusCode = 503;
        await r.response.close();
      });
      final slowTest = testAdapterAsync({
        'id': 'slowtest',
        'type': 'openai_compatible',
        'name': 'Slow',
        'base_url': 'http://127.0.0.1:${slow.port}/v1',
        'api_key': 'k',
        'models': ['m'],
      });
      await Future<void>.delayed(const Duration(milliseconds: 300));
      final sw = Stopwatch()..start();
      await statusSnapshotAsync();
      sw.stop();
      expect(
        sw.elapsedMilliseconds,
        lessThan(1500),
        reason: 'status blocked: ${sw.elapsedMilliseconds}ms',
      );
      await slowTest.catchError((_) => <String, dynamic>{});
      await slow.close(force: true);
      final ids = <String, String>{};
      for (final (tenant, gw, key) in saas) {
        final link =
            'lmbp://pair?v=2&gateway=${Uri.encodeComponent(gw)}&tenant=$tenant';
        final c = api.pairPrepare(link);
        final a = api.pairApprove(c['pair_id'] as String, [
          'local/*',
          'off/m',
        ], maxConcurrency: 2);
        await approve(gw, key, a['user_code'] as String);
        var s = api.pairStatus(c['pair_id'] as String);
        for (var i = 0; i < 100 && s['state'] == 'pending'; i++) {
          await Future<void>.delayed(const Duration(milliseconds: 200));
          s = api.pairStatus(c['pair_id'] as String);
        }
        expect(s['state'], 'paired');
        ids[tenant] = s['connection_id'] as String;
      }
      var ds = DesktopStatus.parse(
        await until(api, (s) => s['connections_online'] == 2),
      );
      expect(ds.label, '已连接 2/2');

      // permission edit as the UI does it: entries from the real adapter
      // list, untick nothing, add one model, send allow only
      final acme = ds.connections.firstWhere((c) => c.id == ids['acme']);
      final entries = policyEntries(api.listAdapters(), acme.allow);
      expect(entries.map((e) => e.id), containsAll(['local/*', 'off/m']));
      final allow = {...acme.allow, 'local/qwen3-8b'}.toList();
      api.setConnectionPolicy(acme.id, allow: allow);
      final after = (api.status()['connections'] as List)
          .cast<Map>()
          .firstWhere((c) => c['id'] == acme.id);
      expect(
        after['allow'],
        containsAll(['local/*', 'off/m', 'local/qwen3-8b']),
      );
      expect(
        after['max_concurrency'],
        2,
        reason: 'allow-only save kept the cap',
      );

      // revoke acme while its key dir cannot be deleted
      final bindings = Directory('$data/bindings');
      await Process.run('chmod', ['0500', bindings.path]);
      final r = api.connectionAction('connection_revoke', acme.id);
      await Process.run('chmod', ['0700', bindings.path]);
      final lines = revokeOutcome(r, acme.title);
      expect(lines.map((l) => l.text).first, contains('已移除连接'));
      expect(lines, hasLength(2), reason: 'cleanup failure shown separately');
      expect(DesktopStatus.parse(api.status()).cleanupWarning, isNotNull);

      // restart: retry succeeds, warning cleared, beta recovered
      api.stop();
      api.start(dataDir: data);
      final s = await until(api, (s) => s['connections_online'] == 1);
      ds = DesktopStatus.parse(s);
      expect(ds.cleanupWarning, isNull);
      expect(ds.connections.single.id, ids['beta']);
      expect(bindings.listSync().map((e) => e.path.split('/').last).toList(), [
        ids['beta'],
      ]);
      api.stop();
    },
    skip: enabled ? false : 'set LSP_NATIVE_LIB and LSP_ACCEPT_MOCK_DIR',
    timeout: const Timeout(Duration(minutes: 2)),
  );
}
