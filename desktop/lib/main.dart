import 'dart:async';
import 'dart:io';

import 'backend_catalog.dart';

import 'package:flutter/services.dart';
import 'package:shadcn_flutter/shadcn_flutter.dart';
import 'package:window_manager/window_manager.dart';

import 'native_api.dart';
import 'status_contract.dart';
import 'app_dialog.dart';
export 'app_dialog.dart';
import 'profile_workspace.dart';
import 'tokens.dart';
import 'tray.dart';
import 'ui_kit.dart';
export 'tokens.dart';
export 'ui_kit.dart';

String _transportLabel(Object? transport) => switch (transport) {
  'wss' => 'WSS（lmbp）',
  'ws' => 'WS（lmbp，仅本机联调）',
  _ => '未连接',
};

/// Light theme from the design tokens.
ThemeData appLightTheme() => ThemeData(
  colorScheme: AppTokens.light.colorScheme(Brightness.light),
  radius: 0.5,
);

/// Dark theme from the design tokens (same layout, dark token set).
ThemeData appDarkTheme() => ThemeData.dark(
  colorScheme: AppTokens.dark.colorScheme(Brightness.dark),
  radius: 0.5,
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
    title: 'Avora',
    debugShowCheckedModeBanner: false,
    theme: appLightTheme(),
    darkTheme: appDarkTheme(),
    // follows the system appearance
    themeMode: ThemeMode.system,
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

  /// When the current pair code was issued (drives the countdown).
  DateTime? pairStartedAt;
  // Pairing session id (pair_id) and the models chosen on the consent page
  // (default deny: nothing is checked until the user picks).
  String? pairId;
  List<String> consentModels = [];
  ModelAccess consentAccess = ModelAccess(const []);
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
        final t = AppTokens.of(context);
        return Container(
          padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 12),
          decoration: BoxDecoration(
            color: t.surface,
            borderRadius: BorderRadius.circular(AppRadius.control),
            border: Border.all(color: t.border),
            boxShadow: [
              BoxShadow(
                color: t.shadow,
                blurRadius: 24,
                offset: const Offset(0, 8),
              ),
            ],
          ),
          child: Row(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Padding(
                padding: const EdgeInsets.only(top: 2),
                child: Icon(
                  error ? LucideIcons.triangleAlert : LucideIcons.circleCheck,
                  size: 16,
                  color: error ? t.danger : t.success,
                ),
              ),
              const SizedBox(width: 12),
              Expanded(
                child: Text(message, style: AppText.sm.copyWith(color: t.fg)),
              ),
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
        builder: (ctx) => AppDialog(
          title: Text(title),
          content: Text(message),
          actions: [
            AppButton(
              label: '取消',
              onPressed: () => Navigator.of(ctx).pop(false),
            ),
            AppButton(
              label: action,
              variant: destructive
                  ? ButtonVariant.destructive
                  : ButtonVariant.primary,
              onPressed: () => Navigator.of(ctx).pop(true),
            ),
          ],
        ),
      ),
    ).future;
    return yes == true;
  }

  Future<void> boot() async {
    try {
      final types = api.backends();
      if (types.isNotEmpty) loadBackendProfiles(types);
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
    await loadUsage(); // per-scheme 7-day usage for the detail pane
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
          consentAccess = ModelAccess(const []);
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
      final p = await pairApproveAsync(pairId, consentAccess.allow);
      if (mounted) {
        setState(() {
          consent = null;
          awaitingCode = true;
          pairStartedAt = DateTime.now();
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
    // Every model (incl. disabled adapters), scheme wildcards and unknown
    // entries are listed so that saving keeps authorizations the user did
    // not touch; "*" is the 允许全部模型 switch, not a row.
    final entries = [
      for (final e in policyEntries(adapters, c.allow))
        if (e.id != '*') e,
    ];
    final access = ModelAccess(
      c.allow,
      models: [for (final e in entries) e.id],
    );
    final types = adapterTypes(adapters);
    final ok = await showOverlay<bool>(
      context,
      DialogConfiguration<bool>(
        builder: (ctx) => StatefulBuilder(
          builder: (ctx, setLocal) => AppDialog(
            title: const Text('可用模型'),
            subtitle: Text('${c.title} · 选择这个连接能调用的模型'),
            maxWidth: 480,
            content: ModelAccessEditor(
              access: access,
              models: [for (final e in entries) e.id],
              notes: {for (final e in entries) e.id: e.note},
              labels: {for (final e in entries) e.id: e.label},
              details: {
                for (final e in entries)
                  e.id: modelDetail(e.id, adapters, types, model: e.detail),
              },
              onChanged: () => setLocal(() {}),
            ),
            actions: [
              AppButton(
                label: '取消',
                onPressed: () => Navigator.of(ctx).pop(false),
              ),
              AppButton(
                label: '保存',
                variant: ButtonVariant.primary,
                onPressed: () => Navigator.of(ctx).pop(true),
              ),
            ],
          ),
        ),
      ),
    ).future;
    if (ok != true || !mounted) return;
    try {
      // allow only: the connection's max_concurrency is left unchanged
      await setConnectionPolicyAsync(c.id, access.allow);
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
          footer: connectionFooter,
          maxWidth: connectStep == 3 ? 720 : 760,
          scrollContent: false,
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
        builder: (_) => _DialogRelay(
          host: this,
          title: '用量统计',
          subtitle: '近 7 天 · 本机',
          maxWidth: 560,
          headerActions: () => [
            AppIconButton(
              key: const ValueKey('usage-refresh'),
              icon: LucideIcons.refreshCw,
              tooltip: '刷新用量',
              onPressed: loadUsage,
            ),
          ],
          content: usageCard,
        ),
      ),
    ).future;
  }

  bool _hasScheme(String id) => adapters.any((a) => '${a['id']}' == id);

  /// Total tokens per scheme id, for the scheme details.
  Map<String, int> get usageByScheme => {
    for (final b in usage)
      if (_hasScheme('${b['adapter'] ?? ''}'))
        '${b['adapter'] ?? ''}': ((b['total_tokens'] ?? 0) as num).toInt(),
  };

  Widget usageCard() => UsagePanel(
    rows: [
      for (final b in usage)
        if (_hasScheme('${b['adapter'] ?? ''}'))
          UsageRow(
            _schemeName('${b['adapter'] ?? ''}'),
            ((b['prompt_tokens'] ?? 0) as num).toInt(),
            ((b['completion_tokens'] ?? 0) as num).toInt(),
            ((b['total_tokens'] ?? 0) as num).toInt(),
          ),
    ],
  );

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
      await loadUsage();
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

  /// Step of the connect dialog: 0 address, 1 consent, 2 code, 3 list.
  int get connectStep {
    final paired = status?['paired'] == true;
    return paired && !adding && consent == null && !awaitingCode
        ? 3
        : awaitingCode
        ? 2
        : consent != null
        ? 1
        : 0;
  }

  Widget connectionCard() {
    final recovery = status?['adapter_recovery'] as Map?;
    final step = connectStep;
    return Column(
      mainAxisSize: MainAxisSize.min,
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        if (recovery != null && !recoveryDismissed) ...[
          // Informational, dismissible and shown once: the app works
          // normally with an empty adapter list; the old file is kept.
          AppAlert(
            tone: Tone.brand,
            icon: LucideIcons.info,
            title: '旧的模型方案配置已备份',
            message:
                '本机保存的模型方案配置无法用当前密钥解密（可能来自旧版本应用），'
                '已备份到：${recovery['backup']}\n'
                '应用可以正常使用，请重新添加模型方案即可。',
            trailing: AppButton(
              label: '知道了',
              variant: ButtonVariant.ghost,
              small: true,
              onPressed: dismissRecovery,
            ),
          ),
          const SizedBox(height: 16),
        ],
        // Step 3 manages its own scrolling (fixed header/footer, list
        // scrolls); the pairing steps scroll as a whole.
        if (step == 3)
          Flexible(child: _connectStepPane(step))
        else
          Flexible(
            child: ScrollArea(
              child: Row(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  SizedBox(width: 160, child: _StepRail(current: step)),
                  const SizedBox(width: AppSpace.s24),
                  Expanded(child: _connectStepPane(step)),
                ],
              ),
            ),
          ),
      ],
    );
  }

  /// Footer bar of the connect dialog for the current step.
  Widget connectionFooter() {
    switch (connectStep) {
      case 1:
        return Row(
          children: [
            const Spacer(),
            AppButton(label: '拒绝', onPressed: busy ? null : denyConsent),
            const SizedBox(width: 8),
            AppButton(
              label: busy ? '处理中…' : '允许',
              variant: ButtonVariant.primary,
              onPressed: busy ? null : approveConsent,
            ),
          ],
        );
      case 2:
        return Row(
          children: [
            AppButton(
              label: '返回',
              icon: LucideIcons.chevronLeft,
              variant: ButtonVariant.ghost,
              onPressed: busy ? null : cancelPairing,
            ),
            const Spacer(),
            AppButton(label: '取消配对', onPressed: busy ? null : cancelPairing),
          ],
        );
      case 3:
        return ConnectionsPanel.footer(status);
      default:
        return Row(
          children: [
            if (adding)
              AppButton(
                label: '返回连接列表',
                icon: LucideIcons.chevronLeft,
                variant: ButtonVariant.ghost,
                onPressed: busy ? null : () => setState(() => adding = false),
              ),
            const Spacer(),
            AppButton(
              label: '连接',
              icon: LucideIcons.link,
              variant: ButtonVariant.primary,
              onPressed: busy ? null : prepareConnection,
            ),
          ],
        );
    }
  }

  /// Opens a URL with the platform handler (pairing confirmation page,
  /// data directory).
  Future<void> openExternal(String target) async {
    try {
      if (Platform.isMacOS) {
        await Process.run('open', [target]);
      } else if (Platform.isWindows) {
        await Process.run('explorer.exe', [target]);
      } else {
        await Process.run('xdg-open', [target]);
      }
    } catch (e) {
      show('无法打开：$e', error: true);
    }
  }

  Widget _connectStepPane(int step) {
    final t = AppTokens.of(context);
    switch (step) {
      case 1: // 身份验证：consent — the SaaS identity was verified by signature
        final saas =
            (consent?['saas'] as Map?)?.cast<String, dynamic>() ?? const {};
        final saasName = '${saas['name'] ?? ''}'.trim();
        final verified = consent?['verified'] == true;
        final types = adapterTypes(adapters);
        final concrete = consentModels;
        return Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            _StepHeader(
              icon: LucideIcons.shieldCheck,
              title: saasName.isEmpty ? '确认服务授权' : '允许 $saasName 连接',
              desc: '请核对服务信息，并选择它能调用的模型',
            ),
            const SizedBox(height: 16),
            _ConsentView(consent: consent!, verified: verified),
            const SizedBox(height: 16),
            Text(
              '可调用的模型',
              style: AppText.sm.copyWith(
                color: t.fg,
                fontWeight: AppText.semibold,
              ),
            ),
            const SizedBox(height: 8),
            ModelAccessEditor(
              access: consentAccess,
              models: concrete,
              labels: schemeLabels(adapters),
              details: {
                for (final m in concrete)
                  m: adapterTypeLabel(types[m.split('/').first] ?? ''),
              },
              onChanged: () => setState(() {}),
            ),
          ],
        );
      case 2: // 确认授权：show the pairing code for the remote page
        final left = pairStartedAt == null
            ? const Duration(minutes: 10)
            : const Duration(minutes: 10) -
                  DateTime.now().difference(pairStartedAt!);
        final expired = left.isNegative || left == Duration.zero;
        final mm = left.inMinutes.clamp(0, 99).toString().padLeft(2, '0');
        final ss = (left.inSeconds % 60)
            .clamp(0, 59)
            .toString()
            .padLeft(2, '0');
        return Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            const _StepHeader(
              icon: LucideIcons.keyRound,
              title: '输入配对码',
              desc: '在远端服务页面输入此配对码完成连接',
            ),
            const SizedBox(height: 16),
            AppCard(
              subtle: true,
              padding: const EdgeInsets.all(AppSpace.s24),
              child: Column(
                children: [
                  Row(
                    mainAxisSize: MainAxisSize.min,
                    children: [
                      Text(
                        pairUserCode ?? '',
                        key: const ValueKey('pair-code'),
                        style: monoStyle(
                          AppText.sm.copyWith(
                            fontSize: 32,
                            height: 40 / 32,
                            fontWeight: FontWeight.w700,
                            letterSpacing: 6,
                            color: t.fg,
                          ),
                        ),
                      ),
                      const SizedBox(width: 12),
                      AppIconButton(
                        icon: LucideIcons.copy,
                        outline: true,
                        tooltip: '复制配对码',
                        onPressed: () {
                          Clipboard.setData(
                            ClipboardData(text: pairUserCode ?? ''),
                          );
                          show('配对码已复制');
                        },
                      ),
                    ],
                  ),
                  const SizedBox(height: 12),
                  if (expired)
                    Text(
                      '配对码已过期，请返回重新开始配对',
                      style: AppText.sm.copyWith(color: t.danger),
                    )
                  else
                    Row(
                      mainAxisSize: MainAxisSize.min,
                      children: [
                        const SpinnerArc(),
                        const SizedBox(width: 8),
                        Text(
                          '等待远端确认… ',
                          style: AppText.sm.copyWith(color: t.fg2),
                        ),
                        Text(
                          '$mm:$ss',
                          style: monoStyle(AppText.sm.copyWith(color: t.fg)),
                        ),
                        Text(' 后过期', style: AppText.sm.copyWith(color: t.fg2)),
                      ],
                    ),
                ],
              ),
            ),
            if ((pairLinkUrl ?? '').isNotEmpty) ...[
              const SizedBox(height: 16),
              Center(
                child: AppButton(
                  label: '在浏览器中打开确认页 ›',
                  onPressed: () => openExternal(pairLinkUrl!),
                ),
              ),
            ],
          ],
        );
      case 3: // 连接列表：every paired SaaS
        return ConnectionsPanel(
          status: status,
          adapters: adapters,
          busy: busy,
          onRefresh: () => refresh(),
          onAdd: () => setState(() {
            adding = true;
            remoteUrl.clear();
          }),
          onToggle: (c) => connectionAction(
            c,
            c.paused ? 'connection_connect' : 'connection_disconnect',
          ),
          onRevoke: (c) => connectionAction(c, 'connection_revoke'),
          onModels: editPolicy,
        );
      default: // 输入地址
        return Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            const _StepHeader(
              icon: LucideIcons.link,
              title: '连接远端服务',
              desc: '粘贴 SaaS 提供的服务地址或配对链接',
            ),
            const SizedBox(height: 16),
            Text(
              '服务地址',
              style: AppText.sm.copyWith(
                color: t.fg,
                fontWeight: AppText.medium,
              ),
            ),
            const SizedBox(height: 6),
            appTextField(
              context,
              controller: remoteUrl,
              placeholder: '输入服务地址（https://…）或配对链接（lmbp://pair?…）',
              clear: true,
              onSubmitted: (_) {
                if (!busy) prepareConnection();
              },
            ),
            const SizedBox(height: 4),
            if ((status?['pairing_error'] ?? '') != '')
              Text(
                '${status?['pairing_error']}',
                style: AppText.xs.copyWith(color: t.danger),
              )
            else
              Text(
                '支持 https://… 或 lmbp://pair?… 两种格式。本机只会主动外连，无需开放端口。',
                style: AppText.xs.copyWith(color: t.fg3),
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
    final t = AppTokens.of(context);
    return Scaffold(
      backgroundColor: t.bg,
      loadingProgressIndeterminate: busy,
      child: AppShell(
        status: status,
        child: kernelReady
            ? ProfileWorkspace(
                api: api,
                adapters: adapters,
                status: status,
                usage: usageByScheme,
                onConnectionTap: openConnection,
                onUsageTap: openUsage,
                onChanged: () async {
                  await refresh(force: true);
                  await loadUsage();
                },
              )
            : KernelErrorPage(
                error: kernelError ?? '内核启动失败',
                onRetry: boot,
                onOpenDataDir: () => openExternal(dataDirPath()),
              ),
      ),
    );
  }
}

/// The kernel's default data directory (os.UserConfigDir()/avora).
String dataDirPath() {
  final env = Platform.environment;
  if (Platform.isWindows) return '${env['APPDATA'] ?? ''}\\avora';
  final home = env['HOME'] ?? '';
  if (Platform.isMacOS) {
    return '$home/Library/Application Support/avora';
  }
  return '${env['XDG_CONFIG_HOME'] ?? '$home/.config'}/avora';
}

/// Shared outer layout: kernel-wide warnings stay visible on every page
/// (e.g. after the last connection is revoked and the pairing page shows).
class AppShell extends StatefulWidget {
  const AppShell({super.key, required this.status, required this.child});

  final Map<String, dynamic>? status;
  final Widget child;

  @override
  State<AppShell> createState() => _AppShellState();
}

class _AppShellState extends State<AppShell> {
  /// The warning text the user closed; a different warning shows again.
  String? dismissed;

  Future<void> details(String warning) async {
    await showOverlay<void>(
      context,
      DialogConfiguration<void>(
        builder: (ctx) => AppDialog(
          title: const Text('本机私钥清理未完成'),
          maxWidth: 520,
          content: Column(
            mainAxisSize: MainAxisSize.min,
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              const Text('已撤销连接的部分私钥文件暂时无法删除，应用会在每次启动时重试。'),
              const SizedBox(height: 12),
              Container(
                padding: const EdgeInsets.all(12),
                decoration: BoxDecoration(
                  color: AppTokens.of(ctx).mutedBg,
                  borderRadius: BorderRadius.circular(AppRadius.control),
                ),
                child: SelectableText(
                  warning,
                  style: monoStyle(
                    AppText.xs.copyWith(
                      color: AppTokens.of(ctx).fg2,
                      height: 18 / 12,
                    ),
                  ),
                ),
              ),
            ],
          ),
          actions: [
            AppButton(
              label: '知道了',
              variant: ButtonVariant.primary,
              onPressed: () => Navigator.of(ctx).pop(),
            ),
          ],
        ),
      ),
    ).future;
  }

  @override
  Widget build(BuildContext context) {
    // The tree shape never changes (only the bar's content does), so the
    // page below keeps its state (e.g. unsaved drafts) when a warning shows.
    final ds = DesktopStatus.parse(widget.status);
    final warning = ds.cleanupWarning;
    final t = AppTokens.of(context);
    final n = ds.cleanupPending;
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        if (warning == null || warning == dismissed)
          const SizedBox.shrink(key: ValueKey('cleanup-warning-slot'))
        else
          Container(
            key: const ValueKey('cleanup-warning'),
            height: AppSize.banner,
            padding: const EdgeInsets.fromLTRB(24, 0, 16, 0),
            decoration: BoxDecoration(
              color: t.danger50,
              border: Border(bottom: BorderSide(color: t.danger200)),
            ),
            child: Row(
              children: [
                Icon(LucideIcons.triangleAlert, size: 16, color: t.dangerFg),
                const SizedBox(width: 8),
                Flexible(
                  child: Text(
                    n > 0
                        ? '有 $n 个已撤销连接的本机私钥未能删除，将在下次启动时重试。'
                        : '部分已撤销连接的本机私钥未能删除，将在下次启动时重试。',
                    maxLines: 1,
                    overflow: TextOverflow.ellipsis,
                    style: AppText.sm.copyWith(color: t.dangerFg),
                  ),
                ),
                const SizedBox(width: 8),
                GestureDetector(
                  key: const ValueKey('cleanup-warning-details'),
                  onTap: () => details(warning),
                  child: MouseRegion(
                    cursor: SystemMouseCursors.click,
                    child: Text(
                      '查看详情',
                      style: AppText.sm.copyWith(
                        color: t.dangerFg,
                        fontWeight: AppText.medium,
                        decoration: TextDecoration.underline,
                        decorationColor: t.dangerFg,
                      ),
                    ),
                  ),
                ),
                const Spacer(),
                AppIconButton(
                  key: const ValueKey('cleanup-warning-close'),
                  icon: LucideIcons.x,
                  size: 28,
                  color: t.dangerFg,
                  tooltip: '关闭',
                  onPressed: () => setState(() => dismissed = warning),
                ),
              ],
            ),
          ),
        Expanded(key: const ValueKey('app-shell-body'), child: widget.child),
      ],
    );
  }
}

/// Full-window page shown when the native kernel failed to start.
class KernelErrorPage extends StatefulWidget {
  const KernelErrorPage({
    super.key,
    required this.error,
    required this.onRetry,
    required this.onOpenDataDir,
  });
  final String error;
  final VoidCallback onRetry;
  final VoidCallback onOpenDataDir;

  @override
  State<KernelErrorPage> createState() => _KernelErrorPageState();
}

class _KernelErrorPageState extends State<KernelErrorPage> {
  bool open = true;

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    final raw = cleanError(widget.error);
    final hint = kernelErrorHint(raw);
    return ColoredBox(
      color: t.subtle,
      child: Center(
        child: SingleChildScrollView(
          padding: const EdgeInsets.all(AppSpace.s24),
          child: Container(
            key: const ValueKey('kernel-error'),
            width: 520,
            padding: const EdgeInsets.all(AppSpace.s32),
            decoration: BoxDecoration(
              color: t.surface,
              borderRadius: BorderRadius.circular(AppRadius.card),
              border: Border.all(color: t.border),
            ),
            child: Column(
              mainAxisSize: MainAxisSize.min,
              crossAxisAlignment: CrossAxisAlignment.stretch,
              children: [
                Row(
                  children: [
                    const IconTile(
                      LucideIcons.triangleAlert,
                      tone: Tone.danger,
                    ),
                    const SizedBox(width: 16),
                    Expanded(
                      child: Column(
                        crossAxisAlignment: CrossAxisAlignment.start,
                        children: [
                          Text(
                            '无法启动本机服务',
                            style: AppText.xl.copyWith(color: t.fg),
                          ),
                          Text(
                            'Avora 的本机内核没有启动成功',
                            style: AppText.sm.copyWith(color: t.fg2),
                          ),
                        ],
                      ),
                    ),
                  ],
                ),
                const SizedBox(height: 16),
                if (hint != null) ...[
                  AppAlert(
                    tone: Tone.warning,
                    title: hint.title,
                    message: hint.message,
                  ),
                  const SizedBox(height: 16),
                ],
                Row(
                  children: [
                    GestureDetector(
                      onTap: () => setState(() => open = !open),
                      child: MouseRegion(
                        cursor: SystemMouseCursors.click,
                        child: Row(
                          children: [
                            Icon(
                              open
                                  ? LucideIcons.chevronDown
                                  : LucideIcons.chevronRight,
                              size: 16,
                              color: t.fg,
                            ),
                            const SizedBox(width: 6),
                            Text(
                              '错误详情',
                              style: AppText.sm.copyWith(
                                color: t.fg,
                                fontWeight: AppText.medium,
                              ),
                            ),
                          ],
                        ),
                      ),
                    ),
                    const Spacer(),
                    AppButton(
                      label: '复制',
                      icon: LucideIcons.copy,
                      variant: ButtonVariant.ghost,
                      small: true,
                      onPressed: () =>
                          Clipboard.setData(ClipboardData(text: raw)),
                    ),
                  ],
                ),
                if (open) ...[
                  const SizedBox(height: 8),
                  Container(
                    padding: const EdgeInsets.all(12),
                    decoration: BoxDecoration(
                      color: t.mutedBg,
                      borderRadius: BorderRadius.circular(AppRadius.control),
                    ),
                    child: SelectableText(
                      raw,
                      key: const ValueKey('kernel-error-detail'),
                      style: monoStyle(
                        AppText.xs.copyWith(color: t.fg2, height: 18 / 12),
                      ),
                    ),
                  ),
                ],
                const SizedBox(height: AppSpace.s24),
                Row(
                  mainAxisAlignment: MainAxisAlignment.end,
                  children: [
                    AppButton(
                      label: '打开数据目录',
                      icon: LucideIcons.folder,
                      onPressed: widget.onOpenDataDir,
                    ),
                    const SizedBox(width: 8),
                    AppButton(
                      label: '重试',
                      icon: LucideIcons.refreshCw,
                      variant: ButtonVariant.primary,
                      onPressed: widget.onRetry,
                    ),
                  ],
                ),
              ],
            ),
          ),
        ),
      ),
    );
  }
}

/// Numbered rail for the connect dialog's 4 steps (24px circles, 56px per
/// step, 2px connector that turns brand once the step is done).
class _StepRail extends StatelessWidget {
  const _StepRail({required this.current});

  final int current;
  static const _labels = ['输入地址', '身份验证', '确认授权', '完成连接'];

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        for (var i = 0; i < _labels.length; i++)
          SizedBox(
            height: 56,
            child: Stack(
              children: [
                if (i < _labels.length - 1)
                  Positioned(
                    left: 11,
                    top: 28,
                    bottom: 4,
                    child: Container(
                      width: 2,
                      color: i < current ? t.brand : t.border,
                    ),
                  ),
                Row(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    Container(
                      width: 24,
                      height: 24,
                      alignment: Alignment.center,
                      decoration: BoxDecoration(
                        shape: BoxShape.circle,
                        color: i < current ? t.brand : t.surface,
                        border: Border.all(
                          width: 2,
                          color: i <= current ? t.brand : t.border,
                        ),
                      ),
                      child: i < current
                          ? Icon(LucideIcons.check, size: 12, color: t.brandFg)
                          : Text(
                              '${i + 1}',
                              style: AppText.xs.copyWith(
                                height: 1,
                                fontWeight: AppText.semibold,
                                color: i == current ? t.brand : t.fg3,
                              ),
                            ),
                    ),
                    const SizedBox(width: 12),
                    SizedBox(
                      height: 24,
                      child: Center(
                        child: Text(
                          _labels[i],
                          style: AppText.sm.copyWith(
                            color: i == current
                                ? t.fg
                                : i < current
                                ? t.fg2
                                : t.fg3,
                            fontWeight: i == current
                                ? AppText.semibold
                                : AppText.regular,
                          ),
                        ),
                      ),
                    ),
                  ],
                ),
              ],
            ),
          ),
      ],
    );
  }
}

Tone summaryTone(ConnSummaryKind k) => switch (k) {
  ConnSummaryKind.online => Tone.success,
  ConnSummaryKind.partial || ConnSummaryKind.connecting => Tone.warning,
  ConnSummaryKind.error || ConnSummaryKind.revoked => Tone.danger,
  _ => Tone.neutral,
};

/// Connection dialog header: colour and wording follow the actual
/// per-connection state (same source as the status bar), not "paired".
class ConnectionSummaryHeader extends StatelessWidget {
  const ConnectionSummaryHeader({super.key, required this.status});
  final DesktopStatus status;

  @override
  Widget build(BuildContext context) {
    final sum = connectionSummary(status);
    final errors = status.stale
        ? 0
        : status.connections.where((c) => c.failed).length;
    final tone = summaryTone(sum.kind);
    return Align(
      alignment: Alignment.centerLeft,
      child: StatusBadge(
        errors > 0 ? '${sum.title} · $errors 个错误' : sum.title,
        key: ValueKey('summary-${sum.kind.name}'),
        tone: tone,
        height: 24,
        icon: switch (sum.kind) {
          ConnSummaryKind.online => LucideIcons.circleCheck,
          ConnSummaryKind.partial => LucideIcons.triangleAlert,
          ConnSummaryKind.connecting => LucideIcons.loader,
          ConnSummaryKind.error => LucideIcons.triangleAlert,
          ConnSummaryKind.revoked => LucideIcons.ban,
          _ => LucideIcons.unplug,
        },
      ),
    );
  }
}

/// Icon tile + title (+ description) for each step.
class _StepHeader extends StatelessWidget {
  const _StepHeader({required this.icon, required this.title, this.desc});

  final IconData icon;
  final String title;
  final String? desc;

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    return Row(
      children: [
        IconTile(icon),
        const SizedBox(width: 12),
        Expanded(
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text(title, style: AppText.lg.copyWith(color: t.fg)),
              if (desc != null)
                Text(desc!, style: AppText.xs.copyWith(color: t.fg2)),
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
    this.subtitle,
    this.footer,
    this.headerActions,
    this.maxWidth = 420,
    this.scrollContent = true,
  });

  final _HomePageState host;
  final String title;
  final String? subtitle;
  final Widget Function() content;
  final Widget Function()? footer;
  final List<Widget> Function()? headerActions;
  final double maxWidth;
  final bool scrollContent;

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
  Widget build(BuildContext context) => DialogFrame(
    title: widget.title,
    subtitle: widget.subtitle,
    maxWidth: widget.maxWidth,
    scrollContent: widget.scrollContent,
    footer: widget.footer?.call(),
    headerActions: widget.headerActions?.call(),
    child: widget.content(),
  );
}

/// Dialog chrome shared by the overlay dialogs.
class DialogFrame extends StatelessWidget {
  const DialogFrame({
    super.key,
    required this.title,
    required this.child,
    this.subtitle,
    this.footer,
    this.headerActions,
    this.maxWidth = 420,
    this.scrollContent = true,
  });

  final String title;
  final String? subtitle;
  final Widget child;
  final Widget? footer;
  final List<Widget>? headerActions;
  final double maxWidth;
  final bool scrollContent;

  @override
  Widget build(BuildContext context) => AppDialog(
    title: Text(title),
    subtitle: subtitle == null ? null : Text(subtitle!),
    maxWidth: maxWidth,
    scrollContent: scrollContent,
    footer: footer,
    headerActions: headerActions,
    content: child,
  );
}

/// 连接列表 page of the connection dialog: summary badge, one card per
/// paired SaaS (scrolls), device/task footer in the dialog footer.
class ConnectionsPanel extends StatelessWidget {
  const ConnectionsPanel({
    super.key,
    required this.status,
    required this.adapters,
    required this.busy,
    required this.onRefresh,
    required this.onAdd,
    required this.onToggle,
    required this.onRevoke,
    required this.onModels,
  });

  final Map<String, dynamic>? status;
  final List<Map<String, dynamic>> adapters;
  final bool busy;
  final VoidCallback onRefresh;
  final VoidCallback onAdd;
  final void Function(ConnectionInfo) onToggle;
  final void Function(ConnectionInfo) onRevoke;
  final void Function(ConnectionInfo) onModels;

  /// Device and running tasks, shown in the dialog footer.
  static Widget footer(Map<String, dynamic>? status) => Builder(
    builder: (context) {
      final t = AppTokens.of(context);
      final ds = DesktopStatus.parse(status);
      final style = AppText.sm.copyWith(color: t.fg2);
      return Row(
        key: const ValueKey('connection-footer'),
        children: [
          Icon(LucideIcons.monitor, size: 16, color: t.fg2),
          const SizedBox(width: 6),
          Flexible(
            child: Text(
              '${status?['device_name'] ?? status?['device_id'] ?? ''}',
              maxLines: 1,
              overflow: TextOverflow.ellipsis,
              style: style,
            ),
          ),
          const SizedBox(width: 16),
          Icon(LucideIcons.list, size: 16, color: t.fg2),
          const SizedBox(width: 6),
          Text(
            '${ds.stale ? '—' : status?['running_tasks'] ?? 0} 个任务',
            style: style,
          ),
        ],
      );
    },
  );

  @override
  Widget build(BuildContext context) {
    final ds = DesktopStatus.parse(status);
    return Column(
      mainAxisSize: MainAxisSize.min,
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        Row(
          children: [
            Expanded(child: ConnectionSummaryHeader(status: ds)),
            AppIconButton(
              icon: LucideIcons.refreshCw,
              outline: true,
              tooltip: '刷新',
              onPressed: busy ? null : onRefresh,
            ),
            const SizedBox(width: 8),
            AppButton(
              label: '添加连接',
              icon: LucideIcons.plus,
              onPressed: busy ? null : onAdd,
            ),
          ],
        ),
        const SizedBox(height: 16),
        // Only the list scrolls; summary/actions above and device/task
        // below stay visible however many SaaS are paired.
        Flexible(
          child: ScrollArea(
            scrollKey: const ValueKey('connection-list'),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.stretch,
              children: [
                for (final c in ds.connections)
                  Padding(
                    padding: const EdgeInsets.only(bottom: 8),
                    child: _ConnectionTile(
                      key: ValueKey('conn-${c.id}'),
                      adapters: adapters,
                      c: c,
                      busy: busy,
                      onToggle: () => onToggle(c),
                      onRevoke: () => onRevoke(c),
                      onModels: () => onModels(c),
                    ),
                  ),
              ],
            ),
          ),
        ),
      ],
    );
  }
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
    final t = AppTokens.of(context);
    return SizedBox(
      height: 32,
      child: Row(
        children: [
          SizedBox(
            width: 72,
            child: Text(label, style: AppText.sm.copyWith(color: t.fg2)),
          ),
          Expanded(
            child: Text(
              value,
              maxLines: 1,
              overflow: TextOverflow.ellipsis,
              style: AppText.sm.copyWith(color: t.fg),
            ),
          ),
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
    final profile = backendForType(type);
    final icon = profile.isCLI
        ? LucideIcons.terminal
        : profile.cloud || !profile.known
        ? LucideIcons.cloud
        : LucideIcons.monitor;
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

String adapterTypeShort(String? type) => backendForType(type).short;
String adapterTypeLabel(String type) => backendForType(type).label;

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
    text:
        widget.existing?['executable'] ??
        widget.existing?['codex_bin'] ??
        backendForType(
          widget.existing?['type'] ?? 'codex_app_server',
        ).executable ??
        '',
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
        'skip_model_probe': backendForType(type).needsApiKey && skipModelProbe,
      };
      if (type == 'codex_app_server') {
        body['codex_bin'] = codex.text.trim();
      } else if (backendForType(type).isCLI) {
        body['executable'] = codex.text.trim();
      } else {
        body['base_url'] = url.text.trim();
      }
      if (backendForType(type).needsApiKey) {
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
    // While saving, the dialog cannot be dismissed (X, Esc or barrier):
    // the save would finish unseen and the list would not refresh.
    return PopScope(
      canPop: !saving,
      child: AppDialog(
        closeEnabled: !saving,
        title: Text(creating ? '添加适配器' : '编辑适配器'),
        maxWidth: 540,
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
                              setState(() {
                                type = value;
                                codex.text =
                                    backendForType(value).executable ?? '';
                              });
                            }
                          : null,
                      popup: (context) => SelectPopup(
                        items: SelectItemList(
                          children: [
                            for (final profile in backendProfiles)
                              SelectItemButton(
                                value: profile.type,
                                child: Text(profile.label),
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
                  if (backendForType(type).isCLI)
                    LabeledField(
                      label: '${backendForType(type).short} CLI 路径',
                      child: TextField(
                        controller: codex,
                        placeholder: Text(
                          backendForType(type).executable ?? '',
                        ),
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
                  if (backendForType(type).needsApiKey) ...[
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
      ),
    );
  }
}

/// Consent screen: the SaaS identity verified from the gateway's signed
/// metadata (docs/lmbp-spec.md §19). Nothing here comes from the link.
class _ConsentView extends StatelessWidget {
  const _ConsentView({required this.consent, this.verified = false});
  final Map<String, dynamic> consent;
  final bool verified;

  static String host(String uri) => Uri.tryParse(uri)?.host ?? '';

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    final saas = (consent['saas'] as Map?)?.cast<String, dynamic>() ?? {};
    final name = '${saas['name'] ?? ''}'.trim();
    final scopes = ((consent['requested_scopes'] as List?) ?? []).join('、');
    final gateway = '${consent['gateway'] ?? ''}';
    final homepage = '${saas['homepage_uri'] ?? ''}';
    final policy = '${saas['policy_uri'] ?? ''}';
    final site = host(homepage).isNotEmpty ? host(homepage) : host(gateway);
    return AppCard(
      key: const ValueKey('consent-saas'),
      subtle: true,
      padding: const EdgeInsets.fromLTRB(16, 12, 16, 4),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          Row(
            children: [
              Container(
                width: 36,
                height: 36,
                alignment: Alignment.center,
                decoration: BoxDecoration(
                  color: t.brand,
                  borderRadius: BorderRadius.circular(AppRadius.control),
                ),
                child: Text(
                  name.isEmpty ? '?' : name.characters.first.toUpperCase(),
                  style: AppText.md.copyWith(
                    color: t.brandFg,
                    fontWeight: AppText.semibold,
                  ),
                ),
              ),
              const SizedBox(width: 12),
              Expanded(
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    Text(
                      name.isEmpty ? '未命名服务' : name,
                      maxLines: 1,
                      overflow: TextOverflow.ellipsis,
                      style: AppText.md.copyWith(
                        color: t.fg,
                        fontWeight: AppText.semibold,
                      ),
                    ),
                    if (site.isNotEmpty)
                      Text(site, style: AppText.xs.copyWith(color: t.fg2)),
                  ],
                ),
              ),
              const SizedBox(width: 12),
              verified
                  ? const StatusBadge(
                      '签名已校验',
                      tone: Tone.success,
                      icon: LucideIcons.check,
                    )
                  : const StatusBadge('身份未校验', tone: Tone.warning),
            ],
          ),
          const SizedBox(height: 10),
          const Hairline(),
          const SizedBox(height: 4),
          // docs/lmbp-spec.md §19: gateway, tenant, homepage, privacy
          // policy, scopes and gateway key must be shown before approval.
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

/// Adapter id -> type, for type marks next to model ids.
Map<String, String> adapterTypes(List<Map<String, dynamic>> adapters) => {
  for (final a in adapters) '${a['id']}': '${a['type'] ?? ''}',
};

/// Secondary line of a model row: provider type, availability, model.
String modelDetail(
  String id,
  List<Map<String, dynamic>> adapters,
  Map<String, String> types, {
  String model = '',
}) {
  if (id == '*' || id.endsWith('/*')) return '';
  final aid = id.split('/').first;
  final a = adapters.where((e) => '${e['id']}' == aid).firstOrNull;
  if (a == null) return model;
  final parts = [adapterTypeLabel(types[aid] ?? '')];
  if (a['enabled'] == true &&
      (a['health'] == 'unavailable' || a['health'] == 'storage_full')) {
    parts.add('当前不可用');
  }
  return parts.join(' · ');
}

/// Checkbox list of local models ("adapter/model") for one SaaS: counter,
/// 全选 / 清空, one 44px row per model with label, detail and note badge.
class ModelChecklist extends StatelessWidget {
  const ModelChecklist({
    super.key,
    required this.models,
    required this.selected,
    required this.onChanged,
    this.notes = const {},
    this.labels = const {},
    this.details = const {},
    this.types = const {},
    this.lockedBy,
    this.onAll,
    this.title,
    this.inlineDetail = false,
    this.allLocked = false,
  });

  /// 允许全部模型 is on: every row checked, faded and read-only, 全选 /
  /// 清空 disabled, and an 已锁定 tag next to the counter.
  final bool allLocked;

  /// Display only: scheme name per stored id (falls back to the id).
  final Map<String, String> labels;

  /// Display only: small secondary text per id (e.g. the provider type).
  final Map<String, String> details;

  /// Optional adapter type per id: shows the provider mark.
  final Map<String, String> types;

  final List<String> models;
  final Set<String> selected;
  final void Function(String model, bool value) onChanged;

  /// Optional hint per model (e.g. 方案已停用), shown as a badge.
  final Map<String, String> notes;

  /// Models already granted by a selected wildcard: shown checked, read-only.
  final bool Function(String model)? lockedBy;

  /// 全选 (true) / 清空 (false); hidden when null.
  final void Function(bool all)? onAll;

  /// Optional heading left of the counter.
  final String? title;

  /// Detail after the label on one line instead of below it.
  final bool inlineDetail;

  static String badgeText(String note) =>
      note.startsWith('方案已停用') ? note.replaceFirst('方案已停用', '已停用') : note;

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    if (models.isEmpty) {
      return Text(
        '还没有启用的模型方案，可稍后添加方案后在连接列表中授权。',
        style: AppText.sm.copyWith(color: t.fg2),
      );
    }
    bool isOn(String m) =>
        allLocked || (lockedBy?.call(m) ?? false) || selected.contains(m);
    final count = models.where(isOn).length;
    final all = count == models.length;
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        Row(
          children: [
            Flexible(
              fit: allLocked ? FlexFit.loose : FlexFit.tight,
              child: Text.rich(
                TextSpan(
                  children: [
                    if (title != null)
                      TextSpan(
                        text: '$title ',
                        style: TextStyle(
                          color: t.fg,
                          fontWeight: AppText.semibold,
                        ),
                      ),
                    TextSpan(
                      text: title != null
                          ? '· 已选 $count / ${models.length}'
                          : '已选 $count / ${models.length}',
                    ),
                  ],
                ),
                key: const ValueKey('model-count'),
                style: AppText.sm.copyWith(color: t.fg2),
              ),
            ),
            if (allLocked) ...[
              const SizedBox(width: 8),
              const StatusBadge(
                '已锁定',
                key: ValueKey('model-locked'),
                tone: Tone.brand,
                icon: LucideIcons.lock,
              ),
            ],
            if (allLocked) const Spacer(),
            if (onAll != null) ...[
              AppButton(
                label: '全选',
                variant: ButtonVariant.ghost,
                small: true,
                onPressed: all || allLocked ? null : () => onAll!(true),
              ),
              AppButton(
                label: '清空',
                variant: ButtonVariant.ghost,
                small: true,
                onPressed: count == 0 || allLocked ? null : () => onAll!(false),
              ),
            ],
          ],
        ),
        const SizedBox(height: 4),
        Opacity(
          opacity: allLocked ? 0.5 : 1,
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [for (final m in models) _row(context, t, m)],
          ),
        ),
      ],
    );
  }

  Widget _row(BuildContext context, AppTokens t, String m) {
    final locked = allLocked || (lockedBy?.call(m) ?? false);
    final note = allLocked
        ? ''
        : locked
        ? '已由通配授权'
        : (notes[m] ?? '');
    final wildcard = m == '*' || m.endsWith('/*');
    final label = (labels[m] ?? '').isEmpty ? m : labels[m]!;
    var detail = details[m] ?? '';
    if (detail == label) detail = '';
    // long wildcard explanations read better as the detail line
    final badge = wildcard ? '' : note;
    if (wildcard && note.isNotEmpty) detail = note;
    final type = types[m] ?? '';
    final on = locked || selected.contains(m);
    final main = Text(
      label,
      maxLines: 1,
      overflow: TextOverflow.ellipsis,
      style: AppText.sm.copyWith(color: t.fg, fontWeight: AppText.semibold),
    );
    return GestureDetector(
      behavior: HitTestBehavior.opaque,
      onTap: locked ? null : () => onChanged(m, !on),
      child: Container(
        key: ValueKey('model-$m'),
        constraints: const BoxConstraints(minHeight: 44),
        padding: const EdgeInsets.symmetric(vertical: 8),
        decoration: BoxDecoration(
          border: Border(bottom: BorderSide(color: t.border)),
        ),
        child: Row(
          children: [
            AppCheckbox(
              checked: on,
              onChanged: locked ? null : (v) => onChanged(m, v),
            ),
            const SizedBox(width: 12),
            if (type.isNotEmpty) ...[TypeMark(type), const SizedBox(width: 12)],
            Expanded(
              child: inlineDetail
                  ? Row(
                      children: [
                        Flexible(child: main),
                        if (detail.isNotEmpty)
                          Text(
                            ' · $detail',
                            style: AppText.xs.copyWith(color: t.fg2),
                          ),
                      ],
                    )
                  : Column(
                      crossAxisAlignment: CrossAxisAlignment.start,
                      children: [
                        main,
                        if (detail.isNotEmpty)
                          Text(
                            detail,
                            style: AppText.xs.copyWith(color: t.fg2),
                          ),
                      ],
                    ),
            ),
            if (badge.isNotEmpty) ...[
              const SizedBox(width: 8),
              StatusBadge(badgeText(badge)),
            ],
          ],
        ),
      ),
    );
  }
}

/// Model access for one SaaS (可用模型 dialog and the pairing consent
/// page): the 允许全部模型 switch ("*") above the flat model checklist.
class ModelAccessEditor extends StatelessWidget {
  const ModelAccessEditor({
    super.key,
    required this.access,
    required this.models,
    required this.onChanged,
    this.labels = const {},
    this.details = const {},
    this.notes = const {},
    this.types = const {},
    this.inlineDetail = false,
  });

  final ModelAccess access;

  /// Rows: concrete models plus kept "adapter/*" and unknown entries
  /// (never "*", which the switch represents).
  final List<String> models;

  /// Called after [access] changed, to rebuild.
  final VoidCallback onChanged;
  final Map<String, String> labels, details, notes, types;
  final bool inlineDetail;

  @override
  Widget build(BuildContext context) {
    final sel = access.selected;
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        AllModelsSwitch(
          value: access.all,
          onChanged: (v) {
            access.all = v;
            onChanged();
          },
        ),
        const SizedBox(height: 12),
        ModelChecklist(
          models: models,
          labels: labels,
          details: details,
          notes: notes,
          types: types,
          inlineDetail: inlineDetail,
          selected: sel,
          allLocked: access.all,
          lockedBy: (m) => coveredByWildcard(m, sel),
          onChanged: (m, v) {
            v ? sel.add(m) : sel.remove(m);
            onChanged();
          },
          onAll: (v) {
            if (v) {
              sel.addAll([
                for (final m in models)
                  if (!m.endsWith('/*')) m,
              ]);
            } else {
              sel.clear();
            }
            onChanged();
          },
        ),
      ],
    );
  }
}

/// The 允许全部模型 row: icon, title (+ "*" tag when on) and a switch.
/// Light brand tint when on; no description text.
class AllModelsSwitch extends StatelessWidget {
  const AllModelsSwitch({super.key, required this.value, this.onChanged});
  final bool value;
  final ValueChanged<bool>? onChanged;

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    return GestureDetector(
      key: const ValueKey('allow-all'),
      behavior: HitTestBehavior.opaque,
      onTap: onChanged == null ? null : () => onChanged!(!value),
      child: Container(
        padding: const EdgeInsets.symmetric(horizontal: 14, vertical: 12),
        decoration: BoxDecoration(
          color: value ? t.brand50 : t.subtle,
          borderRadius: BorderRadius.circular(AppRadius.card),
          border: Border.all(color: value ? t.brand100 : t.border),
        ),
        child: Row(
          children: [
            Container(
              width: 32,
              height: 32,
              alignment: Alignment.center,
              decoration: BoxDecoration(
                color: value ? t.brand : t.brand50,
                borderRadius: BorderRadius.circular(AppRadius.control),
              ),
              child: Icon(
                LucideIcons.grid2x2Plus,
                size: 16,
                color: value ? t.brandFg : t.brand,
              ),
            ),
            const SizedBox(width: 12),
            Text(
              '允许全部模型',
              style: AppText.md.copyWith(
                color: t.fg,
                fontWeight: AppText.semibold,
              ),
            ),
            if (value) ...[
              const SizedBox(width: 8),
              Container(
                key: const ValueKey('allow-all-tag'),
                padding: const EdgeInsets.symmetric(horizontal: 6),
                decoration: BoxDecoration(
                  color: t.brand100,
                  borderRadius: BorderRadius.circular(AppRadius.badge),
                ),
                child: Text(
                  '*',
                  style: monoStyle(
                    AppText.xs.copyWith(
                      color: t.brand,
                      fontWeight: AppText.semibold,
                    ),
                  ),
                ),
              ),
            ],
            const Spacer(),
            AppSwitch(
              key: const ValueKey('allow-all-switch'),
              value: value,
              onChanged: onChanged,
            ),
          ],
        ),
      ),
    );
  }
}

Tone levelTone(StatusLevel l) => switch (l) {
  StatusLevel.ok => Tone.success,
  StatusLevel.warn => Tone.warning,
  StatusLevel.error => Tone.danger,
  StatusLevel.off => Tone.neutral,
};

/// Title of a failed connection's alert, from its last error.
String connErrorTitle(ConnectionInfo c) {
  final e = c.lastError.toLowerCase();
  if (c.state == 'revoked') return '已被吊销';
  if (e.contains('401') ||
      e.contains('403') ||
      e.contains('revoked') ||
      e.contains('unauthorized')) {
    return '认证失败';
  }
  return '连接错误';
}

/// One paired SaaS: one-line summary, expandable details, error alert.
class _ConnectionTile extends StatefulWidget {
  const _ConnectionTile({
    super.key,
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
  State<_ConnectionTile> createState() => _ConnectionTileState();
}

class _ConnectionTileState extends State<_ConnectionTile> {
  bool expanded = false;

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    final c = widget.c;
    final busy = widget.busy;
    final tone = levelTone(c.level);
    final allow = allowSummary(c.allow, widget.adapters).replaceAll(', ', '、');
    final revoke = AppButton(
      label: '撤销',
      variant: ButtonVariant.dangerOutline,
      small: true,
      onPressed: busy ? null : widget.onRevoke,
    );
    return AppCard(
      padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 12),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          Row(
            children: [
              StatusDot(tone),
              const SizedBox(width: 12),
              Expanded(
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    Text(
                      c.title,
                      maxLines: 1,
                      overflow: TextOverflow.ellipsis,
                      style: AppText.md.copyWith(
                        color: t.fg,
                        fontWeight: AppText.semibold,
                      ),
                    ),
                    const SizedBox(height: 2),
                    Text(
                      '模型：$allow · 任务 ${c.runningTasks}/${c.maxConcurrency}',
                      maxLines: 1,
                      overflow: TextOverflow.ellipsis,
                      style: AppText.xs.copyWith(color: t.fg2),
                    ),
                  ],
                ),
              ),
              const SizedBox(width: 12),
              StatusBadge(c.stateLabel, tone: tone),
              const SizedBox(width: 12),
              AppButton(
                label: '模型',
                small: true,
                onPressed: busy ? null : widget.onModels,
              ),
              if (!c.failed) ...[
                const SizedBox(width: 8),
                AppButton(
                  label: c.paused ? '连接' : '断开',
                  small: true,
                  onPressed: busy ? null : widget.onToggle,
                ),
              ] else ...[
                const SizedBox(width: 8),
                revoke,
              ],
              const SizedBox(width: 8),
              AppIconButton(
                key: ValueKey('conn-expand-${c.id}'),
                icon: expanded
                    ? LucideIcons.chevronUp
                    : LucideIcons.chevronDown,
                tooltip: expanded ? '收起' : '详情',
                onPressed: () => setState(() => expanded = !expanded),
              ),
            ],
          ),
          if (c.failed || (c.lastError.isNotEmpty && !c.connected)) ...[
            const SizedBox(height: 12),
            AppAlert(
              tone: Tone.danger,
              compact: true,
              inline: true,
              title: connErrorTitle(c),
              message: c.lastError.isEmpty
                  ? '请撤销后重新配对。'
                  : '${c.lastError}${c.failed ? '。请撤销后重新配对。' : ''}',
            ),
          ],
          if (expanded) ...[
            const SizedBox(height: 8),
            const Hairline(),
            const SizedBox(height: 4),
            _StatRow('网关', c.issuer),
            _StatRow('租户', c.tenant),
            if (c.displayUrl.isNotEmpty) _StatRow('地址', c.displayUrl),
            _StatRow(
              '传输',
              '${_transportLabel(c.transport)}${c.e2ee ? ' · 端到端加密' : ''}',
            ),
            _StatRow('模型', allow),
            _StatRow('任务', '${c.runningTasks} 个（上限 ${c.maxConcurrency}）'),
            if (!c.failed) ...[
              const SizedBox(height: 8),
              Align(alignment: Alignment.centerRight, child: revoke),
            ],
          ],
        ],
      ),
    );
  }
}

/// One scheme row of the usage table.
class UsageRow {
  const UsageRow(this.name, this.input, this.output, this.total);
  final String name;
  final int input, output, total;
}

/// 用量统计: 3 summary cards, per-scheme table with share bars, total row.
class UsagePanel extends StatelessWidget {
  const UsagePanel({super.key, required this.rows});
  final List<UsageRow> rows;

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    if (rows.isEmpty) {
      return const SizedBox(
        height: 220,
        child: EmptyState(
          icon: LucideIcons.chartColumn,
          title: '暂无用量记录',
          message: '有请求后会显示各方案的 token 用量（不记录对话内容）。',
        ),
      );
    }
    final sorted = [...rows]..sort((a, b) => b.total.compareTo(a.total));
    final inSum = rows.fold<int>(0, (s, r) => s + r.input);
    final outSum = rows.fold<int>(0, (s, r) => s + r.output);
    final total = rows.fold<int>(0, (s, r) => s + r.total);
    final head = AppText.xs.copyWith(color: t.fg2);
    final cell = AppText.sm.copyWith(color: t.fg);
    Widget num(String v, TextStyle s) => SizedBox(
      width: 88,
      child: Text(v, textAlign: TextAlign.right, style: s),
    );
    Widget card(String label, String value) => Expanded(
      child: AppCard(
        padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 12),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(label, style: AppText.xs.copyWith(color: t.fg2)),
            Text(value, style: AppText.xl.copyWith(color: t.fg)),
          ],
        ),
      ),
    );
    final bold = cell.copyWith(fontWeight: AppText.semibold);
    return Column(
      mainAxisSize: MainAxisSize.min,
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        Row(
          children: [
            card('总计 Token', fmtTokens(total)),
            const SizedBox(width: 12),
            card('输入', fmtTokens(inSum)),
            const SizedBox(width: 12),
            card('输出', fmtTokens(outSum)),
          ],
        ),
        const SizedBox(height: 16),
        Container(
          height: 32,
          decoration: BoxDecoration(
            border: Border(bottom: BorderSide(color: t.border)),
          ),
          child: Row(
            children: [
              Expanded(child: Text('方案', style: head)),
              num('输入', head),
              num('输出', head),
              num('总计', head),
            ],
          ),
        ),
        for (final r in sorted)
          Container(
            padding: const EdgeInsets.symmetric(vertical: 8),
            decoration: BoxDecoration(
              border: Border(bottom: BorderSide(color: t.border)),
            ),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.stretch,
              children: [
                Row(
                  children: [
                    Expanded(
                      child: Text(
                        r.name,
                        maxLines: 1,
                        overflow: TextOverflow.ellipsis,
                        style: cell,
                      ),
                    ),
                    num(fmtTokens(r.input), cell),
                    num(fmtTokens(r.output), cell),
                    num(fmtTokens(r.total), bold),
                  ],
                ),
                const SizedBox(height: 4),
                FractionallySizedBox(
                  alignment: Alignment.centerLeft,
                  widthFactor: 0.45,
                  child: Container(
                    height: 4,
                    decoration: BoxDecoration(
                      color: t.brand100,
                      borderRadius: BorderRadius.circular(2),
                    ),
                    alignment: Alignment.centerLeft,
                    child: FractionallySizedBox(
                      widthFactor: total == 0
                          ? 0
                          : (r.total / total).clamp(0.01, 1.0),
                      child: Container(
                        decoration: BoxDecoration(
                          color: t.brand,
                          borderRadius: BorderRadius.circular(2),
                        ),
                      ),
                    ),
                  ),
                ),
              ],
            ),
          ),
        SizedBox(
          height: 40,
          child: Row(
            children: [
              Expanded(child: Text('合计', style: bold)),
              num(fmtTokens(inSum), bold),
              num(fmtTokens(outSum), bold),
              num(fmtTokens(total), bold),
            ],
          ),
        ),
        const SizedBox(height: 4),
        Text('只统计 token 数，不记录对话内容。', style: AppText.xs.copyWith(color: t.fg2)),
      ],
    );
  }
}
