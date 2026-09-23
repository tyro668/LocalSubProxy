import 'dart:convert';
import 'dart:async';
import 'dart:io';
import 'package:localsubproxy_ui/native_api.dart';

class _PageWithTimer {
  final timer = Timer.periodic(const Duration(seconds: 60), (_) {});

  Future<void> prepare(String url) => prepareConnectionAsync(url);
  Future<Map<String, dynamic>> claim(String url, String code) =>
      claimAsync(url, code);

  void dispose() => timer.cancel();
}

Future<void> main() async {
  final dir = await Directory.systemTemp.createTemp('localsubproxy-pairing-');
  final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
  final origin = 'http://127.0.0.1:${server.port}';
  final api = NativeApi.instance;
  final page = _PageWithTimer();
  server.listen((request) async {
    request.response.headers.contentType = ContentType.json;
    if (request.uri.path == '/api/llm/v1/local/pairing-info') {
      request.response.write(
        jsonEncode({'device_id': 'dev_test', 'pairing_enabled': true}),
      );
    } else if (request.uri.path == '/api/llm/v1/local/claim') {
      final body = jsonDecode(await utf8.decoder.bind(request).join()) as Map;
      if (body['short_code'] != '12345678') {
        request.response.statusCode = 409;
      } else {
        request.response.write(
          jsonEncode({
            'device_id': 'dev_test',
            'device_token': 'device-token-0123456789-0123456789-abc',
            'gateway_url': origin,
          }),
        );
      }
    } else if (request.uri.path == '/v1/connector/heartbeat') {
      await request.drain();
      request.response.write('{}');
    } else {
      request.response.statusCode = 404;
      request.response.write('{}');
    }
    await request.response.close();
  });
  try {
    api.start(dataDir: dir.path);
    api.createAdapter({
      'id': 'offline-model',
      'type': 'ollama_http',
      'name': 'Offline Model',
      'base_url': 'http://127.0.0.1:11434',
      'models': ['demo'],
      'enabled': true,
    });
    await page.prepare(origin);
    await page.claim(origin, '12345678');
    final paired = api.status();
    String? computerName;
    if (Platform.isMacOS) {
      final result = await Process.run('/usr/sbin/scutil', [
        '--get',
        'ComputerName',
      ]);
      if (result.exitCode == 0) computerName = '${result.stdout}'.trim();
    } else if (Platform.isWindows) {
      computerName = Platform.environment['COMPUTERNAME'];
    }
    if (paired['paired'] != true ||
        paired['remote_url'] != origin ||
        (paired['device_name'] as String?)?.isNotEmpty != true ||
        (computerName?.isNotEmpty == true &&
            paired['device_name'] != computerName) ||
        paired['transport'] != 'http' ||
        api.listAdapters().single['id'] != 'offline-model') {
      throw StateError('pairing status or offline models were not preserved');
    }
    api.stop();
    final identityFile = File('${dir.path}/identity.json');
    final legacyIdentity =
        (jsonDecode(await identityFile.readAsString()) as Map)
            .cast<String, dynamic>();
    legacyIdentity['saas_url'] = legacyIdentity.remove('remote_url');
    await identityFile.writeAsString(jsonEncode(legacyIdentity));
    final restarted = api.start(dataDir: dir.path);
    final upgradedIdentity =
        (jsonDecode(await identityFile.readAsString()) as Map)
            .cast<String, dynamic>();
    if (restarted['paired'] != true ||
        restarted['device_id'] != 'dev_test' ||
        restarted['remote_url'] != origin ||
        upgradedIdentity['remote_url'] != origin ||
        upgradedIdentity.containsKey('saas_url')) {
      throw StateError('saved connection was not restored');
    }
    await disconnectAsync();
    if (api.status()['paired'] != false ||
        api.listAdapters().single['id'] != 'offline-model') {
      throw StateError('disconnect changed offline model configuration');
    }
    stdout.writeln('Native pairing smoke test passed.');
  } finally {
    page.dispose();
    try {
      api.stop();
    } catch (_) {}
    await server.close(force: true);
    await dir.delete(recursive: true);
  }
}
