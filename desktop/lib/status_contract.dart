import 'dart:convert';
// Desktop status contract (docs/desktop-status-contract.md): the kernel's
// `status` JSON lists every paired SaaS in connections[]; the flat fields
// mirror the primary (first) connection for older UI code.

class ConnectionInfo {
  const ConnectionInfo({
    required this.id,
    required this.saasName,
    required this.issuer,
    required this.tenant,
    required this.state,
    required this.connected,
    required this.runningTasks,
    required this.maxConcurrency,
    required this.allow,
    required this.paused,
    required this.lastError,
    required this.deviceId,
    required this.displayUrl,
    required this.transport,
    required this.e2ee,
  });

  final String id;
  final String saasName;
  final String issuer;
  final String tenant;

  /// online | connecting | disconnected | revoked | error
  final String state;
  final bool connected;
  final int runningTasks;
  final int maxConcurrency;

  /// Models this SaaS may use ("adapter/model", "adapter/*" or "*").
  final List<String> allow;
  final bool paused;
  final String lastError;
  final String deviceId;
  final String displayUrl;
  final String transport;
  final bool e2ee;

  String get title =>
      saasName.isNotEmpty ? saasName : (tenant.isNotEmpty ? tenant : id);

  String get stateLabel => switch (state) {
    'online' => '在线',
    'connecting' => '连接中',
    'disconnected' => '已断开',
    'revoked' => '已被吊销',
    'error' => '异常',
    'unknown' => '状态未知',
    _ => state,
  };

  bool allows(String model) {
    final adapter = model.split('/').first;
    return allow.any((e) => e == '*' || e == model || e == '$adapter/*');
  }

  static String _s(Object? v) => v == null ? '' : '$v';
  static int _i(Object? v) => v is num ? v.toInt() : 0;

  factory ConnectionInfo.fromJson(Map<String, dynamic> m) => ConnectionInfo(
    id: _s(m['id']),
    saasName: _s(m['saas_name']),
    issuer: _s(m['issuer']),
    tenant: _s(m['tenant']),
    state: _s(m['state']).isEmpty
        ? (m['connected'] == true ? 'online' : 'connecting')
        : _s(m['state']),
    connected: m['connected'] == true,
    runningTasks: _i(m['running_tasks']),
    maxConcurrency: _i(m['max_concurrency']),
    allow: ((m['allow'] as List?) ?? const []).map((e) => '$e').toList(),
    paused: m['paused'] == true,
    lastError: _s(m['last_error']),
    deviceId: _s(m['device_id']),
    displayUrl: _s(m['display_url']).isNotEmpty
        ? _s(m['display_url'])
        : _s(m['remote_url']),
    transport: _s(m['transport']),
    e2ee: m['e2ee'] == true,
  );
}

class DesktopStatus {
  const DesktopStatus({
    required this.connections,
    required this.multi,
    required this.runningTasks,
    this.stale = false,
    this.cleanupWarning,
  });

  /// Kernel `warning` while revoked connections still have undeleted key
  /// files (`cleanup_pending`); kept until a retry at startup succeeds.
  final String? cleanupWarning;

  final List<ConnectionInfo> connections;

  /// True when the kernel reported connections[] (multi-SaaS contract).
  final bool multi;
  final int runningTasks;

  int get total => connections.length;
  int get online => connections.where((c) => c.connected).length;
  bool get paired => connections.isNotEmpty;
  bool get anyConnected => online > 0;

  /// Status bar label: 已连接 n/m (multi) or 已连接 / 未连接 (legacy kernel).
  String get label {
    if (stale) return '状态未知';
    if (!multi) return anyConnected ? '已连接' : '未连接';
    if (total == 0) return '未连接';
    return '${anyConnected ? '已连接' : '未连接'} $online/$total';
  }

  /// True when the last status refresh failed: the connections below are
  /// the last known ones and their live state is unknown.
  final bool stale;

  factory DesktopStatus.parse(Map<String, dynamic>? s) {
    final running = s?['running_tasks'] is num
        ? (s!['running_tasks'] as num).toInt()
        : 0;
    final stale = s?['stale'] == true;
    final warning = s?['warning'] is String ? s!['warning'] as String : null;
    final raw = s?['connections'];
    if (raw is List) {
      return DesktopStatus(
        multi: true,
        stale: stale,
        cleanupWarning: warning,
        runningTasks: running,
        connections: raw.whereType<Map>().map((e) {
          final m = e.cast<String, dynamic>();
          return ConnectionInfo.fromJson(
            stale ? {...m, 'state': 'unknown', 'connected': false} : m,
          );
        }).toList(),
      );
    }
    // Legacy kernel: a single connection described by the flat fields.
    if (s == null || s['paired'] != true) {
      return DesktopStatus(
        connections: const [],
        multi: false,
        runningTasks: running,
        stale: stale,
      );
    }
    return DesktopStatus(
      multi: false,
      stale: stale,
      runningTasks: running,
      connections: [
        ConnectionInfo.fromJson({
          ...s,
          'id': s['connection_id'] ?? 'default',
          'allow': const ['*'],
        }),
      ],
    );
  }
}

/// One row of the per-connection model permission editor.
class PolicyEntry {
  const PolicyEntry(this.id, this.note, {this.label = '', this.detail = ''});

  /// Display only (never stored): the scheme name, e.g. "gpt-6.1-sol_1".
  final String label;

  /// Display only: secondary text, e.g. the upstream model "gpt-6.1-sol".
  final String detail;

  /// "adapter/model", "adapter/*" or "*" — exactly what is stored in allow.
  final String id;

  /// Why the row is special (disabled adapter, wildcard, unknown model).
  final String note;

  bool get isWildcard => id == '*' || id.endsWith('/*');
}

/// Rows for editing allow: every adapter model (enabled or disabled), plus
/// every allow entry that is not a concrete current model (wildcards and
/// models that no longer exist), so that saving never drops an
/// authorization the user did not touch.
List<PolicyEntry> policyEntries(
  List<Map<String, dynamic>> adapters,
  List<String> allow,
) {
  final out = <PolicyEntry>[];
  final seen = <String>{};
  final labels = schemeLabels(adapters);
  final names = schemeNames(adapters);
  for (final e in allow) {
    if ((e == '*' || e.endsWith('/*')) && seen.add(e)) {
      final scheme = e == '*' ? '' : names[e.substring(0, e.length - 2)] ?? '';
      out.add(
        PolicyEntry(
          e,
          e == '*' ? '通配：全部本机模型，含以后新增和停用的' : '通配：该方案全部模型，含以后新增的',
          label: e == '*' ? '全部本机模型' : (scheme.isEmpty ? e : '$scheme · 全部模型'),
        ),
      );
    }
  }
  for (final a in adapters) {
    for (final m
        in (a['models'] as List?)?.cast<String>() ?? const <String>[]) {
      final id = '${a['id']}/$m';
      if (seen.add(id)) {
        out.add(
          PolicyEntry(
            id,
            a['enabled'] == true ? '' : '方案已停用（授权保留）',
            label: labels[id] ?? id,
            detail: m,
          ),
        );
      }
    }
  }
  for (final e in allow) {
    if (seen.add(e)) out.add(PolicyEntry(e, '当前不存在的模型（授权保留）'));
  }
  return out;
}

/// Whether model id is already granted by a wildcard in selected.
bool coveredByWildcard(String id, Set<String> selected) {
  if (id == '*' || id.endsWith('/*')) return false;
  final adapter = id.split('/').first;
  return selected.contains('*') || selected.contains('$adapter/*');
}

/// Result lines of connection_revoke: the removal and, separately, a failed
/// key cleanup (the kernel sets `warning` but the connection is gone).
List<({String text, bool error})> revokeOutcome(
  Map<String, dynamic> r,
  String title,
) {
  final out = <({String text, bool error})>[
    (text: '已移除连接「$title」', error: false),
  ];
  final w = r['warning'];
  if (w is String && w.isNotEmpty) {
    final errs = (r['cleanup_errors'] as List?)?.join('；') ?? w;
    out.add((text: '本机私钥清理失败（将在每次启动时重试）：$errs', error: true));
  }
  return out;
}

/// After a failed pair_approve: true when the kernel still holds the
/// consent request (pair_status state "consent"), so the same consent page
/// can simply retry; otherwise the UI must restart pairing.
bool approveRetryable(Map<String, dynamic>? pairStatus) =>
    pairStatus?['state'] == 'consent';

/// Scheme (adapter) id -> trimmed display name ("" when unnamed).
Map<String, String> schemeNames(List<Map<String, dynamic>> adapters) => {
  for (final a in adapters) '${a['id']}': '${a['name'] ?? ''}'.trim(),
};

/// Display label per stored model id ("adapter/model"): the scheme name,
/// plus the model when a scheme serves several, and the adapter id when two
/// schemes still share a name (older configs). Falls back to the id.
/// Display only: allow lists and requests keep the ids.
Map<String, String> schemeLabels(List<Map<String, dynamic>> adapters) {
  final raw = <String, String>{};
  for (final a in adapters) {
    final name = '${a['name'] ?? ''}'.trim();
    final models = (a['models'] as List?)?.cast<String>() ?? const <String>[];
    for (final m in models) {
      final id = '${a['id']}/$m';
      raw[id] = name.isEmpty ? id : (models.length > 1 ? '$name · $m' : name);
    }
  }
  final count = <String, int>{};
  for (final v in raw.values) {
    count[v.toLowerCase()] = (count[v.toLowerCase()] ?? 0) + 1;
  }
  return {
    for (final e in raw.entries)
      e.key: (count[e.value.toLowerCase()] ?? 0) > 1 && e.value != e.key
          ? '${e.value} (${e.key.split('/').first})'
          : e.value,
  };
}

/// The allow list as readable text for the connection tile.
String allowSummary(List<String> allow, List<Map<String, dynamic>> adapters) {
  if (allow.isEmpty) return '未授权任何模型';
  if (allow.contains('*')) return '全部本机模型';
  final byId = {for (final e in policyEntries(adapters, allow)) e.id: e.label};
  return allow.map((e) => (byId[e] ?? '').isEmpty ? e : byId[e]!).join(', ');
}

/// Default name for a new scheme: the model itself when free, else `<model>_<n>` with the smallest n >= 2
/// not used by another scheme (trimmed, case-insensitive), so a new scheme
/// usually needs no renaming. Mirrors connector.DefaultSchemeName.
String defaultSchemeName(String model, Iterable<String> taken) {
  var m = model.trim();
  if (m.isEmpty) m = 'scheme';
  final used = {for (final t in taken) t.trim().toLowerCase()};
  final first = _withSuffix(m, '');
  if (!used.contains(first.toLowerCase())) return first;
  for (var n = 2; ; n++) {
    final c = _withSuffix(m, '_$n');
    if (!used.contains(c.toLowerCase())) return c;
  }
}

/// Scheme name limit in UTF-8 bytes (connector.MaxSchemeNameBytes).
const maxSchemeNameBytes = 100;

/// Cuts s to at most n UTF-8 bytes at a character boundary.
String clipSchemeNameBytes(String s, int n) {
  var used = 0;
  final out = StringBuffer();
  for (final r in s.runes) {
    final b = utf8.encode(String.fromCharCode(r)).length;
    if (used + b > n) break;
    used += b;
    out.writeCharCode(r);
  }
  return out.toString().trim();
}

String _withSuffix(String base, String suffix) =>
    clipSchemeNameBytes(base, maxSchemeNameBytes - utf8.encode(suffix).length) + suffix;

/// Inline validation for a scheme name; null when acceptable. exceptId is
/// the scheme being edited (its own name never conflicts).
String? schemeNameError(
  String name,
  List<Map<String, dynamic>> adapters, {
  String? exceptId,
}) {
  final k = name.trim().toLowerCase();
  if (k.isEmpty) return null; // the kernel fills a default
  if (utf8.encode(name.trim()).length > maxSchemeNameBytes) {
    return '名称过长：最多 $maxSchemeNameBytes 字节（约 33 个汉字）';
  }
  for (final a in adapters) {
    if (exceptId != null && '${a['id']}' == exceptId) continue;
    if ('${a['name'] ?? ''}'.trim().toLowerCase() == k) {
      return '名称「${name.trim()}」已被其他方案使用';
    }
  }
  return null;
}

/// Overall state of the paired connections, for the connection dialog
/// header. Derived only from per-connection `state` so it never contradicts
/// the status bar (which uses the same `connected` flags).
enum ConnSummaryKind {
  online,
  partial,
  connecting,
  disconnected,
  revoked,
  error,
  unknown,
  none,
}

class ConnSummary {
  const ConnSummary(this.kind, this.title);
  final ConnSummaryKind kind;
  final String title;
}

ConnSummary connectionSummary(DesktopStatus ds) {
  final total = ds.total;
  if (total == 0) return const ConnSummary(ConnSummaryKind.none, '未连接');
  if (ds.stale) return const ConnSummary(ConnSummaryKind.unknown, '状态未知');
  final online = ds.online;
  String agg(String s) => total > 1 ? '$s（$online/$total 在线）' : s;
  if (online == total) return ConnSummary(ConnSummaryKind.online, agg('已连接'));
  if (online > 0) return ConnSummary(ConnSummaryKind.partial, agg('部分在线'));
  final states = ds.connections.map((c) => c.state).toSet();
  if (states.contains('connecting')) {
    return ConnSummary(ConnSummaryKind.connecting, agg('连接中'));
  }
  if (states.contains('error')) {
    return ConnSummary(ConnSummaryKind.error, agg('连接异常'));
  }
  if (states.length == 1 && states.single == 'revoked') {
    return ConnSummary(ConnSummaryKind.revoked, agg('已被吊销'));
  }
  if (states.length == 1 && states.single == 'disconnected') {
    return ConnSummary(ConnSummaryKind.disconnected, agg('已断开'));
  }
  if (states.contains('unknown')) {
    return ConnSummary(ConnSummaryKind.unknown, agg('状态未知'));
  }
  return ConnSummary(ConnSummaryKind.disconnected, agg('未连接'));
}
