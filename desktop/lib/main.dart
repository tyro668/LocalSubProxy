import 'dart:async';

import 'package:flutter/services.dart';
import 'package:shadcn_flutter/shadcn_flutter.dart';
import 'package:window_manager/window_manager.dart';

import 'native_api.dart';
import 'status_contract.dart';
import 'profile_workspace.dart';
import 'tray.dart';

const _brand = Color(0xFF4D25D8);
const _brandForeground = Color(0xFFFFFFFF);
const _brandDark = Color(0xFFAB94FF);
const _brandDarkForeground = Color(0xFF0C1224);

String _transportLabel(Object? transport) => switch (transport) {
  'wss' => 'WSS（lmbp）',
  'ws' => 'WS（lmbp，仅本机联调）',
  _ => '未连接',
};

ThemeData _lightTheme() => ThemeData(
  colorScheme: ColorSchemes.lightZinc.copyWith(
    primary: () => _brand,
    primaryForeground: () => _brandForeground,
    ring: () => _brand,
  ),
  radius: 0.6,
);

ThemeData _darkTheme() => ThemeData.dark(
  colorScheme: ColorSchemes.darkZinc.copyWith(
    primary: () => _brandDark,
    primaryForeground: () => _brandDarkForeground,
    ring: () => _brandDark,
  ),
  radius: 0.6,
);

void main() async {
  WidgetsFlutterBinding.ensureInitialized();
  await windowManager.ensureInitialized();
  runApp(const ProxyApp());
}

class ProxyApp extends StatelessWidget {
  const ProxyApp({super.key});

  @override
  Widget build(BuildContext context) => ShadcnApp(
    title: 'LocalSubProxy',
    debugShowCheckedModeBanner: false,
    theme: _lightTheme(),
    darkTheme: _darkTheme(),
    themeMode: ThemeMode.light,
    home: const HomePage(),
  );
}

class HomePage extends StatefulWidget {
  const HomePage({super.key});

  @override
  State<HomePage> createState() => _HomePageState();
}

class _HomePageState extends State<HomePage> {
  final remoteUrl = TextEditingController();
  Timer? timer;
  bool kernelReady = false;
  bool recoveryDismissed = false;
  String? kernelError;
  Map<String, dynamic>? status;
  List<Map<String, dynamic>> adapters = [];
  bool refreshing = false;
  bool busy = false;
  bool awaitingCode = false;
  String? pendingRemoteURL;
  // Pairing (RFC 8628 style): code shown on this device, confirmed on the
  // remote service page.
  String? pairUserCode;
  String? pairLinkUrl;
  // Verified SaaS identity awaiting the user's explicit approve/deny.
  Map<String, dynamic>? consent;
  // Pairing session id (pair_id) and the models chosen on the consent page
  // (default deny: nothing is checked until the user picks).
  String? pairId;
  List<String> consentModels = [];
  final Set<String> consentAllow = {};
  // Showing the "add connection" form while other SaaS are already paired.
  bool adding = false;
  // Local token usage (last 7 days, by SaaS and model).
  List<Map<String, dynamic>> usage = [];
  final Set<VoidCallback> _overlayRelays = {};

  NativeApi get api => NativeApi.instance;

  @override
  void setState(VoidCallback fn) {
    super.setState(fn);
    for (final relay in List<VoidCallback>.of(_overlayRelays)) {
      relay();
    }
  }

  @override
  void initState() {
    super.initState();
    // 首帧渲染后再创建托盘项；应用启动早期创建 NSStatusItem 会导致其无法正确寄宿到菜单栏
    WidgetsBinding.instance.addPostFrameCallback(
      (_) => AppTray.instance.init(),
    );
    boot();
    timer = Timer.periodic(const Duration(seconds: 2), (_) {
      refresh();
      pollPairing();
    });
  }

  @override
  void dispose() {
    timer?.cancel();
    remoteUrl.dispose();
    super.dispose();
  }

  void show(String message, {bool error = false}) {
    if (!mounted) return;
    showToast(
      context: context,
      location: ToastLocation.bottomRight,
      showDuration: const Duration(seconds: 4),
      builder: (context, overlay) {
        final theme = Theme.of(context);
        return SurfaceCard(
          padding: const EdgeInsets.symmetric(horizontal: 14, vertical: 12),
          child: Row(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Icon(
                error ? LucideIcons.triangleAlert : LucideIcons.circleCheck,
                size: 16,
                color: error
                    ? theme.colorScheme.destructive
                    : theme.colorScheme.chart2,
              ),
              const Gap(10),
              Expanded(child: Text(message).small()),
            ],
          ),
        );
      },
    );
  }

  Future<bool> confirm({
    required String title,
    required String message,
    required String action,
    bool destructive = false,
  }) async {
    final yes = await showOverlay<bool>(
      context,
      DialogConfiguration<bool>(
        builder: (ctx) => AlertDialog(
          title: Text(title),
          content: Text(message),
          actions: [
            OutlineButton(
              onPressed: () => Navigator.of(ctx).pop(false),
              child: const Text('取消'),
            ),
            destructive
                ? DestructiveButton(
                    onPressed: () => Navigator.of(ctx).pop(true),
                    child: Text(action),
                  )
                : PrimaryButton(
                    onPressed: () => Navigator.of(ctx).pop(true),
                    child: Text(action),
                  ),
          ],
        ),
      ),
    ).future;
    return yes == true;
  }

  Future<void> boot() async {
    try {
      api.start();
      if (mounted) {
        setState(() {
          kernelReady = true;
          kernelError = null;
        });
      }
    } catch (e) {
      if (mounted) {
        setState(() {
          kernelReady = false;
          kernelError = e.toString();
        });
      }
      return;
    }
    await refresh();
  }

  Future<void> refresh({bool force = false}) async {
    if (!kernelReady || refreshing || (busy && !force)) return;
    refreshing = true;
    try {
      // In an isolate: a native call must never block the UI thread.
      final (s, a) = await statusSnapshotAsync();
      if (mounted) {
        setState(() {
          status = s;
          adapters = a;
        });
      }
    } catch (_) {
      if (mounted) {
        // Keep the last known list but mark it stale: no connection is
        // shown as online while the kernel cannot be reached.
        setState(
          () => status = {...?status, 'connected': false, 'stale': true},
        );
      }
    } finally {
      refreshing = false;
    }
  }

  Future<void> prepareConnection() async {
    final url = remoteUrl.text.trim();
    if (url.isEmpty) {
      show('请输入远端服务地址。', error: true);
      return;
    }
    setState(() => busy = true);
    try {
      final p = await pairPrepareAsync(url);
      if (mounted) {
        setState(() {
          pendingRemoteURL = url;
          pairId = p['pair_id'] as String?;
          consent = (p['consent'] as Map?)?.cast<String, dynamic>();
          consentModels = ((p['available_models'] as List?) ?? const [])
              .cast<String>();
          consentAllow.clear();
        });
      }
    } catch (e) {
      if (mounted) show('连接远端服务失败：$e', error: true);
    } finally {
      if (mounted) setState(() => busy = false);
    }
  }

  Future<void> approveConsent() async {
    setState(() => busy = true);
    try {
      final p = await pairApproveAsync(pairId, consentAllow.toList()..sort());
      if (mounted) {
        setState(() {
          consent = null;
          awaitingCode = true;
          pairUserCode = p['user_code'] as String?;
          pairLinkUrl = p['link_url'] as String?;
        });
      }
    } catch (e) {
      // Ask the kernel whether this consent request can still be approved.
      Map<String, dynamic>? st;
      try {
        st = await pairStatusAsync(pairId);
      } catch (_) {}
      if (!mounted) return;
      if (approveRetryable(st)) {
        show('授权请求失败，可直接重试：$e', error: true);
      } else {
        // the request is gone: back to the address step to start over
        setState(() {
          consent = null;
          pairId = null;
        });
        show('配对请求已失效，请重新开始配对：$e', error: true);
      }
    } finally {
      if (mounted) setState(() => busy = false);
    }
  }

  Future<void> denyConsent() async {
    try {
      await pairDenyAsync(pairId);
    } catch (_) {}
    if (mounted) {
      setState(() {
        consent = null;
        pendingRemoteURL = null;
        pairId = null;
      });
      show('已拒绝该服务的配对请求（CONSENT_DENIED）');
    }
  }

  Future<void> loadUsage() async {
    try {
      final u = await usageStatsAsync(['adapter'], 7);
      if (mounted) setState(() => usage = u);
    } catch (_) {}
  }

  String _schemeName(String id) {
    for (final a in adapters) {
      if (a['id'] == id) {
        final n = a['name'];
        return n is String && n.isNotEmpty ? n : id;
      }
    }
    return id;
  }

  static String _fmtTokens(num n) {
    String trim(double v) {
      final s = v.toStringAsFixed(1);
      return s.endsWith('.0') ? s.substring(0, s.length - 2) : s;
    }

    if (n >= 1000000) return '${trim(n / 1000000)}M';
    if (n >= 1000) return '${trim(n / 1000)}k';
    return '$n';
  }

  Future<void> pollPairing() async {
    if (pairUserCode == null || !awaitingCode) return;
    try {
      final st = await pairStatusAsync(pairId);
      if (!mounted) return;
      if (st['state'] == 'paired') {
        setState(() {
          awaitingCode = false;
          adding = false;
          pairId = null;
          pendingRemoteURL = null;
          pairUserCode = null;
          pairLinkUrl = null;
        });
        await refresh(force: true);
        show('配对成功，正在连接远端服务');
      } else if (st['state'] == 'failed' || st['state'] == 'denied') {
        setState(() {
          awaitingCode = false;
          pairUserCode = null;
          pairLinkUrl = null;
        });
        show('配对失败：${st['error'] ?? ''}', error: true);
      }
    } catch (_) {}
  }

  Future<void> cancelPairing() async {
    try {
      await pairCancelAsync(pairId);
    } catch (_) {}
    if (mounted) {
      setState(() {
        awaitingCode = false;
        pairId = null;
        pendingRemoteURL = null;
        pairUserCode = null;
        pairLinkUrl = null;
      });
    }
  }

  Future<void> connectionAction(ConnectionInfo c, String action) async {
    if (action == 'connection_revoke') {
      final yes = await confirm(
        title: '撤销连接',
        message: '将断开「${c.title}」并删除本机为它保存的设备私钥，其他服务不受影响。模型配置会保留；再次连接需要重新配对。',
        action: '撤销',
        destructive: true,
      );
      if (!yes || !mounted) return;
    }
    setState(() => busy = true);
    try {
      final r = await connectionActionAsync(action, c.id);
      await refresh(force: true);
      if (action == 'connection_revoke') {
        for (final line in revokeOutcome(r, c.title)) {
          show(line.text, error: line.error);
        }
      } else {
        show(switch (action) {
          'connection_disconnect' => '已断开「${c.title}」',
          _ => '正在重新连接「${c.title}」',
        });
      }
    } catch (e) {
      if (mounted) show('操作失败：$e', error: true);
    } finally {
      if (mounted) setState(() => busy = false);
    }
  }

  Future<void> editPolicy(ConnectionInfo c) async {
    // Every model (incl. disabled adapters), wildcards and unknown entries
    // are listed so that saving keeps authorizations the user did not touch.
    final entries = policyEntries(adapters, c.allow);
    final selected = <String>{...c.allow};
    final ok = await showOverlay<bool>(
      context,
      DialogConfiguration<bool>(
        builder: (ctx) => StatefulBuilder(
          builder: (ctx, setLocal) => AlertDialog(
            title: Text('「${c.title}」可用模型'),
            content: ConstrainedBox(
              constraints: const BoxConstraints(maxWidth: 460),
              child: ModelChecklist(
                models: [for (final e in entries) e.id],
                notes: {for (final e in entries) e.id: e.note},
                labels: {for (final e in entries) e.id: e.label},
                details: {for (final e in entries) e.id: e.detail},
                selected: selected,
                lockedBy: (m) => coveredByWildcard(m, selected),
                onChanged: (m, v) =>
                    setLocal(() => v ? selected.add(m) : selected.remove(m)),
              ),
            ),
            actions: [
              OutlineButton(
                onPressed: () => Navigator.of(ctx).pop(false),
                child: const Text('取消'),
              ),
              PrimaryButton(
                onPressed: () => Navigator.of(ctx).pop(true),
                child: const Text('保存'),
              ),
            ],
          ),
        ),
      ),
    ).future;
    if (ok != true || !mounted) return;
    try {
      // allow only: the connection's max_concurrency is left unchanged
      await setConnectionPolicyAsync(c.id, selected.toList()..sort());
      await refresh(force: true);
      show('已更新「${c.title}」的可用模型');
    } catch (e) {
      if (mounted) show('保存失败：$e', error: true);
    }
  }

  Future<void> openConnection() async {
    await showOverlay<void>(
      context,
      DialogConfiguration<void>(
        builder: (_) => _DialogRelay(
          host: this,
          title: '远端服务连接',
          content: connectionCard,
          maxWidth: 800,
        ),
      ),
    ).future;
  }

  Future<void> openUsage() async {
    await loadUsage();
    if (!mounted) return;
    await showOverlay<void>(
      context,
      DialogConfiguration<void>(
        builder: (_) =>
            _DialogRelay(host: this, title: '用量统计', content: usageCard),
      ),
    ).future;
  }

  Widget usageCard() {
    final rows = [...usage]
      ..sort(
        (a, b) => (b['total_tokens'] ?? 0).compareTo(a['total_tokens'] ?? 0),
      );
    Widget cell(String v, {bool head = false, bool right = false}) {
      final t = Text(v, textAlign: right ? TextAlign.right : null);
      return right
          ? SizedBox(width: 72, child: head ? t.xSmall().muted() : t.small())
          : Expanded(
              child: head
                  ? t.xSmall().muted()
                  : t.small().medium().singleLine().ellipsis,
            );
    }

    return Column(
      mainAxisSize: MainAxisSize.min,
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        Row(
          children: [
            Expanded(child: const Text('近 7 天用量').small().medium()),
            GhostButton(
              size: ButtonSize.small,
              onPressed: loadUsage,
              child: const Text('刷新用量'),
            ),
          ],
        ),
        const Gap(10),
        if (rows.isEmpty)
          const Text('暂无记录（不记录对话内容）').small().muted()
        else ...[
          Row(
            children: [
              cell('方案名', head: true),
              cell('输入', head: true, right: true),
              cell('输出', head: true, right: true),
              cell('总计', head: true, right: true),
            ],
          ),
          const Gap(4),
          const Divider(),
          for (final b in rows) ...[
            const Gap(2),
            Row(
              children: [
                cell(_schemeName('${b['adapter'] ?? ''}')),
                cell(_fmtTokens(b['prompt_tokens'] ?? 0), right: true),
                cell(_fmtTokens(b['completion_tokens'] ?? 0), right: true),
                cell(_fmtTokens(b['total_tokens'] ?? 0), right: true),
              ],
            ),
            const Gap(2),
          ],
        ],
      ],
    );
  }

  Future<void> edit([Map<String, dynamic>? existing]) async {
    final changed = await showOverlay<bool>(
      context,
      DialogConfiguration<bool>(
        builder: (_) => AdapterDialog(api: api, existing: existing),
      ),
    ).future;
    if (changed == true) await refresh();
  }

  Future<void> remove(Map<String, dynamic> adapter) async {
    final label = adapter['name']?.toString().isNotEmpty == true
        ? adapter['name']
        : adapter['id'];
    final yes = await confirm(
      title: '删除适配器',
      message: '删除 $label？',
      action: '删除',
      destructive: true,
    );
    if (!yes) return;
    try {
      api.deleteAdapter(adapter['id'] as String);
      await refresh();
    } catch (e) {
      if (mounted) show('$e', error: true);
    }
  }

  Future<void> dismissRecovery() async {
    setState(() => recoveryDismissed = true);
    try {
      await ackAdapterRecoveryAsync();
      await refresh();
    } catch (e) {
      if (mounted) show('$e', error: true);
    }
  }

  Widget connectionCard() {
    if (!kernelReady) {
      return Card(
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Alert(
              destructive: true,
              leading: const Icon(LucideIcons.triangleAlert),
              title: const Text('内核启动失败'),
              content: Text(kernelError ?? '内核启动失败'),
            ),
            const Gap(16),
            Align(
              alignment: Alignment.centerRight,
              child: PrimaryButton(
                onPressed: boot,
                leading: const Icon(LucideIcons.refreshCw),
                child: const Text('重试'),
              ),
            ),
          ],
        ),
      );
    }

    final paired = status?['paired'] == true;
    final recovery = status?['adapter_recovery'] as Map?;
    final step = paired && !adding && consent == null && !awaitingCode
        ? 3
        : awaitingCode
        ? 2
        : consent != null
        ? 1
        : 0;
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        if (recovery != null && !recoveryDismissed) ...[
          // Informational, dismissible and shown once: the app works
          // normally with an empty adapter list; the old file is kept.
          Alert(
            leading: const Icon(LucideIcons.info),
            title: const Text('旧的模型方案配置已备份'),
            content: Text(
              '本机保存的模型方案配置无法用当前密钥解密（可能来自旧版本应用），'
              '已备份到：${recovery['backup']}\n'
              '应用可以正常使用，请重新添加模型方案即可。',
            ),
            trailing: GhostButton(
              onPressed: dismissRecovery,
              child: const Text('知道了'),
            ),
          ),
          const Gap(16),
        ],
        if (step == 3)
          _connectStepPane(step)
        else
          Row(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              SizedBox(width: 140, child: _StepRail(current: step)),
              const Gap(24),
              Expanded(child: _connectStepPane(step)),
            ],
          ),
      ],
    );
  }

  Widget _connectStepPane(int step) {
    final theme = Theme.of(context);
    switch (step) {
      case 1: // 身份验证：consent — the SaaS identity was verified by signature
        final saas =
            (consent?['saas'] as Map?)?.cast<String, dynamic>() ?? const {};
        final saasName = '${saas['name'] ?? ''}'.trim();
        final verified = consent?['verified'] == true;
        return Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            _StepHeader(
              icon: LucideIcons.shieldCheck,
              title: saasName.isEmpty ? '确认服务授权' : '「$saasName」请求使用本机模型',
              desc: verified ? '身份已通过网关签名校验' : '身份未校验',
            ),
            const Gap(14),
            _ConsentView(consent: consent!),
            const Gap(12),
            const Text('可用本机模型').small().medium(),
            const Gap(6),
            ModelChecklist(
              models: consentModels,
              labels: schemeLabels(adapters),
              details: {
                for (final m in consentModels)
                  if (m.contains('/')) m: m.substring(m.indexOf('/') + 1),
              },
              selected: consentAllow,
              onChanged: (m, v) => setState(
                () => v ? consentAllow.add(m) : consentAllow.remove(m),
              ),
            ),
            const Gap(16),
            Row(
              mainAxisAlignment: MainAxisAlignment.end,
              children: [
                OutlineButton(
                  onPressed: busy ? null : denyConsent,
                  child: const Text('拒绝'),
                ),
                const Gap(8),
                PrimaryButton(
                  onPressed: busy ? null : approveConsent,
                  leading: const Icon(LucideIcons.link),
                  child: Text(busy ? '处理中…' : '同意并配对'),
                ),
              ],
            ),
          ],
        );
      case 2: // 确认授权：show the pairing code for the remote page
        return Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            const _StepHeader(
              icon: LucideIcons.keyRound,
              title: '输入配对码',
              desc: '在远端服务页面输入此配对码即可完成连接',
            ),
            const Gap(16),
            Container(
              decoration: BoxDecoration(
                color: theme.colorScheme.muted.withValues(alpha: 0.55),
                border: Border.all(color: theme.colorScheme.border),
                borderRadius: BorderRadius.circular(10),
              ),
              padding: const EdgeInsets.symmetric(vertical: 20, horizontal: 16),
              child: Column(
                children: [
                  Row(
                    mainAxisSize: MainAxisSize.min,
                    children: [
                      Text(
                        pairUserCode ?? '',
                        style: const TextStyle(
                          fontSize: 28,
                          fontWeight: FontWeight.w600,
                          letterSpacing: 4,
                          fontFamily: 'monospace',
                        ),
                      ),
                      const Gap(6),
                      Tooltip(
                        tooltip: (context) =>
                            const TooltipContainer(child: Text('复制配对码')),
                        child: IconButton.ghost(
                          density: ButtonDensity.icon,
                          icon: const Icon(LucideIcons.copy),
                          onPressed: () {
                            Clipboard.setData(
                              ClipboardData(text: pairUserCode ?? ''),
                            );
                            show('配对码已复制');
                          },
                        ),
                      ),
                    ],
                  ),
                  const Gap(6),
                  const Text('10 分钟内有效').xSmall().muted(),
                ],
              ),
            ),
            const Gap(14),
            Align(
              alignment: Alignment.centerRight,
              child: GhostButton(
                onPressed: busy ? null : cancelPairing,
                child: const Text('返回'),
              ),
            ),
          ],
        );
      case 3: // 连接列表：every paired SaaS
        final ds = DesktopStatus.parse(status);
        return Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Row(
              children: [
                Expanded(
                  child: _StepHeader(
                    icon: LucideIcons.circleCheck,
                    iconColor: theme.colorScheme.chart2,
                    title: ds.total > 1
                        ? '已成功连接（${ds.online}/${ds.total} 在线）'
                        : '已成功连接',
                  ),
                ),
                Tooltip(
                  tooltip: (context) =>
                      const TooltipContainer(child: Text('刷新')),
                  child: IconButton.ghost(
                    density: ButtonDensity.icon,
                    icon: const Icon(LucideIcons.refreshCw),
                    onPressed: busy ? null : () => refresh(),
                  ),
                ),
                const Gap(6),
                OutlineButton(
                  size: ButtonSize.small,
                  onPressed: busy
                      ? null
                      : () => setState(() {
                          adding = true;
                          remoteUrl.clear();
                        }),
                  leading: const Icon(LucideIcons.plus, size: 14),
                  child: const Text('添加连接'),
                ),
              ],
            ),
            const Gap(14),
            for (final c in ds.connections) ...[
              _ConnectionTile(
                adapters: adapters,
                c: c,
                busy: busy,
                onToggle: () => connectionAction(
                  c,
                  c.paused ? 'connection_connect' : 'connection_disconnect',
                ),
                onRevoke: () => connectionAction(c, 'connection_revoke'),
                onModels: () => editPolicy(c),
              ),
              const Gap(10),
            ],
            _StatRow(
              '设备',
              '${status?['device_name'] ?? status?['device_id'] ?? ''}',
            ),
            _StatRow('任务', '${status?['running_tasks'] ?? 0} 个'),
          ],
        );
      default: // 输入地址
        return Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            const _StepHeader(icon: LucideIcons.link, title: '连接远端服务'),
            const Gap(14),
            TextField(
              controller: remoteUrl,
              placeholder: const Text('输入服务地址（https://…）或配对链接（lmbp://pair?…）'),
              features: const [InputFeature.clear()],
              onSubmitted: (_) {
                if (!busy) prepareConnection();
              },
            ),
            if ((status?['pairing_error'] ?? '') != '') ...[
              const Gap(8),
              Text(
                '${status?['pairing_error']}',
                style: TextStyle(color: theme.colorScheme.destructive),
              ).xSmall(),
            ],
            const Gap(16),
            Row(
              mainAxisAlignment: MainAxisAlignment.end,
              children: [
                if (adding) ...[
                  GhostButton(
                    onPressed: busy
                        ? null
                        : () => setState(() => adding = false),
                    child: const Text('返回连接列表'),
                  ),
                  const Gap(8),
                ],
                PrimaryButton(
                  onPressed: busy ? null : prepareConnection,
                  leading: const Icon(LucideIcons.link),
                  child: Text(busy ? '处理中…' : '连接'),
                ),
              ],
            ),
          ],
        );
    }
  }

  Widget adapterTile(Map<String, dynamic> adapter) {
    final type = adapter['type'] as String?;
    final health = adapter['enabled'] != true
        ? '停用'
        : adapter['health'] == 'ready'
        ? '可用'
        : adapter['health'] == 'storage_full'
        ? '存储已满'
        : adapter['health'] == 'unavailable'
        ? '不可用'
        : '检测中';
    final models = (adapter['models'] as List?)?.join(', ') ?? '';
    final subtitle = StringBuffer('${adapterTypeShort(type)} · $models');
    if (adapter['auth_state'] == 'required') subtitle.write(' · 需要登录');
    final name = (adapter['name'] as String?)?.isNotEmpty == true
        ? adapter['name'] as String
        : '${adapter['id']}';
    return Card(
      padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 12),
      child: Basic(
        padding: EdgeInsets.zero,
        leadingAlignment: Alignment.center,
        trailingAlignment: Alignment.center,
        leading: _TypeMark(type),
        title: Text(name).medium().singleLine().ellipsis,
        subtitle: Text(
          subtitle.toString(),
        ).small().muted().singleLine().ellipsis,
        trailing: Row(
          mainAxisSize: MainAxisSize.min,
          children: [
            _HealthBadge(health),
            const Gap(4),
            Tooltip(
              tooltip: (context) => const TooltipContainer(child: Text('编辑')),
              child: IconButton.ghost(
                density: ButtonDensity.icon,
                icon: const Icon(LucideIcons.pencil),
                onPressed: () => edit(adapter),
              ),
            ),
            Tooltip(
              tooltip: (context) => const TooltipContainer(child: Text('删除')),
              child: IconButton.destructive(
                density: ButtonDensity.icon,
                icon: const Icon(LucideIcons.trash2),
                onPressed: () => remove(adapter),
              ),
            ),
          ],
        ),
      ),
    );
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      backgroundColor: const Color(0xFFFFFFFF),
      loadingProgressIndeterminate: busy,
      child: AppShell(
        status: status,
        child: kernelReady
            ? ProfileWorkspace(
                api: api,
                adapters: adapters,
                status: status,
                onConnectionTap: openConnection,
                onUsageTap: openUsage,
                onChanged: () => refresh(force: true),
              )
            : Center(
                child: ConstrainedBox(
                  constraints: const BoxConstraints(maxWidth: 560),
                  child: connectionCard(),
                ),
              ),
      ),
    );
  }
}

/// Shared outer layout: kernel-wide warnings stay visible on every page
/// (e.g. after the last connection is revoked and the pairing page shows).
class AppShell extends StatelessWidget {
  const AppShell({super.key, required this.status, required this.child});

  final Map<String, dynamic>? status;
  final Widget child;

  @override
  Widget build(BuildContext context) {
    // The tree shape never changes (only the bar's content does), so the
    // page below keeps its state (e.g. unsaved drafts) when a warning shows.
    final warning = DesktopStatus.parse(status).cleanupWarning;
    final theme = Theme.of(context);
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        if (warning == null)
          const SizedBox.shrink(key: ValueKey('cleanup-warning-slot'))
        else
          Container(
            key: const ValueKey('cleanup-warning'),
            color: theme.colorScheme.destructive.withValues(alpha: 0.08),
            padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 8),
            child: Text(
              '本机私钥清理未完成（将在每次启动时重试）：$warning',
              style: TextStyle(color: theme.colorScheme.destructive),
            ).small,
          ),
        Expanded(key: const ValueKey('app-shell-body'), child: child),
      ],
    );
  }
}

/// Numbered rail for the connect dialog's 4 steps.
class _StepRail extends StatelessWidget {
  const _StepRail({required this.current});

  final int current;
  static const _labels = ['输入地址', '身份验证', '确认授权', '完成连接'];

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        for (var i = 0; i < _labels.length; i++) ...[
          Row(
            children: [
              Container(
                width: 22,
                height: 22,
                decoration: BoxDecoration(
                  shape: BoxShape.circle,
                  color: i <= current
                      ? theme.colorScheme.primary
                      : theme.colorScheme.muted,
                ),
                alignment: Alignment.center,
                child: i < current
                    ? const Icon(
                        LucideIcons.check,
                        size: 12,
                        color: Colors.white,
                      )
                    : Text(
                        '${i + 1}',
                        style: TextStyle(
                          fontSize: 11,
                          color: i == current
                              ? Colors.white
                              : theme.colorScheme.mutedForeground,
                        ),
                      ),
              ),
              const Gap(8),
              Expanded(
                child: i <= current
                    ? Text(_labels[i]).small().medium()
                    : Text(_labels[i]).small().muted(),
              ),
            ],
          ),
          if (i < _labels.length - 1)
            Padding(
              padding: const EdgeInsets.only(left: 10.5, top: 4, bottom: 4),
              child: Align(
                alignment: Alignment.centerLeft,
                child: Container(
                  width: 1,
                  height: 14,
                  color: i < current
                      ? theme.colorScheme.primary
                      : theme.colorScheme.border,
                ),
              ),
            ),
        ],
      ],
    );
  }
}

/// Icon tile + title + description header for each connect step.
class _StepHeader extends StatelessWidget {
  const _StepHeader({
    required this.icon,
    required this.title,
    this.desc,
    this.iconColor,
  });

  final IconData icon;
  final String title;
  final String? desc;
  final Color? iconColor;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final color = iconColor ?? theme.colorScheme.primary;
    return Row(
      children: [
        Container(
          width: 40,
          height: 40,
          decoration: BoxDecoration(
            color: color.withValues(alpha: 0.12),
            borderRadius: BorderRadius.circular(10),
          ),
          alignment: Alignment.center,
          child: Icon(icon, size: 20, color: color),
        ),
        const Gap(12),
        Expanded(
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text(title).medium(),
              if (desc != null) ...[const Gap(3), Text(desc!).xSmall().muted()],
            ],
          ),
        ),
      ],
    );
  }
}

class _DialogRelay extends StatefulWidget {
  const _DialogRelay({
    required this.host,
    required this.title,
    required this.content,
    this.maxWidth = 420,
  });

  final _HomePageState host;
  final String title;
  final Widget Function() content;
  final double maxWidth;

  @override
  State<_DialogRelay> createState() => _DialogRelayState();
}

class _DialogRelayState extends State<_DialogRelay> {
  void refresh() {
    if (mounted) setState(() {});
  }

  @override
  void initState() {
    super.initState();
    widget.host._overlayRelays.add(refresh);
  }

  @override
  void dispose() {
    widget.host._overlayRelays.remove(refresh);
    super.dispose();
  }

  @override
  Widget build(BuildContext context) => ConstrainedBox(
    constraints: BoxConstraints(maxWidth: widget.maxWidth),
    child: AlertDialog(
      title: Row(
        children: [
          Expanded(child: Text(widget.title)),
          IconButton.ghost(
            density: ButtonDensity.icon,
            icon: const Icon(LucideIcons.x),
            onPressed: () => Navigator.of(context).pop(),
          ),
        ],
      ),
      content: SingleChildScrollView(child: widget.content()),
    ),
  );
}

class LabeledField extends StatelessWidget {
  const LabeledField({
    super.key,
    required this.label,
    required this.child,
    this.hint,
  });

  final String label;
  final Widget child;
  final String? hint;

  @override
  Widget build(BuildContext context) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        Text(label).small().medium(),
        const Gap(6),
        child,
        if (hint != null) ...[const Gap(4), Text(hint!).xSmall().muted()],
      ],
    );
  }
}

class _StatRow extends StatelessWidget {
  const _StatRow(this.label, this.value);

  final String label;
  final String value;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.only(top: 6),
      child: Row(
        children: [
          SizedBox(
            width: 64,
            child: Text(label).small().muted().singleLine().ellipsis,
          ),
          Expanded(child: Text(value).small().medium().singleLine().ellipsis),
        ],
      ),
    );
  }
}

class _TypeMark extends StatelessWidget {
  const _TypeMark(this.type);

  final String? type;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final icon = switch (type) {
      'codex_app_server' => LucideIcons.terminal,
      'ollama_http' => LucideIcons.monitor,
      _ => LucideIcons.cloud,
    };
    return Container(
      width: 36,
      height: 36,
      alignment: Alignment.center,
      decoration: BoxDecoration(
        color: theme.colorScheme.muted,
        borderRadius: BorderRadius.circular(theme.radiusMd),
      ),
      child: Icon(icon, size: 16, color: theme.colorScheme.foreground),
    );
  }
}

class _HealthBadge extends StatelessWidget {
  const _HealthBadge(this.health);

  final String health;

  @override
  Widget build(BuildContext context) {
    final child = Text(health);
    return switch (health) {
      '可用' => PrimaryBadge(child: child),
      '停用' => OutlineBadge(child: child),
      '存储已满' || '不可用' => DestructiveBadge(child: child),
      _ => SecondaryBadge(child: child),
    };
  }
}

String adapterTypeShort(String? type) => switch (type) {
  'codex_app_server' => 'Codex',
  'anthropic_compatible' => 'Anthropic 兼容',
  'ollama_http' => 'Ollama',
  _ => 'OpenAI 兼容',
};

String adapterTypeLabel(String type) => switch (type) {
  'codex_app_server' => 'OpenAI Codex（订阅）',
  'anthropic_compatible' => 'Anthropic 兼容协议',
  'ollama_http' => '旧版 Ollama',
  _ => 'OpenAI 兼容协议',
};

class AdapterDialog extends StatefulWidget {
  const AdapterDialog({super.key, required this.api, this.existing});

  final NativeApi api;
  final Map<String, dynamic>? existing;

  @override
  State<AdapterDialog> createState() => _AdapterDialogState();
}

class _AdapterDialogState extends State<AdapterDialog> {
  late final id = TextEditingController(text: widget.existing?['id'] ?? '');
  late final name = TextEditingController(text: widget.existing?['name'] ?? '');
  late final url = TextEditingController(
    text: widget.existing?['base_url'] ?? '',
  );
  late final codex = TextEditingController(
    text: widget.existing?['codex_bin'] ?? 'codex',
  );
  late final models = TextEditingController(
    text: (widget.existing?['models'] as List?)?.join(', ') ?? '',
  );
  final apiKey = TextEditingController();
  late String type = widget.existing?['type'] ?? 'codex_app_server';
  late bool skipModelProbe = widget.existing?['skip_model_probe'] ?? false;
  bool saving = false;
  String? error;

  @override
  void dispose() {
    for (final controller in [id, name, url, codex, models, apiKey]) {
      controller.dispose();
    }
    super.dispose();
  }

  Future<void> save() async {
    setState(() {
      saving = true;
      error = null;
    });
    try {
      final body = <String, dynamic>{
        'id': id.text.trim(),
        'name': name.text.trim(),
        'type': type,
        'models': models.text
            .split(',')
            .map((item) => item.trim())
            .where((item) => item.isNotEmpty)
            .toList(),
        // edit is a partial update: keep the stored enabled state
        'enabled': widget.existing?['enabled'] ?? true,
        'skip_model_probe':
            (type == 'openai_compatible' || type == 'anthropic_compatible') &&
            skipModelProbe,
      };
      if (type == 'codex_app_server') {
        body['codex_bin'] = codex.text.trim();
      } else {
        body['base_url'] = url.text.trim();
      }
      if (type == 'openai_compatible' || type == 'anthropic_compatible') {
        body['api_key'] = apiKey.text.trim();
      }
      // off the UI thread: saving may probe the provider over the network
      if (widget.existing == null) {
        await createAdapterAsync(body);
      } else {
        await updateAdapterAsync(body);
      }
      if (mounted) Navigator.pop(context, true);
    } catch (e) {
      if (mounted) setState(() => error = e.toString());
    } finally {
      if (mounted) setState(() => saving = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final creating = widget.existing == null;
    return AlertDialog(
      title: Text(creating ? '添加适配器' : '编辑适配器'),
      content: DefaultTextStyle(
        style: theme.typography.small.copyWith(
          color: theme.colorScheme.foreground,
        ),
        child: SizedBox(
          width: 460,
          child: SingleChildScrollView(
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.stretch,
              mainAxisSize: MainAxisSize.min,
              children: [
                LabeledField(
                  label: '类型',
                  child: Select<String>(
                    value: type,
                    enabled: creating,
                    constraints: const BoxConstraints(minWidth: 460),
                    placeholder: const Text('选择类型'),
                    itemBuilder: (context, item) =>
                        Text(adapterTypeLabel(item)),
                    onChanged: creating
                        ? (value) {
                            if (value == null) return;
                            setState(() => type = value);
                          }
                        : null,
                    popup: (context) => const SelectPopup(
                      items: SelectItemList(
                        children: [
                          SelectItemButton(
                            value: 'codex_app_server',
                            child: Text('OpenAI Codex（订阅）'),
                          ),
                          SelectItemButton(
                            value: 'openai_compatible',
                            child: Text('OpenAI 兼容协议'),
                          ),
                          SelectItemButton(
                            value: 'anthropic_compatible',
                            child: Text('Anthropic 兼容协议'),
                          ),
                        ],
                      ),
                    ),
                  ),
                ),
                const Gap(14),
                LabeledField(
                  label: '适配器 ID',
                  child: TextField(
                    controller: id,
                    enabled: creating,
                    placeholder: const Text('例如 local-codex'),
                  ),
                ),
                const Gap(14),
                LabeledField(
                  label: '显示名称',
                  child: TextField(
                    controller: name,
                    placeholder: const Text('在列表中显示的名称'),
                  ),
                ),
                const Gap(14),
                if (type == 'codex_app_server')
                  LabeledField(
                    label: '官方 Codex CLI 路径',
                    child: TextField(
                      controller: codex,
                      placeholder: const Text('codex'),
                    ),
                  )
                else
                  LabeledField(
                    label: 'API 根地址（HTTPS 或本机 HTTP）',
                    child: TextField(
                      controller: url,
                      placeholder: const Text('https://host/v1'),
                    ),
                  ),
                if (type == 'openai_compatible' ||
                    type == 'anthropic_compatible') ...[
                  const Gap(14),
                  LabeledField(
                    label: widget.existing?['has_api_key'] == true
                        ? 'API Key（留空保留原密钥）'
                        : 'API Key',
                    child: TextField(
                      controller: apiKey,
                      placeholder: const Text('sk-...'),
                      features: const [InputFeature.passwordToggle()],
                    ),
                  ),
                ],
                const Gap(14),
                LabeledField(
                  label: '允许的模型 ID（逗号分隔）',
                  child: TextField(
                    controller: models,
                    placeholder: const Text('gpt-4o, gpt-4o-mini'),
                  ),
                ),
                const Gap(8),
                if (error != null) ...[
                  const Gap(8),
                  Text(
                    error!,
                    style: TextStyle(color: theme.colorScheme.destructive),
                  ).small(),
                ],
              ],
            ),
          ),
        ),
      ),
      actions: [
        OutlineButton(
          onPressed: saving ? null : () => Navigator.pop(context, false),
          child: const Text('取消'),
        ),
        PrimaryButton(
          onPressed: saving ? null : save,
          child: Text(saving ? '保存中…' : '保存'),
        ),
      ],
    );
  }
}

/// Consent screen: the SaaS identity verified from the gateway's signed
/// metadata (docs/lmbp-spec.md §19). Nothing here comes from the link.
class _ConsentView extends StatelessWidget {
  const _ConsentView({required this.consent});
  final Map<String, dynamic> consent;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final saas = (consent['saas'] as Map?)?.cast<String, dynamic>() ?? {};
    final scopes = ((consent['requested_scopes'] as List?) ?? []).join(', ');
    final gateway = '${consent['gateway'] ?? ''}';
    final homepage = '${saas['homepage_uri'] ?? ''}';
    final policy = '${saas['policy_uri'] ?? ''}';
    return Container(
      decoration: BoxDecoration(
        color: theme.colorScheme.muted.withValues(alpha: 0.4),
        border: Border.all(color: theme.colorScheme.border),
        borderRadius: BorderRadius.circular(10),
      ),
      padding: const EdgeInsets.fromLTRB(14, 8, 14, 12),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          _StatRow('网关', gateway),
          _StatRow('租户', '${consent['tenant'] ?? ''}'),
          if (homepage.isNotEmpty && homepage != gateway)
            _StatRow('主页', homepage),
          if (policy.isNotEmpty) _StatRow('隐私政策', policy),
          if (scopes.isNotEmpty) _StatRow('请求权限', scopes),
          _StatRow('网关密钥', '${consent['gateway_key_id'] ?? ''}'),
        ],
      ),
    );
  }
}

/// Checkbox list of local models ("adapter/model") for one SaaS.
class ModelChecklist extends StatelessWidget {
  Widget _label(String m, String note) {
    final l = (labels[m] ?? '').isEmpty ? m : labels[m]!;
    final main = Text(note.isEmpty ? l : '$l  · $note').small();
    final d = details[m] ?? '';
    if (d.isEmpty || d == l) return main;
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      mainAxisSize: MainAxisSize.min,
      children: [main, Text(d).xSmall().muted()],
    );
  }

  const ModelChecklist({
    super.key,
    required this.models,
    required this.selected,
    required this.onChanged,
    this.notes = const {},
    this.labels = const {},
    this.details = const {},
    this.lockedBy,
  });

  /// Display only: scheme name per stored id (falls back to the id).
  final Map<String, String> labels;

  /// Display only: small secondary text per id (e.g. the upstream model).
  final Map<String, String> details;

  final List<String> models;
  final Set<String> selected;
  final void Function(String model, bool value) onChanged;

  /// Optional hint per model (e.g. 方案已停用).
  final Map<String, String> notes;

  /// Models already granted by a selected wildcard: shown checked, read-only.
  final bool Function(String model)? lockedBy;

  @override
  Widget build(BuildContext context) {
    if (models.isEmpty) {
      return const Text('还没有启用的模型方案，可稍后添加方案后在连接列表中授权。').small().muted();
    }
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        for (final m in models)
          Padding(
            padding: const EdgeInsets.symmetric(vertical: 3),
            child: Builder(
              builder: (context) {
                final locked = lockedBy?.call(m) ?? false;
                final note = locked ? '已由通配授权' : (notes[m] ?? '');
                return Checkbox(
                  state: locked || selected.contains(m)
                      ? CheckboxState.checked
                      : CheckboxState.unchecked,
                  onChanged: locked
                      ? null
                      : (v) => onChanged(m, v == CheckboxState.checked),
                  trailing: _label(m, note),
                );
              },
            ),
          ),
      ],
    );
  }
}

/// One paired SaaS in the connections list.
class _ConnectionTile extends StatelessWidget {
  const _ConnectionTile({
    required this.c,
    this.adapters = const [],
    required this.busy,
    required this.onToggle,
    required this.onRevoke,
    required this.onModels,
  });

  final ConnectionInfo c;
  final List<Map<String, dynamic>> adapters;
  final bool busy;
  final VoidCallback onToggle;
  final VoidCallback onRevoke;
  final VoidCallback onModels;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final color = c.connected
        ? theme.colorScheme.chart2
        : (c.state == 'revoked' || c.state == 'error')
        ? theme.colorScheme.destructive
        : theme.colorScheme.mutedForeground;
    final allow = allowSummary(c.allow, adapters);
    return Container(
      decoration: BoxDecoration(
        border: Border.all(color: theme.colorScheme.border),
        borderRadius: BorderRadius.circular(10),
      ),
      padding: const EdgeInsets.fromLTRB(14, 10, 14, 12),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          Row(
            children: [
              Container(
                width: 8,
                height: 8,
                decoration: BoxDecoration(color: color, shape: BoxShape.circle),
              ),
              const Gap(8),
              Expanded(child: Text(c.title).medium().singleLine().ellipsis),
              Text(c.stateLabel).xSmall().muted(),
              const Gap(10),
              GhostButton(
                size: ButtonSize.small,
                onPressed: busy ? null : onModels,
                child: const Text('模型'),
              ),
              if (c.state != 'revoked' && c.state != 'error')
                OutlineButton(
                  size: ButtonSize.small,
                  onPressed: busy ? null : onToggle,
                  child: Text(c.paused ? '连接' : '断开'),
                ),
              const Gap(6),
              DestructiveButton(
                size: ButtonSize.small,
                onPressed: busy ? null : onRevoke,
                child: const Text('撤销'),
              ),
            ],
          ),
          _StatRow('网关', c.issuer),
          _StatRow('租户', c.tenant),
          if (c.displayUrl.isNotEmpty) _StatRow('地址', c.displayUrl),
          _StatRow('传输', _transportLabel(c.transport)),
          _StatRow('模型', allow),
          _StatRow('任务', '${c.runningTasks} 个（上限 ${c.maxConcurrency}）'),
          if (c.lastError.isNotEmpty) _StatRow('错误', c.lastError),
        ],
      ),
    );
  }
}
