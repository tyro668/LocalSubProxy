import 'dart:convert';
import 'dart:ffi';
import 'dart:io';
import 'dart:isolate';
import 'package:ffi/ffi.dart';

typedef _NativeCall = Pointer<Utf8> Function(Pointer<Utf8>);
typedef _DartCall = Pointer<Utf8> Function(Pointer<Utf8>);
typedef _NativeFree = Void Function(Pointer<Void>);
typedef _DartFree = void Function(Pointer<Void>);

class NativeApi {
  NativeApi._();
  static final NativeApi instance = NativeApi._();

  static String libraryPath() {
    final override = Platform.environment['LSP_NATIVE_LIB'];
    if (override != null && override.isNotEmpty) return override;
    final executableDir = File(Platform.resolvedExecutable).parent.path;
    if (Platform.isWindows) return '$executableDir\\localsubproxy.dll';
    return '$executableDir/../Frameworks/liblocalsubproxy.dylib';
  }

  late final DynamicLibrary _library = DynamicLibrary.open(libraryPath());
  late final _DartCall _start = _library.lookupFunction<_NativeCall, _DartCall>(
    'LSP_Start',
  );
  late final _DartCall _call = _library.lookupFunction<_NativeCall, _DartCall>(
    'LSP_Call',
  );
  late final _DartFree _free = _library.lookupFunction<_NativeFree, _DartFree>(
    'LSP_Free',
  );

  dynamic _invoke(_DartCall fn, Map<String, dynamic> payload) {
    final input = jsonEncode(payload).toNativeUtf8();
    try {
      final output = fn(input);
      if (output == nullptr) {
        throw StateError('本地内核未返回结果');
      }
      try {
        final response =
            jsonDecode(output.toDartString()) as Map<String, dynamic>;
        if (response['ok'] != true) {
          throw StateError(response['error'] ?? '本地 API 调用失败');
        }
        return response['data'];
      } finally {
        _free(output.cast<Void>());
      }
    } finally {
      calloc.free(input);
    }
  }

  Map<String, dynamic> start({String? dataDir, String? journalKey}) {
    final input = <String, dynamic>{};
    if (dataDir != null) {
      input['data_dir'] = dataDir;
    }
    if (journalKey != null) {
      input['journal_key'] = journalKey;
    }
    return (_invoke(_start, input) as Map).cast<String, dynamic>();
  }

  Map<String, dynamic> status() =>
      (_invoke(_call, {'action': 'status'}) as Map).cast<String, dynamic>();

  // Protocol v2 (lsp2): RFC 8628 pairing. The kernel shows a user code that the
  // user confirms on the signed-in remote service page; it then polls itself.
  Map<String, dynamic> pairStart(String remoteUrl) =>
      (_invoke(_call, {'action': 'pair_start', 'remote_url': remoteUrl})
              as Map)
          .cast<String, dynamic>();

  Map<String, dynamic> pairStatus() =>
      (_invoke(_call, {'action': 'pair_status'}) as Map)
          .cast<String, dynamic>();

  void pairCancel() => _invoke(_call, {'action': 'pair_cancel'});

  void prepareConnection(String remoteUrl) =>
      _invoke(_call, {'action': 'prepare_connection', 'remote_url': remoteUrl});

  Map<String, dynamic> claim(String remoteUrl, String shortCode) =>
      (_invoke(_call, {
                'action': 'claim',
                'remote_url': remoteUrl,
                'short_code': shortCode,
              })
              as Map)
          .cast<String, dynamic>();

  void disconnect() => _invoke(_call, {'action': 'disconnect'});

  List<Map<String, dynamic>> listAdapters() {
    final data = (_invoke(_call, {'action': 'list_adapters'}) as Map)
        .cast<String, dynamic>();
    return (data['adapters'] as List)
        .map((item) => (item as Map).cast<String, dynamic>())
        .toList();
  }

  Map<String, dynamic> createAdapter(Map<String, dynamic> adapter) =>
      (_invoke(_call, {'action': 'create_adapter', 'adapter': adapter}) as Map)
          .cast<String, dynamic>();

  Map<String, dynamic> updateAdapter(Map<String, dynamic> adapter) =>
      (_invoke(_call, {'action': 'update_adapter', 'adapter': adapter}) as Map)
          .cast<String, dynamic>();

  void deleteAdapter(String id) =>
      _invoke(_call, {'action': 'delete_adapter', 'id': id});

  Map<String, dynamic> testAdapter(Map<String, dynamic> adapter) =>
      (_invoke(_call, {'action': 'test_adapter', 'adapter': adapter}) as Map)
          .cast<String, dynamic>();

  List<String> codexModels(String path) =>
      (_invoke(_call, {'action': 'codex_models', 'path': path}) as List)
          .cast<String>();

  String startCodexLogin(String path) =>
      (_invoke(_call, {'action': 'codex_login_start', 'path': path})
              as Map)['auth_url']
          as String;

  Map<String, dynamic> checkCodexLogin() =>
      (_invoke(_call, {'action': 'codex_login_check'}) as Map)
          .cast<String, dynamic>();

  void cancelCodexLogin() => _invoke(_call, {'action': 'codex_login_cancel'});

  void logoutCodex(String path) =>
      _invoke(_call, {'action': 'codex_logout', 'path': path});

  void stop() => _invoke(_call, {'action': 'stop'});
}

// These closures are created outside widget State. Isolate.run otherwise can
// retain a State (including its Timer), which cannot cross isolate boundaries.
Future<void> prepareConnectionAsync(String url) =>
    Isolate.run(() => NativeApi.instance.prepareConnection(url));

Future<Map<String, dynamic>> pairStartAsync(String url) =>
    Isolate.run(() => NativeApi.instance.pairStart(url));

Future<Map<String, dynamic>> pairStatusAsync() =>
    Isolate.run(() => NativeApi.instance.pairStatus());

Future<void> pairCancelAsync() =>
    Isolate.run(() => NativeApi.instance.pairCancel());

Future<Map<String, dynamic>> claimAsync(String url, String code) =>
    Isolate.run(() => NativeApi.instance.claim(url, code));

Future<void> disconnectAsync() =>
    Isolate.run(() => NativeApi.instance.disconnect());

Future<Map<String, dynamic>> createAdapterAsync(Map<String, dynamic> adapter) =>
    Isolate.run(() => NativeApi.instance.createAdapter(adapter));

Future<Map<String, dynamic>> updateAdapterAsync(Map<String, dynamic> adapter) =>
    Isolate.run(() => NativeApi.instance.updateAdapter(adapter));

Future<void> deleteAdapterAsync(String id) =>
    Isolate.run(() => NativeApi.instance.deleteAdapter(id));

Future<Map<String, dynamic>> testAdapterAsync(Map<String, dynamic> adapter) =>
    Isolate.run(() => NativeApi.instance.testAdapter(adapter));

Future<List<String>> codexModelsAsync(String path) =>
    Isolate.run(() => NativeApi.instance.codexModels(path));

Future<String> startCodexLoginAsync(String path) =>
    Isolate.run(() => NativeApi.instance.startCodexLogin(path));

Future<Map<String, dynamic>> checkCodexLoginAsync() =>
    Isolate.run(() => NativeApi.instance.checkCodexLogin());

Future<void> cancelCodexLoginAsync() =>
    Isolate.run(() => NativeApi.instance.cancelCodexLogin());

Future<void> logoutCodexAsync(String path) =>
    Isolate.run(() => NativeApi.instance.logoutCodex(path));
