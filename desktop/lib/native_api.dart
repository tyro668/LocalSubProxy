import 'dart:convert';
import 'dart:ffi';
import 'dart:io';
import 'dart:isolate';
import 'package:ffi/ffi.dart';
import 'package:flutter/foundation.dart' show visibleForTesting;

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
  _DartCall _startFn() => _start;
  late final _DartCall _call = _library.lookupFunction<_NativeCall, _DartCall>(
    'LSP_Call',
  );
  late final _DartFree _free = _library.lookupFunction<_NativeFree, _DartFree>(
    'LSP_Free',
  );

  /// Test seam: when set, every kernel call goes here instead of the native
  /// library (payload as sent to LSP_Call; LSP_Start gets `'action': 'start'`),
  /// and the *Async helpers run on the calling isolate. Never set in the app.
  @visibleForTesting
  static dynamic Function(Map<String, dynamic> payload)? debugTransport;

  /// Test seam: awaited before each *Async call when [debugTransport] is set,
  /// so a test can hold a call "in flight".
  @visibleForTesting
  static Future<void> Function()? debugGate;

  dynamic _invoke(_DartCall Function() fnOf, Map<String, dynamic> payload) {
    final t = debugTransport;
    if (t != null) {
      return t(fnOf == _startFn ? {'action': 'start', ...payload} : payload);
    }
    final fn = fnOf();
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

  Map<String, dynamic> start({String? dataDir}) {
    final input = <String, dynamic>{};
    if (dataDir != null) {
      input['data_dir'] = dataDir;
    }
    return (_invoke(_startFn, input) as Map).cast<String, dynamic>();
  }

  Map<String, dynamic> status() =>
      (_invoke(() => _call, {'action': 'status'}) as Map)
          .cast<String, dynamic>();

  // lmbp pairing, step 1: resolve the SaaS (https origin or lmbp://pair
  // link) and verify the gateway's signed metadata. Returns
  // {state: consent, consent: {saas: {...}, gateway, tenant, ...}}.
  Map<String, dynamic> pairPrepare(String urlOrLink) =>
      (_invoke(() => _call, {
                'action': 'pair_prepare',
                if (urlOrLink.startsWith('lmbp://'))
                  'link': urlOrLink
                else
                  'remote_url': urlOrLink,
              })
              as Map)
          .cast<String, dynamic>();

  // Step 2a: the user approved the consent screen and chose the models this
  // SaaS may use (allow; empty denies all). Returns the RFC 8628 user code
  // that the user confirms on the signed-in SaaS page.
  Map<String, dynamic> pairApprove(
    String? pairId,
    List<String> allow, {
    int maxConcurrency = 0,
  }) =>
      (_invoke(() => _call, {
                'action': 'pair_approve',
                'pair_id': ?pairId,
                'allow': allow,
                if (maxConcurrency > 0) 'max_concurrency': maxConcurrency,
              })
              as Map)
          .cast<String, dynamic>();

  // Step 2b: the user declined (CONSENT_DENIED); nothing is sent.
  void pairDeny(String? pairId) =>
      _invoke(() => _call, {'action': 'pair_deny', 'pair_id': ?pairId});

  // Per-connection management (connection_id = status connections[].id).
  Map<String, dynamic> connectionAction(String action, String id) =>
      (_invoke(() => _call, {'action': action, 'connection_id': id}) as Map)
          .cast<String, dynamic>();

  /// Partial update: only the given fields change (the kernel keeps the
  /// rest, e.g. max_concurrency when only allow is saved).
  Map<String, dynamic> setConnectionPolicy(
    String id, {
    List<String>? allow,
    int? maxConcurrency,
  }) =>
      (_invoke(
                () => _call,
                policyRequest(id, allow: allow, maxConcurrency: maxConcurrency),
              )
              as Map)
          .cast<String, dynamic>();

  /// The connection_policy_set request (fields left null are not sent).
  static Map<String, dynamic> policyRequest(
    String id, {
    List<String>? allow,
    int? maxConcurrency,
  }) => {
    'action': 'connection_policy_set',
    'connection_id': id,
    'policy': {'allow': ?allow, 'max_concurrency': ?maxConcurrency},
  };

  // Local audit log and token usage (no prompt content is stored).
  List<Map<String, dynamic>> usageStats(List<String> groupBy, int days) {
    final since = DateTime.now()
        .toUtc()
        .subtract(Duration(days: days))
        .toIso8601String();
    final r =
        _invoke(() => _call, {
              'action': 'usage_stats',
              'group_by': groupBy,
              'filter': {'since': since},
              'local_time': true,
            })
            as Map;
    return ((r['buckets'] as List?) ?? [])
        .map((e) => (e as Map).cast<String, dynamic>())
        .toList();
  }

  List<Map<String, dynamic>> auditList(int limit) {
    final r =
        _invoke(() => _call, {
              'action': 'audit_list',
              'filter': {'limit': limit},
            })
            as Map;
    return ((r['records'] as List?) ?? [])
        .map((e) => (e as Map).cast<String, dynamic>())
        .toList();
  }

  Map<String, dynamic> pairStatus(String? pairId) =>
      (_invoke(() => _call, {'action': 'pair_status', 'pair_id': ?pairId})
              as Map)
          .cast<String, dynamic>();

  void pairCancel(String? pairId) =>
      _invoke(() => _call, {'action': 'pair_cancel', 'pair_id': ?pairId});

  List<Map<String, dynamic>> listAdapters() {
    final data = (_invoke(() => _call, {'action': 'list_adapters'}) as Map)
        .cast<String, dynamic>();
    return (data['adapters'] as List)
        .map((item) => (item as Map).cast<String, dynamic>())
        .toList();
  }

  Map<String, dynamic> createAdapter(Map<String, dynamic> adapter) =>
      (_invoke(() => _call, {'action': 'create_adapter', 'adapter': adapter})
              as Map)
          .cast<String, dynamic>();

  Map<String, dynamic> updateAdapter(Map<String, dynamic> adapter) =>
      (_invoke(() => _call, {'action': 'update_adapter', 'adapter': adapter})
              as Map)
          .cast<String, dynamic>();

  /// Dismisses the "adapter configuration could not be decrypted" notice
  /// for good (persisted by the core, so it is not shown again).
  void ackAdapterRecovery() =>
      _invoke(() => _call, {'action': 'adapter_recovery_ack'});
  void deleteAdapter(String id) =>
      _invoke(() => _call, {'action': 'delete_adapter', 'id': id});

  Map<String, dynamic> testAdapter(Map<String, dynamic> adapter) =>
      (_invoke(() => _call, {'action': 'test_adapter', 'adapter': adapter})
              as Map)
          .cast<String, dynamic>();

  /// Models the provider of a draft profile offers (GET /models); nothing is
  /// saved. A blank api_key reuses the saved key for the same endpoint.
  List<String> providerModels(Map<String, dynamic> adapter) =>
      ((_invoke(() => _call, {'action': 'provider_models', 'adapter': adapter})
                  as List?) ??
              const [])
          .cast<String>();

  List<String> codexModels(String path) =>
      (_invoke(() => _call, {'action': 'codex_models', 'path': path}) as List)
          .cast<String>();

  String startCodexLogin(String path) =>
      (_invoke(() => _call, {'action': 'codex_login_start', 'path': path})
              as Map)['auth_url']
          as String;

  Map<String, dynamic> checkCodexLogin() =>
      (_invoke(() => _call, {'action': 'codex_login_check'}) as Map)
          .cast<String, dynamic>();

  void cancelCodexLogin() =>
      _invoke(() => _call, {'action': 'codex_login_cancel'});

  void logoutCodex(String path) =>
      _invoke(() => _call, {'action': 'codex_logout', 'path': path});

  void stop() => _invoke(() => _call, {'action': 'stop'});
}

// These closures are created outside widget State. Isolate.run otherwise can
// retain a State (including its Timer), which cannot cross isolate boundaries.
Future<Map<String, dynamic>> pairPrepareAsync(String urlOrLink) =>
    _offThread(() => NativeApi.instance.pairPrepare(urlOrLink));

Future<Map<String, dynamic>> pairApproveAsync(
  String? pairId,
  List<String> allow,
) => _offThread(() => NativeApi.instance.pairApprove(pairId, allow));

Future<void> pairDenyAsync(String? pairId) =>
    _offThread(() => NativeApi.instance.pairDeny(pairId));

/// action: connection_disconnect | connection_connect | connection_revoke
Future<Map<String, dynamic>> connectionActionAsync(String action, String id) =>
    _offThread(() => NativeApi.instance.connectionAction(action, id));

Future<Map<String, dynamic>> setConnectionPolicyAsync(
  String id,
  List<String> allow,
) => _offThread(() => NativeApi.instance.setConnectionPolicy(id, allow: allow));

Future<List<Map<String, dynamic>>> usageStatsAsync(
  List<String> groupBy,
  int days,
) => _offThread(() => NativeApi.instance.usageStats(groupBy, days));

Future<List<Map<String, dynamic>>> auditListAsync(int limit) =>
    _offThread(() => NativeApi.instance.auditList(limit));

Future<Map<String, dynamic>> pairStatusAsync(String? pairId) =>
    _offThread(() => NativeApi.instance.pairStatus(pairId));

/// Status + adapter list for the 2s poll, read off the UI thread.
Future<(Map<String, dynamic>, List<Map<String, dynamic>>)>
statusSnapshotAsync() => _offThread(
  () => (NativeApi.instance.status(), NativeApi.instance.listAdapters()),
);

Future<void> pairCancelAsync(String? pairId) =>
    _offThread(() => NativeApi.instance.pairCancel(pairId));

Future<Map<String, dynamic>> createAdapterAsync(Map<String, dynamic> adapter) =>
    _offThread(() => NativeApi.instance.createAdapter(adapter));

Future<Map<String, dynamic>> updateAdapterAsync(Map<String, dynamic> adapter) =>
    _offThread(() => NativeApi.instance.updateAdapter(adapter));

Future<void> ackAdapterRecoveryAsync() =>
    _offThread(() => NativeApi.instance.ackAdapterRecovery());
Future<void> deleteAdapterAsync(String id) =>
    _offThread(() => NativeApi.instance.deleteAdapter(id));

Future<Map<String, dynamic>> testAdapterAsync(Map<String, dynamic> adapter) =>
    _offThread(() => NativeApi.instance.testAdapter(adapter));

Future<List<String>> providerModelsAsync(Map<String, dynamic> adapter) =>
    _offThread(() => NativeApi.instance.providerModels(adapter));

Future<List<String>> codexModelsAsync(String path) =>
    _offThread(() => NativeApi.instance.codexModels(path));

Future<String> startCodexLoginAsync(String path) =>
    _offThread(() => NativeApi.instance.startCodexLogin(path));

Future<Map<String, dynamic>> checkCodexLoginAsync() =>
    _offThread(() => NativeApi.instance.checkCodexLogin());

Future<void> cancelCodexLoginAsync() =>
    _offThread(() => NativeApi.instance.cancelCodexLogin());

Future<void> logoutCodexAsync(String path) =>
    _offThread(() => NativeApi.instance.logoutCodex(path));

/// Native calls run in a separate isolate so they never block the UI thread;
/// with the test transport they run here (statics are per isolate).
Future<T> _offThread<T>(T Function() f) async {
  if (NativeApi.debugTransport == null) return Isolate.run(f);
  final gate = NativeApi.debugGate;
  if (gate != null) await gate();
  return f();
}
