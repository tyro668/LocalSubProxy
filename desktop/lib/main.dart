import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'package:flutter/material.dart';

void main() => runApp(const ProxyApp());

String defaultTokenPath() {
  final env = Platform.environment;
  if (Platform.isWindows) {
    return "${env['APPDATA'] ?? env['USERPROFILE'] ?? '.'}\\localsubproxy\\control.token";
  }
  return "${env['HOME'] ?? '.'}/Library/Application Support/localsubproxy/control.token";
}

class LocalApi {
  LocalApi(this.origin, this.tokenPath);
  final String origin, tokenPath;
  Future<dynamic> request(String method, String path, [Map<String, dynamic>? body]) async {
    final token = (await File(tokenPath).readAsString()).trim();
    final client = HttpClient()..connectionTimeout = const Duration(seconds: 4);
    try {
      final req = await client.openUrl(method, Uri.parse('$origin$path'));
      req.headers.set(HttpHeaders.authorizationHeader, 'Bearer $token');
      if (body != null) {
        req.headers.contentType = ContentType.json;
        req.write(jsonEncode(body));
      }
      final res = await req.close().timeout(const Duration(seconds: 10));
      final data = await utf8.decoder.bind(res).join();
      if (res.statusCode < 200 || res.statusCode >= 300) {
        throw Exception('HTTP ${res.statusCode}: ${data.trim()}');
      }
      return data.isEmpty ? null : jsonDecode(data);
    } finally {
      client.close(force: true);
    }
  }
}

class ProxyApp extends StatelessWidget {
  const ProxyApp({super.key});
  @override
  Widget build(BuildContext context) => MaterialApp(
    title: 'LocalSubProxy',
    debugShowCheckedModeBanner: false,
    theme: ThemeData(
      useMaterial3: true,
      colorScheme: ColorScheme.fromSeed(seedColor: const Color(0xff3958cc)),
      scaffoldBackgroundColor: const Color(0xfff6f7fb),
    ),
    home: const HomePage(),
  );
}

class HomePage extends StatefulWidget {
  const HomePage({super.key});
  @override
  State<HomePage> createState() => _HomePageState();
}

class _HomePageState extends State<HomePage> {
  final origin = TextEditingController(text: 'http://127.0.0.1:8765');
  final tokenPath = TextEditingController(text: defaultTokenPath());
  final binary = TextEditingController(text: Platform.isWindows ? 'localsubproxy.exe' : 'localsubproxy');
  final serverUrl = TextEditingController(text: 'http://127.0.0.1:8080');
  final deviceId = TextEditingController(text: 'dev_local');
  final deviceToken = TextEditingController();
  final journalKey = TextEditingController();
  Timer? timer;
  Process? process;
  Map<String, dynamic>? status;
  List<Map<String, dynamic>> adapters = [];
  String? error;
  bool loading = false;
  LocalApi get api => LocalApi(origin.text.trim(), tokenPath.text.trim());

  @override
  void initState() {
    super.initState();
    refresh();
    timer = Timer.periodic(const Duration(seconds: 10), (_) => refresh());
  }
  @override
  void dispose() {
    timer?.cancel();
    for (final c in [origin, tokenPath, binary, serverUrl, deviceId, deviceToken, journalKey]) {
      c.dispose();
    }
    super.dispose();
  }
  void show(String message) {
    if (mounted) ScaffoldMessenger.of(context).showSnackBar(SnackBar(content: Text(message)));
  }
  Future<void> refresh() async {
    if (loading) return;
    loading = true;
    try {
      final s = await api.request('GET', '/v1/status') as Map<String, dynamic>;
      final a = await api.request('GET', '/v1/adapters') as Map<String, dynamic>;
      if (mounted) setState(() {
        status = s;
        adapters = (a['adapters'] as List).cast<Map<String, dynamic>>();
        error = null;
      });
    } catch (e) {
      if (mounted) setState(() { status = null; error = e.toString(); });
    } finally {
      loading = false;
    }
  }
  Future<void> startProxy() async {
    if (deviceToken.text.length < 32 || journalKey.text.length != 64) {
      show('设备令牌至少 32 字符；加密密钥需为 64 位十六进制字符串。');
      return;
    }
    try {
      process = await Process.start(binary.text.trim(), [
        'connector', '-server-url', serverUrl.text.trim(),
        '-device-id', deviceId.text.trim(),
        '-control-listen', Uri.parse(origin.text.trim()).authority,
        '-control-token-file', tokenPath.text.trim(),
      ], environment: {
        'LOCALSUBPROXY_DEVICE_TOKEN': deviceToken.text,
        'LOCALSUBPROXY_JOURNAL_KEY': journalKey.text,
      });
      process!.stdout.drain<void>();
      process!.stderr.drain<void>();
      await Future<void>.delayed(const Duration(milliseconds: 800));
      await refresh();
      if (status == null) throw Exception('连接器未启动；请检查二进制路径、密钥和 SaaS 地址。');
      show('连接器已启动');
    } catch (e) { show('启动失败：$e'); }
  }
  Future<void> edit([Map<String, dynamic>? existing]) async {
    final changed = await showDialog<bool>(context: context, builder: (_) => AdapterDialog(api: api, existing: existing));
    if (changed == true) await refresh();
  }
  Future<void> remove(Map<String, dynamic> adapter) async {
    final yes = await showDialog<bool>(context: context, builder: (ctx) => AlertDialog(
      title: const Text('删除适配器'),
      content: Text('删除 ${adapter['name']?.toString().isNotEmpty == true ? adapter['name'] : adapter['id']}？'),
      actions: [
        TextButton(onPressed: () => Navigator.pop(ctx, false), child: const Text('取消')),
        FilledButton(onPressed: () => Navigator.pop(ctx, true), child: const Text('删除')),
      ],
    ));
    if (yes != true) return;
    try {
      await api.request('DELETE', '/v1/adapters/${Uri.encodeComponent(adapter['id'] as String)}');
      await refresh();
    } catch (e) { show('$e'); }
  }
  Widget field(TextEditingController controller, String label, {bool secret = false}) =>
    Padding(padding: const EdgeInsets.only(bottom: 12), child: TextField(
      controller: controller, obscureText: secret,
      decoration: InputDecoration(labelText: label, border: const OutlineInputBorder(), isDense: true),
    ));
  @override
  Widget build(BuildContext context) {
    final wide = MediaQuery.sizeOf(context).width > 850;
    return Scaffold(
      appBar: AppBar(title: const Text('LocalSubProxy'), actions: [
        IconButton(tooltip: '刷新', onPressed: refresh, icon: const Icon(Icons.refresh)),
        const SizedBox(width: 12),
      ]),
      body: Center(child: ConstrainedBox(constraints: const BoxConstraints(maxWidth: 1120),
        child: ListView(padding: const EdgeInsets.all(24), children: [
          Text('本机模型接入', style: Theme.of(context).textTheme.headlineMedium),
          const SizedBox(height: 6),
          const Text('管理官方订阅客户端、本地模型和第三方 OpenAI 兼容 API。'),
          const SizedBox(height: 20),
          Card(child: Padding(padding: const EdgeInsets.all(20), child: Column(
            crossAxisAlignment: CrossAxisAlignment.start, children: [
              Row(children: [
                Icon(status == null ? Icons.cloud_off : Icons.check_circle,
                  color: status == null ? Colors.orange : Colors.green),
                const SizedBox(width: 10),
                Text(status == null ? '本机连接器未连接' : '本机连接器已连接',
                  style: Theme.of(context).textTheme.titleMedium),
              ]),
              const SizedBox(height: 8),
              Text(status == null ? (error ?? '请启动连接器。') :
                '设备 ${status!['device_id']} · SaaS ${status!['server_url']} · 正在执行 ${status!['running_tasks']} 个任务'),
              const SizedBox(height: 14),
              if (wide) Row(children: [
                Expanded(child: field(origin, '管理接口地址')),
                const SizedBox(width: 12),
                Expanded(child: field(tokenPath, '本机令牌文件')),
              ]) else ...[field(origin, '管理接口地址'), field(tokenPath, '本机令牌文件')],
              Align(alignment: Alignment.centerRight,
                child: OutlinedButton(onPressed: refresh, child: const Text('连接管理接口'))),
            ],
          ))),
          if (status == null) ...[
            const SizedBox(height: 16),
            Card(child: Padding(padding: const EdgeInsets.all(20), child: Column(
              crossAxisAlignment: CrossAxisAlignment.start, children: [
                Text('启动连接器', style: Theme.of(context).textTheme.titleMedium),
                const SizedBox(height: 12),
                field(binary, 'Go 可执行文件路径'),
                if (wide) Row(children: [
                  Expanded(child: field(serverUrl, 'SaaS 地址')),
                  const SizedBox(width: 12),
                  Expanded(child: field(deviceId, '设备 ID')),
                ]) else ...[field(serverUrl, 'SaaS 地址'), field(deviceId, '设备 ID')],
                field(deviceToken, '设备令牌', secret: true),
                field(journalKey, '本机加密密钥（64 位十六进制）', secret: true),
                Align(alignment: Alignment.centerRight, child: FilledButton.icon(
                  onPressed: startProxy, icon: const Icon(Icons.play_arrow), label: const Text('启动'))),
              ],
            ))),
          ],
          const SizedBox(height: 24),
          Row(children: [
            Expanded(child: Text('适配器', style: Theme.of(context).textTheme.headlineSmall)),
            FilledButton.icon(onPressed: status == null ? null : () => edit(),
              icon: const Icon(Icons.add), label: const Text('添加')),
          ]),
          const SizedBox(height: 12),
          if (adapters.isEmpty) const Card(child: Padding(padding: EdgeInsets.all(28),
            child: Text('暂无适配器。连接本机服务后添加官方订阅、本地模型或第三方 API。'))),
          ...adapters.map((a) => Card(child: ListTile(
            contentPadding: const EdgeInsets.symmetric(horizontal: 20, vertical: 8),
            leading: Icon(a['type'] == 'codex_app_server' ? Icons.terminal :
              a['type'] == 'ollama_http' ? Icons.computer : Icons.cloud_outlined),
            title: Text((a['name'] as String?)?.isNotEmpty == true ? a['name'] : a['id']),
            subtitle: Text("${a['type']} · ${(a['models'] as List).join(', ')}${a['has_api_key'] == true ? ' · 已设置 API Key' : ''}"),
            trailing: Row(mainAxisSize: MainAxisSize.min, children: [
              Chip(label: Text(a['enabled'] == true ? '启用' : '停用')),
              IconButton(tooltip: '编辑', onPressed: () => edit(a), icon: const Icon(Icons.edit_outlined)),
              IconButton(tooltip: '删除', onPressed: () => remove(a), icon: const Icon(Icons.delete_outline)),
            ]),
          ))),
        ]),
      )),
    );
  }
}

class AdapterDialog extends StatefulWidget {
  const AdapterDialog({super.key, required this.api, this.existing});
  final LocalApi api;
  final Map<String, dynamic>? existing;
  @override
  State<AdapterDialog> createState() => _AdapterDialogState();
}

class _AdapterDialogState extends State<AdapterDialog> {
  late final id = TextEditingController(text: widget.existing?['id'] ?? '');
  late final name = TextEditingController(text: widget.existing?['name'] ?? '');
  late final url = TextEditingController(text: widget.existing?['base_url'] ?? '');
  late final codex = TextEditingController(text: widget.existing?['codex_bin'] ?? 'codex');
  late final models = TextEditingController(text: (widget.existing?['models'] as List?)?.join(', ') ?? '');
  final apiKey = TextEditingController();
  late String type = widget.existing?['type'] ?? 'codex_app_server';
  late bool enabled = widget.existing?['enabled'] ?? true;
  bool saving = false;
  String? error;
  @override
  void dispose() {
    for (final c in [id, name, url, codex, models, apiKey]) { c.dispose(); }
    super.dispose();
  }
  Future<void> save() async {
    setState(() { saving = true; error = null; });
    try {
      final body = <String, dynamic>{
        'id': id.text.trim(), 'name': name.text.trim(), 'type': type,
        'models': models.text.split(',').map((s) => s.trim()).where((s) => s.isNotEmpty).toList(),
        'enabled': enabled,
      };
      if (type == 'codex_app_server') { body['codex_bin'] = codex.text.trim(); }
      else { body['base_url'] = url.text.trim(); }
      if (type == 'openai_compatible') { body['api_key'] = apiKey.text.trim(); }
      final current = widget.existing;
      await widget.api.request(current == null ? 'POST' : 'PUT',
        current == null ? '/v1/adapters' : '/v1/adapters/${Uri.encodeComponent(current['id'] as String)}', body);
      if (mounted) Navigator.pop(context, true);
    } catch (e) {
      if (mounted) setState(() => error = e.toString());
    } finally {
      if (mounted) setState(() => saving = false);
    }
  }
  @override
  Widget build(BuildContext context) => AlertDialog(
    title: Text(widget.existing == null ? '添加适配器' : '编辑适配器'),
    content: SizedBox(width: 520, child: SingleChildScrollView(child: Column(
      mainAxisSize: MainAxisSize.min, children: [
        DropdownButtonFormField<String>(
          initialValue: type,
          decoration: const InputDecoration(labelText: '类型'),
          items: const [
            DropdownMenuItem(value: 'codex_app_server', child: Text('官方 Codex 订阅客户端')),
            DropdownMenuItem(value: 'ollama_http', child: Text('本机 Ollama')),
            DropdownMenuItem(value: 'openai_compatible', child: Text('OpenAI 兼容 API')),
          ],
          onChanged: widget.existing == null ? (v) => setState(() => type = v!) : null,
        ),
        TextField(controller: id, enabled: widget.existing == null,
          decoration: const InputDecoration(labelText: '适配器 ID')),
        TextField(controller: name, decoration: const InputDecoration(labelText: '显示名称')),
        if (type == 'codex_app_server')
          TextField(controller: codex, decoration: const InputDecoration(labelText: '官方 Codex CLI 路径')),
        if (type != 'codex_app_server')
          TextField(controller: url, decoration: const InputDecoration(labelText: 'API 根地址（HTTPS 或本机 HTTP）')),
        if (type == 'openai_compatible')
          TextField(controller: apiKey, obscureText: true, decoration: InputDecoration(
            labelText: widget.existing?['has_api_key'] == true ? 'API Key（留空保留原密钥）' : 'API Key')),
        TextField(controller: models, decoration: const InputDecoration(labelText: '允许的模型 ID（逗号分隔）')),
        SwitchListTile(contentPadding: EdgeInsets.zero, title: const Text('启用'), value: enabled,
          onChanged: (v) => setState(() => enabled = v)),
        if (error != null) Text(error!, style: TextStyle(color: Theme.of(context).colorScheme.error)),
      ],
    ))),
    actions: [
      TextButton(onPressed: () => Navigator.pop(context, false), child: const Text('取消')),
      FilledButton(onPressed: saving ? null : save, child: const Text('保存')),
    ],
  );
}
