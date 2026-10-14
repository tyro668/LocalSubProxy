import 'dart:async';

import 'package:shadcn_flutter/shadcn_flutter.dart';
import 'package:window_manager/window_manager.dart';

import 'native_api.dart';
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
  // Local token usage (last 7 days, by SaaS and model).
  List<Map<String, dynamic>> usage = [];
  VoidCallback? _connectionRefresh;

  NativeApi get api => NativeApi.instance;

  @override
  void setState(VoidCallback fn) {
    super.setState(fn);
    _connectionRefresh?.call();
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
      final s = api.status();
      final a = api.listAdapters();
      if (mounted) {
        setState(() {
          status = s;
          adapters = a;
        });
      }
    } catch (_) {
      if (mounted) {
        setState(() => status = {...?status, 'connected': false});
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
          consent = (p['consent'] as Map?)?.cast<String, dynamic>();
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
      final p = await pairApproveAsync();
      if (mounted) {
        setState(() {
          consent = null;
          awaitingCode = true;
          pairUserCode = p['user_code'] as String?;
          pairLinkUrl = p['link_url'] as String?;
        });
      }
    } catch (e) {
      if (mounted) show('配对失败：$e', error: true);
    } finally {
      if (mounted) setState(() => busy = false);
    }
  }

  Future<void> denyConsent() async {
    try {
      await pairDenyAsync();
    } catch (_) {}
    if (mounted) {
      setState(() {
        consent = null;
        pendingRemoteURL = null;
      });
      show('已拒绝该服务的配对请求（CONSENT_DENIED）');
    }
  }

  Future<void> loadUsage() async {
    try {
      final u = await usageStatsAsync(['saas', 'model'], 7);
      if (mounted) setState(() => usage = u);
    } catch (_) {}
  }

  Future<void> pollPairing() async {
    if (pairUserCode == null || !awaitingCode) return;
    try {
      final st = await pairStatusAsync();
      if (!mounted) return;
      if (st['state'] == 'paired') {
        setState(() {
          awaitingCode = false;
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
      await pairCancelAsync();
    } catch (_) {}
    if (mounted) {
      setState(() {
        awaitingCode = false;
        pendingRemoteURL = null;
        pairUserCode = null;
        pairLinkUrl = null;
      });
    }
  }

  Future<void> disconnect() async {
    final yes = await confirm(
      title: '断开连接',
      message: '将清除本机连接信息和设备私钥。模型配置会保留；再次连接需要重新配对。',
      action: '断开',
      destructive: true,
    );
    if (!yes || !mounted) return;
    setState(() => busy = true);
    try {
      await disconnectAsync();
      await refresh(force: true);
      show('已断开');
    } catch (e) {
      if (mounted) show('断开失败：$e', error: true);
    } finally {
      if (mounted) setState(() => busy = false);
    }
  }

  Future<void> openConnection() async {
    await showOverlay<void>(
      context,
      DialogConfiguration<void>(builder: (_) => _ConnectionRelay(host: this)),
    ).future;
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
    final connected = status?['connected'] == true;
    final theme = Theme.of(context);
    return Card(
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          Row(
            children: [
              Icon(
                connected ? LucideIcons.circleCheck : LucideIcons.cloudOff,
                size: 18,
                color: connected
                    ? theme.colorScheme.chart2
                    : theme.colorScheme.mutedForeground,
              ),
              const Gap(8),
              Expanded(child: Text(connected ? '已连接' : '未连接').medium()),
              Tooltip(
                tooltip: (context) => const TooltipContainer(child: Text('刷新')),
                child: IconButton.ghost(
                  density: ButtonDensity.icon,
                  icon: const Icon(LucideIcons.refreshCw),
                  onPressed: busy ? null : () => refresh(),
                ),
              ),
              if (paired)
                OutlineButton(
                  size: ButtonSize.small,
                  onPressed: busy ? null : disconnect,
                  child: const Text('断开'),
                ),
            ],
          ),
          if (paired) ...[
            const Gap(14),
            if ((status?['saas_name'] ?? '') != '')
              _StatRow('服务', '${status?['saas_name']}'),
            if ((status?['issuer'] ?? '') != '')
              _StatRow('网关', '${status?['issuer']}'),
            _StatRow(
              '设备',
              '${status?['device_name'] ?? status?['device_id'] ?? ''}',
            ),
            _StatRow('地址', '${status?['remote_url'] ?? ''}'),
            _StatRow('传输', _transportLabel(status?['transport'])),
            _StatRow('任务', '${status?['running_tasks'] ?? 0} 个'),
            const Gap(14),
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
            if (usage.isEmpty)
              const Text('暂无记录（不记录对话内容）').small().muted()
            else
              for (final b in usage)
                _StatRow(
                  '${b['saas_name'] ?? b['saas'] ?? ''} · ${b['model'] ?? ''}',
                  '${b['requests']} 次 · ${b['total_tokens']} tokens'
                      '${(b['errors'] ?? 0) > 0 ? ' · ${b['errors']} 失败' : ''}',
                ),
          ] else if (consent != null) ...[
            const Gap(12),
            _ConsentView(consent: consent!),
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
          ] else ...[
            const Gap(8),
            const Text(
              '输入远端服务地址并点击连接，然后在已登录的远端服务页面输入本机显示的配对码。',
            ).small().muted(),
            if ((status?['pairing_error'] ?? '') != '' && !awaitingCode) ...[
              const Gap(8),
              Text('${status?['pairing_error']}').small(),
            ],
            const Gap(16),
            if (awaitingCode)
              Text(
                '远端服务地址：${pendingRemoteURL ?? remoteUrl.text.trim()}',
              ).small().medium()
            else
              LabeledField(
                label: '远端服务地址',
                child: TextField(
                  controller: remoteUrl,
                  placeholder: const Text(
                    'https://remote.example.com 或 lmbp://pair?… 链接',
                  ),
                  features: const [InputFeature.clear()],
                ),
              ),
            if (awaitingCode) ...[
              const Gap(16),
              LabeledField(
                label: '配对码（请在远端服务页面输入）',
                hint: '10 分钟内有效；确认后本机会自动完成连接',
                child: Text(
                  pairUserCode ?? '',
                  style: const TextStyle(
                    fontSize: 28,
                    letterSpacing: 4,
                    fontFamily: 'monospace',
                  ),
                ),
              ),
              if (pairLinkUrl != null && pairLinkUrl!.isNotEmpty) ...[
                const Gap(8),
                Text('打开：$pairLinkUrl').small().muted(),
              ],
              const Gap(8),
              const Text('等待远端服务确认…').small().muted(),
            ],
            const Gap(16),
            Row(
              mainAxisAlignment: MainAxisAlignment.end,
              children: [
                if (awaitingCode) ...[
                  GhostButton(
                    onPressed: busy ? null : cancelPairing,
                    child: const Text('返回'),
                  ),
                  const Gap(8),
                ],
                if (!awaitingCode)
                  PrimaryButton(
                    onPressed: busy ? null : prepareConnection,
                    leading: const Icon(LucideIcons.link),
                    child: Text(busy ? '处理中…' : '连接'),
                  ),
              ],
            ),
          ],
        ],
      ),
    );
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
      child: kernelReady
          ? ProfileWorkspace(
              api: api,
              adapters: adapters,
              status: status,
              onConnectionTap: openConnection,
              onChanged: () => refresh(force: true),
            )
          : Center(
              child: ConstrainedBox(
                constraints: const BoxConstraints(maxWidth: 560),
                child: connectionCard(),
              ),
            ),
    );
  }
}

class _ConnectionRelay extends StatefulWidget {
  const _ConnectionRelay({required this.host});

  final _HomePageState host;

  @override
  State<_ConnectionRelay> createState() => _ConnectionRelayState();
}

class _ConnectionRelayState extends State<_ConnectionRelay> {
  void refresh() {
    if (mounted) setState(() {});
  }

  @override
  void initState() {
    super.initState();
    widget.host._connectionRefresh = refresh;
  }

  @override
  void dispose() {
    if (widget.host._connectionRefresh == refresh) {
      widget.host._connectionRefresh = null;
    }
    super.dispose();
  }

  @override
  Widget build(BuildContext context) => AlertDialog(
    title: const Text('远端服务连接'),
    content: SizedBox(width: 470, child: widget.host.connectionCard()),
    actions: [
      OutlineButton(
        onPressed: () => Navigator.of(context).pop(),
        child: const Text('关闭'),
      ),
    ],
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
          SizedBox(width: 48, child: Text(label).small().muted()),
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
        'enabled': true,
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
      if (widget.existing == null) {
        widget.api.createAdapter(body);
      } else {
        widget.api.updateAdapter(body);
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
                  const Gap(14),
                  _ToggleRow(
                    title: '跳过模型列表探测',
                    subtitle: '提供方不支持 GET /models 时使用；按已配置的模型开放任务。',
                    value: skipModelProbe,
                    onChanged: (value) =>
                        setState(() => skipModelProbe = value),
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

class _ToggleRow extends StatelessWidget {
  const _ToggleRow({
    required this.title,
    required this.value,
    required this.onChanged,
    this.subtitle,
  });

  final String title;
  final String? subtitle;
  final bool value;
  final ValueChanged<bool> onChanged;

  @override
  Widget build(BuildContext context) {
    return Row(
      crossAxisAlignment: CrossAxisAlignment.center,
      children: [
        Expanded(
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text(title).small().medium(),
              if (subtitle != null) ...[
                const Gap(2),
                Text(subtitle!).xSmall().muted(),
              ],
            ],
          ),
        ),
        const Gap(12),
        Switch(value: value, onChanged: onChanged),
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
    final saas = (consent['saas'] as Map?)?.cast<String, dynamic>() ?? {};
    final models = ((consent['requested_models'] as List?) ?? []).join(', ');
    final scopes = ((consent['requested_scopes'] as List?) ?? []).join(', ');
    final logo = '${saas['logo_uri'] ?? ''}';
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        Row(
          children: [
            if (logo.startsWith('https://'))
              Padding(
                padding: const EdgeInsets.only(right: 10),
                child: Image.network(logo, width: 32, height: 32,
                    errorBuilder: (_, __, ___) => const SizedBox.shrink()),
              ),
            Expanded(
              child: Text('「${saas['name'] ?? ''}」请求使用本机模型').medium(),
            ),
          ],
        ),
        const Gap(4),
        Text(consent['verified'] == true
                ? '身份已通过网关签名校验'
                : '身份未校验')
            .small()
            .muted(),
        const Gap(10),
        _StatRow('网关', '${consent['gateway'] ?? ''}'),
        _StatRow('租户', '${consent['tenant'] ?? ''}'),
        if ('${saas['homepage_uri'] ?? ''}' != '')
          _StatRow('主页', '${saas['homepage_uri']}'),
        if ('${saas['policy_uri'] ?? ''}' != '')
          _StatRow('隐私政策', '${saas['policy_uri']}'),
        _StatRow('请求模型', models.isEmpty ? '由本机配置决定' : models),
        _StatRow('请求权限', scopes),
        _StatRow('网关密钥', '${consent['gateway_key_id'] ?? ''}'),
      ],
    );
  }
}
