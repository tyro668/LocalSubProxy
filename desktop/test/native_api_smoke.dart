import 'dart:io';
import 'dart:isolate';
import 'package:localsubproxy_ui/native_api.dart';

Future<void> main() async {
  final dir = await Directory.systemTemp.createTemp('localsubproxy-native-');
  final api = NativeApi.instance;
  try {
    final started = api.start(dataDir: dir.path);
    if (started['paired'] != false || started['transport'] != 'offline') {
      throw StateError('offline start failed');
    }
    final workerStatus = await Isolate.run(() => NativeApi.instance.status());
    if (workerStatus['paired'] != false) {
      throw StateError('cross-isolate API failed');
    }
    final adapter = <String, dynamic>{
      'id': 'local',
      'type': 'ollama_http',
      'name': 'Local',
      'base_url': 'http://127.0.0.1:11434',
      'models': ['demo'],
      'enabled': true,
    };
    api.createAdapter(adapter);
    if (api.listAdapters().single['id'] != 'local') {
      throw StateError('create failed');
    }
    adapter['name'] = 'Renamed';
    api.updateAdapter(adapter);
    if (api.listAdapters().single['name'] != 'Renamed') {
      throw StateError('update failed');
    }
    api.stop();
    final identityFile = File('${dir.path}/identity.json');
    final restarted = api.start(dataDir: dir.path);
    if (restarted['paired'] != false ||
        api.listAdapters().single['name'] != 'Renamed') {
      throw StateError('restart failed');
    }
    api.deleteAdapter('local');
    if (api.listAdapters().isNotEmpty) throw StateError('delete failed');
    if (!File('${dir.path}/adapters.enc').existsSync()) {
      throw StateError('encrypted configuration missing');
    }
    if (!identityFile.existsSync()) {
      throw StateError('persistent local identity missing');
    }
    stdout.writeln('Native API smoke test passed.');
  } finally {
    try {
      api.stop();
    } catch (_) {}
    await dir.delete(recursive: true);
  }
}
