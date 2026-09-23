// ignore_for_file: curly_braces_in_flow_control_structures, implicit_call_tearoffs
import 'dart:async';
import 'dart:io';

import 'package:shadcn_flutter/shadcn_flutter.dart';

import 'native_api.dart';

const _purple = Color(0xFF5B37DD);
const _border = Color(0xFFDFD9EC);
const _ink = Color(0xFF292D3B);
const _muted = Color(0xFF707586);
const _soft = Color(0xFFF0ECFF);

class ProfileWorkspace extends StatefulWidget {
  const ProfileWorkspace({
    super.key,
    required this.api,
    required this.adapters,
    required this.onChanged,
  });

  final NativeApi api;
  final List<Map<String, dynamic>> adapters;
  final Future<void> Function() onChanged;

  @override
  State<ProfileWorkspace> createState() => _ProfileWorkspaceState();
}

class _ProfileWorkspaceState extends State<ProfileWorkspace> {
  final name = TextEditingController();
  final model = TextEditingController();
  final baseURL = TextEditingController();
  final apiKey = TextEditingController();
  final codexPath = TextEditingController(text: 'codex');
  final contextWindow = TextEditingController();
  final maxTokens = TextEditingController();
  String? selectedID;
  String provider = 'codex';
  String reasoning = 'medium';
  bool isDefault = false;
  bool skipModelProbe = false;
  bool dirty = false;
  bool working = false;
  bool codexConnected = false;
  bool checkingCodex = false;
  bool loginPending = false;
  String? loginURL;
  String? testStatus;
  bool testPassed = false;
  String? error;
  List<String> codexModels = [];
  List<String> originalModels = [];
  String originalModel = '';
  Timer? loginPoll;
  VoidCallback? _dialogRefresh;
  String? highlightedID;

  Map<String, dynamic>? get selected {
    for (final item in widget.adapters) {
      if (item['id'] == selectedID) return item;
    }
    return null;
  }

  bool get creating => selectedID == null;
  bool get hasReusableKey =>
      selected?['has_api_key'] == true &&
      selected?['type'] == type &&
      selected?['base_url'] == baseURL.text.trim();
  String get type => switch (provider) {
    'codex' => 'codex_app_server',
    'anthropic' => 'anthropic_compatible',
    'legacy_ollama' => 'ollama_http',
    _ => 'openai_compatible',
  };

  @override
  void initState() {
    super.initState();
    for (final c in [
      name,
      model,
      baseURL,
      apiKey,
      codexPath,
      contextWindow,
      maxTokens,
    ]) {
      c.addListener(markDirty);
    }
    _pickHighlight();
  }

  @override
  void didUpdateWidget(covariant ProfileWorkspace oldWidget) {
    super.didUpdateWidget(oldWidget);
    _pickHighlight();
  }

  void _pickHighlight() {
    if (widget.adapters.any((item) => item['id'] == highlightedID)) return;
    if (widget.adapters.isEmpty) {
      highlightedID = null;
      return;
    }
    final defaults = widget.adapters.where(
      (item) => item['is_default'] == true,
    );
    highlightedID =
        (defaults.isNotEmpty ? defaults.first : widget.adapters.first)['id']
            as String?;
  }

  @override
  void setState(VoidCallback fn) {
    super.setState(fn);
    _dialogRefresh?.call();
  }

  @override
  void dispose() {
    loginPoll?.cancel();
    for (final c in [
      name,
      model,
      baseURL,
      apiKey,
      codexPath,
      contextWindow,
      maxTokens,
    ]) {
      c.dispose();
    }
    super.dispose();
  }

  void markDirty() {
    if (!mounted) return;
    setState(() {
      dirty = true;
      testStatus = null;
    });
  }

  void load(Map<String, dynamic> a) {
    selectedID = a['id'] as String;
    provider = switch (a['type']) {
      'codex_app_server' => 'codex',
      'anthropic_compatible' => 'anthropic',
      'ollama_http' => 'legacy_ollama',
      _ => 'openai',
    };
    name.text = (a['name'] as String?)?.isNotEmpty == true
        ? a['name'] as String
        : selectedID!;
    originalModels = (a['models'] as List?)?.cast<String>() ?? [];
    model.text = originalModels.isEmpty ? '' : originalModels.first;
    originalModel = model.text;
    baseURL.text = a['base_url'] as String? ?? '';
    apiKey.clear();
    codexPath.text = a['codex_bin'] as String? ?? 'codex';
    contextWindow.text = (a['context_window'] as int? ?? 0) == 0
        ? ''
        : '${a['context_window']}';
    maxTokens.text = (a['max_output_tokens'] as int? ?? 0) == 0
        ? ''
        : '${a['max_output_tokens']}';
    reasoning = (a['reasoning_effort'] as String?)?.isNotEmpty == true
        ? a['reasoning_effort'] as String
        : (provider == 'codex' ? 'medium' : 'off');
    isDefault = a['is_default'] == true;
    skipModelProbe = a['skip_model_probe'] == true;
    dirty = false;
    testStatus = null;
    error = null;
    codexConnected = a['auth_state'] != 'required' && a['health'] == 'ready';
    if (mounted) setState(() {});
    if (provider == 'codex') refreshCodex();
  }

  void newProfile() {
    selectedID = null;
    name.text = '';
    model.text = '';
    originalModels = [];
    originalModel = '';
    provider = 'codex';
    baseURL.text = '';
    apiKey.clear();
    codexPath.text = 'codex';
    contextWindow.text = '';
    maxTokens.text = '';
    reasoning = 'medium';
    isDefault = widget.adapters.isEmpty;
    skipModelProbe = false;
    dirty = false;
    testStatus = null;
    error = null;
    if (mounted) setState(() {});
    refreshCodex();
  }

  Future<bool> confirm(String title, String message, String action) async {
    final result = await showOverlay<bool>(
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
            PrimaryButton(
              onPressed: () => Navigator.of(ctx).pop(true),
              child: Text(action),
            ),
          ],
        ),
      ),
    ).future;
    return result == true;
  }

  Future<bool> allowDiscard() async {
    if (!dirty) return true;
    return confirm('放弃未保存的更改？', '当前方案的更改尚未保存。', '放弃更改');
  }

  Future<void> openEditor([Map<String, dynamic>? existing]) async {
    if (!mounted) return;
    if (existing == null) {
      newProfile();
    } else {
      load(existing);
    }
    await showOverlay<void>(
      context,
      DialogConfiguration<void>(
        barrierDismissible: false,
        barrierColor: const Color.fromRGBO(0, 0, 0, 0.72),
        builder: (ctx) => _EditorRelay(host: this),
      ),
    ).future;
  }

  Future<void> setDefault(Map<String, dynamic> a) async {
    if (a['is_default'] == true) return;
    if (!await allowDiscard() || !mounted) return;
    try {
      final body = <String, dynamic>{...a, 'is_default': true};
      body.remove('has_api_key');
      body.remove('health');
      body.remove('auth_state');
      await updateAdapterAsync(body);
      await widget.onChanged();
    } catch (e) {
      if (mounted) setState(() => error = '$e');
    }
  }

  Future<void> deleteProfile(Map<String, dynamic> a) async {
    if (!await confirm(
          '删除方案',
          '确定删除 ${a['name']}？此操作会从远端服务可用模型中移除该方案。',
          '删除',
        ) ||
        !mounted)
      return;
    try {
      await deleteAdapterAsync(a['id'] as String);
      if (selectedID == a['id']) selectedID = null;
      dirty = false;
      await widget.onChanged();
    } catch (e) {
      if (mounted) setState(() => error = '$e');
    }
  }

  Future<void> options(Map<String, dynamic> a) async {
    await showOverlay<void>(
      context,
      DialogConfiguration<void>(
        builder: (ctx) => AlertDialog(
          title: Text(a['name'] as String? ?? '方案'),
          content: Column(
            mainAxisSize: MainAxisSize.min,
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              OutlineButton(
                onPressed: () {
                  Navigator.of(ctx).pop();
                  renameProfile(a);
                },
                child: const Text('重命名方案'),
              ),
              const Gap(10),
              OutlineButton(
                onPressed: () {
                  Navigator.of(ctx).pop();
                  setDefault(a);
                },
                child: const Text('设为默认方案'),
              ),
              const Gap(10),
              OutlineButton(
                onPressed: () {
                  Navigator.of(ctx).pop();
                  deleteProfile(a);
                },
                child: const Text('删除方案'),
              ),
            ],
          ),
          actions: [
            OutlineButton(
              onPressed: () => Navigator.of(ctx).pop(),
              child: const Text('关闭'),
            ),
          ],
        ),
      ),
    ).future;
  }

  Future<void> renameProfile(Map<String, dynamic> a) async {
    final input = TextEditingController(text: '${a['name'] ?? a['id']}');
    final result = await showOverlay<bool>(
      context,
      DialogConfiguration<bool>(
        builder: (ctx) => AlertDialog(
          title: const Text('重命名方案'),
          content: SizedBox(width: 360, child: TextField(controller: input)),
          actions: [
            OutlineButton(
              onPressed: () => Navigator.of(ctx).pop(false),
              child: const Text('取消'),
            ),
            PrimaryButton(
              onPressed: () => Navigator.of(ctx).pop(true),
              child: const Text('确定'),
            ),
          ],
        ),
      ),
    ).future;
    final next = input.text.trim();
    input.dispose();
    if (result != true || !mounted || next.isEmpty || next == a['name']) {
      return;
    }
    try {
      final body = <String, dynamic>{...a, 'name': next};
      body.remove('has_api_key');
      body.remove('health');
      body.remove('auth_state');
      await updateAdapterAsync(body);
      await widget.onChanged();
    } catch (e) {
      if (mounted) setState(() => error = '$e');
    }
  }

  Future<void> closeEditor(BuildContext dialogContext) async {
    if (!await allowDiscard() || !dialogContext.mounted) return;
    dirty = false;
    _dialogRefresh = null;
    Navigator.of(dialogContext).pop();
  }

  Widget _editorDialog(BuildContext dialogContext) {
    final theme = Theme.of(dialogContext);
    final size = MediaQuery.sizeOf(dialogContext);
    final width = size.width - 80 < 880 ? size.width - 80 : 880.0;
    final height = size.height * 0.78;
    return SizedBox(
      width: width,
      height: height,
      child: ModalContainer(
        filled: true,
        fillColor: theme.colorScheme.popover,
        borderColor: theme.colorScheme.border,
        borderWidth: 1,
        borderRadius: BorderRadius.circular(theme.radiusXl),
        clipBehavior: Clip.antiAlias,
        padding: EdgeInsets.zero,
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Padding(
              padding: const EdgeInsets.fromLTRB(24, 20, 24, 16),
              child: Text(creating ? '添加方案' : '编辑方案').large().semiBold(),
            ),
            const Divider(),
            Expanded(
              child: SingleChildScrollView(
                padding: const EdgeInsets.fromLTRB(24, 16, 24, 20),
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.stretch,
                  children: [
                    TextField(
                      controller: name,
                      placeholder: const Text('方案名称（默认使用模型 ID）'),
                    ),
                    const Gap(16),
                    authPanel(),
                    const Gap(16),
                    modelPanel(),
                    if (error != null) ...[
                      const Gap(10),
                      Text(
                        error!,
                        style: const TextStyle(
                          color: Color(0xFFAD3232),
                          fontSize: 12,
                        ),
                      ),
                    ],
                  ],
                ),
              ),
            ),
            const Divider(),
            Padding(
              padding: const EdgeInsets.fromLTRB(20, 12, 20, 16),
              child: Row(
                children: [
                  OutlineButton(
                    onPressed: working ? null : test,
                    leading: const Icon(LucideIcons.play, size: 16),
                    child: const Text('测试连接'),
                  ),
                  const Gap(10),
                  Icon(
                    testPassed ? LucideIcons.circleCheck : LucideIcons.server,
                    color: testPassed ? const Color(0xFF218153) : _muted,
                    size: 17,
                  ),
                  const Gap(7),
                  Expanded(
                    child: Text(
                      testStatus ?? '连接尚未测试',
                      maxLines: 1,
                      overflow: TextOverflow.ellipsis,
                      style: const TextStyle(color: _muted, fontSize: 13),
                    ),
                  ),
                  const Gap(12),
                  OutlineButton(
                    onPressed: working
                        ? null
                        : () => closeEditor(dialogContext),
                    child: const Text('取消'),
                  ),
                  const Gap(8),
                  PrimaryButton(
                    onPressed: working ? null : () => save(dialogContext),
                    child: Text(working ? '处理中…' : '确定'),
                  ),
                ],
              ),
            ),
          ],
        ),
      ),
    );
  }

  void changeProvider(String? value) {
    if (value == null || value == provider) return;
    setState(() {
      provider = value;
      baseURL.text = '';
      model.text = value == 'codex' && codexModels.isNotEmpty
          ? codexModels.first
          : '';
      reasoning = value == 'codex' ? 'medium' : 'off';
      skipModelProbe = false;
      apiKey.clear();
      testStatus = null;
      dirty = true;
    });
    if (provider == 'codex') refreshCodex();
  }

  Future<void> refreshCodex() async {
    if (provider != 'codex' || checkingCodex) return;
    setState(() => checkingCodex = true);
    try {
      final list = await codexModelsAsync(
        codexPath.text.trim().isEmpty ? 'codex' : codexPath.text.trim(),
      );
      if (!mounted || provider != 'codex') return;
      setState(() {
        codexModels = list;
        codexConnected = true;
        if (model.text.isEmpty && list.isNotEmpty) {
          model.text = list.first;
          if (creating) dirty = true;
        }
      });
    } catch (_) {
      if (mounted)
        setState(() {
          codexConnected = false;
          codexModels = [];
        });
    } finally {
      if (mounted) setState(() => checkingCodex = false);
    }
  }

  Future<void> connectCodex() async {
    setState(() {
      working = true;
      error = null;
    });
    try {
      final url = await startCodexLoginAsync(
        codexPath.text.trim().isEmpty ? 'codex' : codexPath.text.trim(),
      );
      if (!mounted) return;
      setState(() {
        loginURL = url;
        loginPending = true;
      });
      if (Platform.isMacOS) {
        await Process.run('open', [url]);
      } else if (Platform.isWindows) {
        await Process.run('rundll32.exe', ['url.dll,FileProtocolHandler', url]);
      }
      loginPoll?.cancel();
      loginPoll = Timer.periodic(
        const Duration(seconds: 3),
        (_) => checkLogin(),
      );
    } catch (e) {
      if (mounted) setState(() => error = '启动订阅登录失败：$e');
    } finally {
      if (mounted) setState(() => working = false);
    }
  }

  Future<void> checkLogin() async {
    if (!loginPending || checkingCodex) return;
    checkingCodex = true;
    try {
      final data = await checkCodexLoginAsync();
      if (!mounted) return;
      if (data['complete'] == true) {
        loginPoll?.cancel();
        setState(() {
          loginPending = false;
          loginURL = null;
          codexConnected = true;
          codexModels = (data['models'] as List?)?.cast<String>() ?? [];
        });
        if (model.text.isEmpty && codexModels.isNotEmpty)
          model.text = codexModels.first;
      }
    } catch (e) {
      if (mounted) setState(() => error = '检查订阅登录失败：$e');
    } finally {
      checkingCodex = false;
    }
  }

  Future<void> cancelLogin() async {
    loginPoll?.cancel();
    await cancelCodexLoginAsync();
    if (mounted)
      setState(() {
        loginPending = false;
        loginURL = null;
      });
  }

  Future<void> logoutCodex() async {
    if (!await confirm(
          '断开 Codex 订阅',
          '将退出本机 Codex CLI 的登录状态，其他使用此登录的应用也会受到影响。',
          '断开连接',
        ) ||
        !mounted)
      return;
    setState(() => working = true);
    try {
      await logoutCodexAsync(
        codexPath.text.trim().isEmpty ? 'codex' : codexPath.text.trim(),
      );
      if (mounted)
        setState(() {
          codexConnected = false;
          codexModels = [];
          testStatus = null;
        });
    } catch (e) {
      if (mounted) setState(() => error = '$e');
    } finally {
      if (mounted) setState(() => working = false);
    }
  }

  Map<String, dynamic> body() {
    final chosenModel = model.text.trim();
    final models =
        selectedID != null &&
            chosenModel == originalModel &&
            originalModels.isNotEmpty
        ? originalModels
        : [chosenModel];
    return {
      'id': selectedID ?? 'profile_${DateTime.now().microsecondsSinceEpoch}',
      'name': name.text.trim().isEmpty ? chosenModel : name.text.trim(),
      'type': type,
      'provider':
          selected != null &&
              selected?['type'] == type &&
              selected?['base_url'] == baseURL.text.trim()
          ? (selected?['provider'] ?? provider)
          : provider,
      'models': models,
      'base_url': type == 'codex_app_server' ? '' : baseURL.text.trim(),
      'api_key': apiKey.text.trim(),
      'codex_bin': codexPath.text.trim().isEmpty
          ? 'codex'
          : codexPath.text.trim(),
      'context_window': int.tryParse(contextWindow.text.trim()) ?? 0,
      'max_output_tokens': int.tryParse(maxTokens.text.trim()) ?? 0,
      'reasoning_effort': reasoning,
      'is_default': isDefault,
      'enabled': true,
      'skip_model_probe': skipModelProbe,
    };
  }

  String? validate() {
    if (model.text.trim().isEmpty) return '请输入模型 ID。';
    if (contextWindow.text.trim().isNotEmpty &&
        (int.tryParse(contextWindow.text.trim()) ?? -1) < 1)
      return '上下文窗口必须是正整数。';
    if (maxTokens.text.trim().isNotEmpty &&
        (int.tryParse(maxTokens.text.trim()) ?? -1) < 1)
      return '最大输出 Token 必须是正整数。';
    if (type != 'codex_app_server' && baseURL.text.trim().isEmpty)
      return '请输入 API 根地址。';
    if ((type == 'openai_compatible' || type == 'anthropic_compatible') &&
        apiKey.text.trim().isEmpty &&
        !hasReusableKey)
      return '请输入 API 密钥。';
    return null;
  }

  Future<void> test() async {
    final issue = validate();
    if (issue != null) {
      setState(() => error = issue);
      return;
    }
    setState(() {
      working = true;
      error = null;
      testStatus = '正在测试连接…';
      testPassed = false;
    });
    try {
      final draft = body();
      final result = await testAdapterAsync(draft);
      if (!mounted) return;
      setState(() {
        testPassed = result['health'] == 'ready';
        testStatus = testPassed
            ? '连接成功，模型可用'
            : '连接不可用：${result['auth_state'] == 'required' ? '需要登录' : '请检查地址、密钥和模型 ID'}';
      });
    } catch (e) {
      if (mounted)
        setState(() {
          testStatus = '连接测试失败';
          error = '$e';
        });
    } finally {
      if (mounted) setState(() => working = false);
    }
  }

  Future<void> save([BuildContext? dialogContext]) async {
    final issue = validate();
    if (issue != null) {
      setState(() => error = issue);
      return;
    }
    setState(() {
      working = true;
      error = null;
    });
    try {
      final draft = body();
      if (creating) {
        await createAdapterAsync(draft);
      } else {
        await updateAdapterAsync(draft);
      }
      selectedID = draft['id'] as String;
      dirty = false;
      await widget.onChanged();
      if (dialogContext != null && dialogContext.mounted) {
        _dialogRefresh = null;
        Navigator.of(dialogContext).pop();
        return;
      }
      if (mounted)
        setState(() {
          dirty = false;
          testStatus ??= '设置已保存';
        });
    } catch (e) {
      if (mounted) setState(() => error = '保存失败：$e');
    } finally {
      if (mounted) setState(() => working = false);
    }
  }

  Widget iconBox(IconData icon) => Container(
    width: 42,
    height: 42,
    alignment: Alignment.center,
    decoration: BoxDecoration(
      color: _soft,
      borderRadius: BorderRadius.circular(12),
    ),
    child: Icon(icon, color: _purple, size: 24),
  );

  Map<String, dynamic>? get highlighted {
    for (final item in widget.adapters) {
      if (item['id'] == highlightedID) return item;
    }
    return null;
  }

  String _providerLabel(Map<String, dynamic> adapter) {
    return switch (adapter['type']) {
      'codex_app_server' => 'OpenAI Codex（订阅）',
      'anthropic_compatible' => 'Anthropic 兼容协议',
      'ollama_http' => '旧版 Ollama',
      _ => 'OpenAI 兼容协议',
    };
  }

  Widget schemeRow(Map<String, dynamic> adapter) {
    final active = adapter['id'] == highlightedID;
    final models = (adapter['models'] as List?)?.cast<String>() ?? [];
    final model = models.isEmpty ? '未设置模型' : models.first;
    return GestureDetector(
      onTap: () => setState(() => highlightedID = adapter['id'] as String?),
      child: Container(
        color: active ? _soft : const Color(0xFFFFFFFF),
        padding: const EdgeInsets.fromLTRB(16, 12, 8, 12),
        child: Row(
          children: [
            Icon(
              active ? LucideIcons.circleCheck : LucideIcons.circle,
              color: active ? _purple : _muted,
              size: 18,
            ),
            const Gap(12),
            Expanded(
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Row(
                    children: [
                      Flexible(
                        child: Text(
                          '${adapter['name'] ?? adapter['id']}',
                          maxLines: 1,
                          overflow: TextOverflow.ellipsis,
                          style: const TextStyle(
                            color: _ink,
                            fontSize: 15,
                            fontWeight: FontWeight.w600,
                          ),
                        ),
                      ),
                      if (adapter['is_default'] == true) ...[
                        const Gap(8),
                        Container(
                          padding: const EdgeInsets.symmetric(
                            horizontal: 7,
                            vertical: 3,
                          ),
                          decoration: BoxDecoration(
                            color: _purple,
                            borderRadius: BorderRadius.circular(6),
                          ),
                          child: const Text(
                            '默认',
                            style: TextStyle(
                              color: Color(0xFFFFFFFF),
                              fontSize: 10,
                            ),
                          ),
                        ),
                      ],
                      if (adapter['enabled'] == false) ...[
                        const Gap(8),
                        const Text(
                          '停用',
                          style: TextStyle(color: _muted, fontSize: 12),
                        ),
                      ],
                    ],
                  ),
                  const Gap(3),
                  Text(
                    '${_providerLabel(adapter)} · $model',
                    maxLines: 1,
                    overflow: TextOverflow.ellipsis,
                    style: const TextStyle(color: _muted, fontSize: 12),
                  ),
                ],
              ),
            ),
            IconButton.ghost(
              density: ButtonDensity.icon,
              icon: const Icon(LucideIcons.ellipsisVertical, size: 17),
              onPressed: () {
                setState(() => highlightedID = adapter['id'] as String?);
                options(adapter);
              },
            ),
          ],
        ),
      ),
    );
  }

  Widget fieldRow(String label, Widget child) => Padding(
    padding: const EdgeInsets.only(top: 19),
    child: Row(
      crossAxisAlignment: CrossAxisAlignment.center,
      children: [
        SizedBox(
          width: 132,
          child: Text(
            label,
            style: const TextStyle(
              color: _ink,
              fontSize: 14,
              fontWeight: FontWeight.w600,
            ),
          ),
        ),
        Expanded(child: child),
      ],
    ),
  );

  Widget providerSelect() => Select<String>(
    value: provider,
    itemBuilder: (context, value) => Text(switch (value) {
      'codex' => 'OpenAI Codex（订阅）',
      'openai' => 'OpenAI 兼容协议',
      'anthropic' => 'Anthropic 兼容协议',
      _ => '旧版 Ollama（仅现有方案）',
    }),
    onChanged: changeProvider,
    popup: const SelectPopup(
      items: SelectItemList(
        children: [
          SelectItemButton(value: 'codex', child: Text('OpenAI Codex（订阅）')),
          SelectItemButton(value: 'openai', child: Text('OpenAI 兼容协议')),
          SelectItemButton(value: 'anthropic', child: Text('Anthropic 兼容协议')),
        ],
      ),
    ),
  );

  Widget authPanel() => panel(LucideIcons.wifi, '服务与认证', [
    fieldRow('API 提供商', providerSelect()),
    if (provider == 'codex') ...[
      const Gap(21),
      Container(
        padding: const EdgeInsets.all(17),
        decoration: BoxDecoration(
          border: Border.all(color: _border),
          borderRadius: BorderRadius.circular(12),
        ),
        child: Row(
          children: [
            Icon(
              codexConnected ? LucideIcons.circleCheck : LucideIcons.circleHelp,
              color: codexConnected ? const Color(0xFF218153) : _muted,
              size: 25,
            ),
            const Gap(12),
            Expanded(
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  const Text(
                    'ChatGPT 订阅（Codex）连接',
                    style: TextStyle(
                      color: _ink,
                      fontSize: 14,
                      fontWeight: FontWeight.w600,
                    ),
                  ),
                  const Gap(4),
                  Text(
                    codexConnected
                        ? '已连接：本地 Codex CLI 登录态'
                        : checkingCodex
                        ? '正在检查本机登录态…'
                        : '尚未连接本机 Codex CLI',
                    style: TextStyle(
                      color: codexConnected ? const Color(0xFF218153) : _muted,
                      fontSize: 12,
                    ),
                  ),
                ],
              ),
            ),
            OutlineButton(
              onPressed: working
                  ? null
                  : (codexConnected ? logoutCodex : connectCodex),
              child: Text(codexConnected ? '断开连接' : '连接订阅'),
            ),
          ],
        ),
      ),
      if (loginPending) ...[
        const Gap(12),
        Text(
          '请在浏览器完成登录，然后等待自动检测。',
          style: const TextStyle(color: _muted, fontSize: 12),
        ),
        if (loginURL != null)
          SelectableText(
            loginURL!,
            style: const TextStyle(color: _purple, fontSize: 11),
          ),
        const Gap(8),
        Row(
          children: [
            OutlineButton(onPressed: checkLogin, child: const Text('检查登录')),
            const Gap(8),
            OutlineButton(onPressed: cancelLogin, child: const Text('取消')),
          ],
        ),
      ],
      fieldRow(
        'Codex CLI 路径',
        TextField(controller: codexPath, placeholder: const Text('codex')),
      ),
    ] else ...[
      fieldRow(
        'API 地址',
        TextField(
          controller: baseURL,
          placeholder: const Text('https://example.com/v1'),
        ),
      ),
      if (provider != 'legacy_ollama')
        fieldRow(
          'API 密钥',
          TextField(
            controller: apiKey,
            placeholder: Text(hasReusableKey ? '已保存密钥，留空则保留' : '输入 API 密钥'),
            features: const [InputFeature.passwordToggle()],
          ),
        ),
    ],
  ]);

  Widget modelSelect() {
    if (provider != 'codex') {
      return TextField(
        controller: model,
        placeholder: const Text('输入供应商提供的模型 ID'),
      );
    }
    final choices = <String>{
      if (model.text.isNotEmpty) model.text,
      ...codexModels,
    }.toList();
    if (choices.isEmpty)
      return TextField(controller: model, placeholder: const Text('输入模型 ID'));
    return Select<String>(
      value: model.text.isEmpty ? null : model.text,
      placeholder: const Text('选择模型'),
      constraints: const BoxConstraints(minWidth: 240),
      popupWidthConstraint: PopoverConstraint.anchorMinSize,
      popupConstraints: const BoxConstraints(maxHeight: 280, minWidth: 240),
      itemBuilder: (context, value) => Text(value),
      onChanged: (value) {
        if (value == null || value == model.text) return;
        model.text = value;
        markDirty();
      },
      popup: SelectPopup.noVirtualization(
        items: SelectItemList(
          children: [
            for (final choice in choices)
              SelectItemButton(value: choice, child: Text(choice)),
          ],
        ),
      ),
    );
  }

  Future<void> customModel() async {
    final input = TextEditingController(text: model.text);
    final result = await showOverlay<bool>(
      context,
      DialogConfiguration<bool>(
        builder: (ctx) => AlertDialog(
          title: const Text('自定义模型'),
          content: SizedBox(
            width: 380,
            child: TextField(
              controller: input,
              placeholder: const Text('输入供应商提供的模型 ID'),
            ),
          ),
          actions: [
            OutlineButton(
              onPressed: () => Navigator.of(ctx).pop(false),
              child: const Text('取消'),
            ),
            PrimaryButton(
              onPressed: () => Navigator.of(ctx).pop(true),
              child: const Text('确定'),
            ),
          ],
        ),
      ),
    ).future;
    if (result == true && mounted) model.text = input.text.trim();
    input.dispose();
  }

  Widget modelPanel() => panel(LucideIcons.slidersHorizontal, '模型与推理', [
    fieldRow(
      '模型',
      Row(
        children: [
          Expanded(child: modelSelect()),
          const Gap(8),
          OutlineButton(
            onPressed: customModel,
            leading: const Icon(LucideIcons.pencil, size: 16),
            child: const Text('自定义模型'),
          ),
        ],
      ),
    ),
    fieldRow(
      '上下文窗口',
      TextField(controller: contextWindow, placeholder: const Text('自动检测')),
    ),
    fieldRow(
      '最大输出 Token',
      TextField(controller: maxTokens, placeholder: const Text('自动检测')),
    ),
    if (provider != 'anthropic' && provider != 'legacy_ollama')
      fieldRow(
        '思考强度',
        Container(
          height: 47,
          padding: const EdgeInsets.all(4),
          decoration: BoxDecoration(
            border: Border.all(color: _border),
            borderRadius: BorderRadius.circular(10),
          ),
          child: Row(
            children: [
              for (final item in const [
                ('off', '关闭'),
                ('low', '低'),
                ('medium', '中'),
                ('high', '高'),
              ])
                Expanded(
                  child: GestureDetector(
                    onTap: () => setState(() {
                      reasoning = item.$1;
                      dirty = true;
                      testStatus = null;
                    }),
                    child: Container(
                      alignment: Alignment.center,
                      decoration: BoxDecoration(
                        color: reasoning == item.$1
                            ? _purple
                            : const Color(0x00000000),
                        borderRadius: BorderRadius.circular(7),
                      ),
                      child: Text(
                        item.$2,
                        style: TextStyle(
                          color: reasoning == item.$1
                              ? const Color(0xFFFFFFFF)
                              : _muted,
                          fontSize: 13,
                          fontWeight: FontWeight.w600,
                        ),
                      ),
                    ),
                  ),
                ),
            ],
          ),
        ),
      ),
    if (provider == 'openai' || provider == 'anthropic')
      fieldRow(
        '模型列表探测',
        Row(
          children: [
            Switch(
              value: skipModelProbe,
              onChanged: (v) => setState(() {
                skipModelProbe = v;
                dirty = true;
              }),
            ),
            const Gap(8),
            const Expanded(
              child: Text(
                '提供商不支持 /models 时开启',
                style: TextStyle(color: _muted, fontSize: 12),
              ),
            ),
          ],
        ),
      ),
  ]);

  Widget panel(IconData icon, String title, List<Widget> children) => Container(
    padding: const EdgeInsets.fromLTRB(24, 22, 24, 25),
    decoration: BoxDecoration(
      color: const Color(0xFFFFFFFF),
      border: Border.all(color: _border),
      borderRadius: BorderRadius.circular(13),
    ),
    child: Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        Row(
          children: [
            iconBox(icon),
            const Gap(13),
            Text(
              title,
              style: const TextStyle(
                color: _ink,
                fontSize: 17,
                fontWeight: FontWeight.w700,
              ),
            ),
          ],
        ),
        const Gap(4),
        ...children,
      ],
    ),
  );

  @override
  Widget build(BuildContext context) {
    final current = highlighted;
    return Padding(
      padding: const EdgeInsets.fromLTRB(20, 16, 20, 20),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          if (error != null && _dialogRefresh == null) ...[
            Text(
              error!,
              style: const TextStyle(color: Color(0xFFAD3232), fontSize: 12),
            ),
            const Gap(10),
          ],
          Expanded(
            child: Container(
              decoration: BoxDecoration(
                color: const Color(0xFFFFFFFF),
                border: Border.all(color: _border),
                borderRadius: BorderRadius.circular(13),
              ),
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.stretch,
                children: [
                  Padding(
                    padding: const EdgeInsets.fromLTRB(16, 12, 12, 12),
                    child: Row(
                      children: [
                        Image.asset(
                          'assets/brand/localsubproxy_mark.png',
                          width: 42,
                          height: 42,
                        ),
                        const Gap(12),
                        const Expanded(
                          child: Text(
                            '配置方案',
                            style: TextStyle(
                              color: _ink,
                              fontSize: 18,
                              fontWeight: FontWeight.w700,
                            ),
                          ),
                        ),
                        OutlineButton(
                          onPressed: current == null
                              ? null
                              : () => openEditor(current),
                          leading: const Icon(LucideIcons.pencil, size: 16),
                          child: const Text('编辑'),
                        ),
                        const Gap(8),
                        PrimaryButton(
                          onPressed: () => openEditor(),
                          leading: const Icon(LucideIcons.plus, size: 16),
                          child: const Text('添加方案'),
                        ),
                      ],
                    ),
                  ),
                  const Divider(),
                  Expanded(
                    child: widget.adapters.isEmpty
                        ? const Center(
                            child: Text(
                              '还没有模型方案。点击添加方案进行配置。',
                              style: TextStyle(color: _muted, fontSize: 14),
                            ),
                          )
                        : ListView.separated(
                            itemCount: widget.adapters.length,
                            separatorBuilder: (context, index) =>
                                const Divider(),
                            itemBuilder: (context, index) =>
                                schemeRow(widget.adapters[index]),
                          ),
                  ),
                ],
              ),
            ),
          ),
        ],
      ),
    );
  }
}

class _EditorRelay extends StatefulWidget {
  const _EditorRelay({required this.host});

  final _ProfileWorkspaceState host;

  @override
  State<_EditorRelay> createState() => _EditorRelayState();
}

class _EditorRelayState extends State<_EditorRelay> {
  void refresh() {
    if (mounted) setState(() {});
  }

  @override
  void initState() {
    super.initState();
    widget.host._dialogRefresh = refresh;
  }

  @override
  void dispose() {
    if (widget.host._dialogRefresh == refresh) {
      widget.host._dialogRefresh = null;
    }
    super.dispose();
  }

  @override
  Widget build(BuildContext context) => widget.host._editorDialog(context);
}
