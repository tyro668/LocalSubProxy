// ignore_for_file: curly_braces_in_flow_control_structures, implicit_call_tearoffs
import 'dart:async';
import 'dart:io';

import 'package:shadcn_flutter/shadcn_flutter.dart';

import 'native_api.dart';
import 'status_contract.dart';

const _headerHeight = 44.0;
const _purple = Color(0xFF4D25D8);
const _border = Color(0xFFDFE3ED);
const _ink = Color(0xFF202637);
const _muted = Color(0xFF6F7689);
const _soft = Color(0xFFF1ECFF);
const _green = Color(0xFF148653);

class ProfileWorkspace extends StatefulWidget {
  const ProfileWorkspace({
    super.key,
    required this.api,
    required this.adapters,
    required this.onChanged,
    this.status,
    this.onConnectionTap,
    this.onUsageTap,
  });

  final NativeApi api;
  final List<Map<String, dynamic>> adapters;
  final Future<void> Function() onChanged;
  final Map<String, dynamic>? status;
  final VoidCallback? onConnectionTap;
  final VoidCallback? onUsageTap;

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
  // false while the name is the generated "<模型>_<编号>" default, which then
  // follows the chosen model; true once the user typed a name.
  bool nameTouched = true;
  bool dirty = false;
  bool working = false;
  bool codexConnected = false;
  bool checkingCodex = false;
  bool loginPending = false;
  String? loginURL;
  String? testStatus;
  bool testPassed = false;
  bool editorOpen = false;
  String? error;
  List<String> codexModels = [];
  List<String> originalModels = [];
  String originalModel = '';
  Timer? loginPoll;
  String? highlightedID;
  final List<Map<String, dynamic>> pending = [];

  List<Map<String, dynamic>> get schemes => [...pending, ...widget.adapters];

  Map<String, dynamic>? _schemeById(String? id) {
    if (id == null) return null;
    for (final item in schemes) {
      if (item['id'] == id) return item;
    }
    return null;
  }

  Map<String, dynamic>? get selected => _schemeById(selectedID);

  Map<String, dynamic>? get highlighted => _schemeById(highlightedID);

  bool get creating => selected?['draft'] == true;
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
    model.addListener(_followModelName);
    _pickHighlight();
  }

  @override
  void didUpdateWidget(covariant ProfileWorkspace oldWidget) {
    super.didUpdateWidget(oldWidget);
    _pickHighlight();
    if (selectedID != null && _schemeById(selectedID) == null) {
      selectedID = null;
      editorOpen = false;
    }
  }

  void _pickHighlight() {
    if (_schemeById(highlightedID) != null) return;
    if (schemes.isEmpty) {
      highlightedID = null;
      return;
    }
    final defaults = schemes.where((item) => item['is_default'] == true);
    highlightedID =
        (defaults.isNotEmpty ? defaults.first : schemes.first)['id'] as String?;
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

  /// Saved scheme names except the one being edited.
  List<String> takenNames() => [
    for (final a in widget.adapters)
      if ('${a['id']}' != selectedID) '${a['name'] ?? ''}',
  ];

  String? get nameError =>
      schemeNameError(name.text, widget.adapters, exceptId: selectedID);

  void _followModelName() {
    if (nameTouched || !editorOpen) return;
    final m = model.text.trim();
    final next = m.isEmpty ? '' : defaultSchemeName(m, takenNames());
    if (name.text != next) name.text = next;
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
    highlightedID = selectedID;
    editorOpen = true;
    provider = switch (a['type']) {
      'codex_app_server' => 'codex',
      'anthropic_compatible' => 'anthropic',
      'ollama_http' => 'legacy_ollama',
      _ => 'openai',
    };
    // A new draft keeps the generated default name until the user edits it.
    nameTouched = a['draft'] != true || a['name_custom'] == true;
    name.text = nameTouched
        ? ((a['name'] as String?)?.isNotEmpty == true
              ? a['name'] as String
              : selectedID!)
        : '';
    originalModels = (a['models'] as List?)?.cast<String>() ?? [];
    model.text = originalModels.isEmpty ? '' : originalModels.first;
    _followModelName();
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

  Future<void> addScheme() async {
    if (editorOpen && !await allowDiscard() || !mounted) return;
    final draft = <String, dynamic>{
      'id': 'draft_${DateTime.now().microsecondsSinceEpoch}',
      'name': '未命名方案',
      'type': 'codex_app_server',
      'provider': 'codex',
      'models': <String>[],
      'codex_bin': 'codex',
      'enabled': true,
      'is_default': false,
      'draft': true,
    };
    setState(() {
      editorOpen = false;
      dirty = false;
      selectedID = null;
      pending.insert(0, draft);
      highlightedID = draft['id'] as String;
    });
  }

  Future<void> selectScheme(Map<String, dynamic> scheme) async {
    if (highlightedID == scheme['id'] && !editorOpen) return;
    if (editorOpen && !await allowDiscard() || !mounted) return;
    setState(() {
      highlightedID = scheme['id'] as String?;
      editorOpen = false;
      dirty = false;
      selectedID = null;
    });
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

  Future<void> openEditor(Map<String, dynamic> existing) async {
    if (!mounted ||
        (editorOpen && selectedID != existing['id'] && !await allowDiscard())) {
      return;
    }
    load(existing);
  }

  Future<void> setDefault(Map<String, dynamic> a) async {
    if (a['is_default'] == true) return;
    if (a['draft'] == true) {
      setState(() => error = '请先保存方案，再设为默认。');
      return;
    }
    if (!await allowDiscard() || !mounted) return;
    try {
      final body = adapterEditBody(a, {'is_default': true});
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
      if (a['draft'] == true) {
        setState(() {
          pending.removeWhere((item) => item['id'] == a['id']);
          if (selectedID == a['id']) {
            selectedID = null;
            editorOpen = false;
            dirty = false;
          }
          if (highlightedID == a['id']) highlightedID = null;
          _pickHighlight();
        });
        return;
      }
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
    final dup = schemeNameError(
      next,
      widget.adapters,
      exceptId: a['draft'] == true ? null : '${a['id']}',
    );
    if (dup != null) {
      setState(() => error = dup);
      return;
    }
    if (a['draft'] == true) {
      setState(() {
        a['name'] = next;
        a['name_custom'] = true;
      });
      return;
    }
    try {
      final body = adapterEditBody(a, {'name': next});
      await updateAdapterAsync(body);
      await widget.onChanged();
    } catch (e) {
      if (mounted) setState(() => error = '$e');
    }
  }

  Future<void> closeEditor() async {
    if (!await allowDiscard() || !mounted) return;
    setState(() {
      dirty = false;
      editorOpen = false;
    });
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
      'name': name.text.trim().isEmpty
          ? defaultSchemeName(chosenModel, takenNames())
          : name.text.trim(),
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
      'enabled': creating ? true : (selected?['enabled'] ?? true),
      'skip_model_probe': skipModelProbe,
    };
  }

  String? validate() {
    if (model.text.trim().isEmpty) return '请输入模型 ID。';
    final dup = nameError;
    if (dup != null) return dup;
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

  Future<void> save() async {
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
      final draftID = selectedID;
      if (creating) {
        await createAdapterAsync(draft);
        pending.removeWhere((item) => item['id'] == draftID);
      } else {
        await updateAdapterAsync(draft);
      }
      selectedID = draft['id'] as String;
      highlightedID = selectedID;
      dirty = false;
      editorOpen = false;
      await widget.onChanged();
      if (mounted) {
        setState(() => testStatus ??= '设置已保存');
      }
    } catch (e) {
      if (mounted) {
        setState(
          () => error = '$e'.contains('scheme name already exists')
              ? '保存失败：方案名称已被其他方案使用，请换一个名称'
              : '保存失败：$e',
        );
      }
    } finally {
      if (mounted) setState(() => working = false);
    }
  }

  String _providerLabel(Map<String, dynamic> adapter) =>
      switch (adapter['type']) {
        'codex_app_server' => 'OpenAI Codex（订阅）',
        'anthropic_compatible' => 'Anthropic 兼容协议',
        'ollama_http' => '旧版 Ollama',
        _ => 'OpenAI 兼容协议',
      };

  Widget _separator() => const ColoredBox(
    color: _border,
    child: SizedBox(height: 1, width: double.infinity),
  );

  Widget _statusDot(bool active, {double size = 10}) => Container(
    width: size,
    height: size,
    decoration: BoxDecoration(
      color: active ? _green : const Color(0xFF9AA2B2),
      shape: BoxShape.circle,
    ),
  );

  Widget _tableCell(String value, {bool bold = false}) => Text(
    value,
    maxLines: 1,
    overflow: TextOverflow.ellipsis,
    style: TextStyle(
      color: _ink,
      fontSize: 13,
      fontWeight: bold ? FontWeight.w600 : FontWeight.w500,
    ),
  );

  Widget _schemeRow(Map<String, dynamic> adapter) {
    final active = adapter['id'] == highlightedID;
    final name = '${adapter['name'] ?? adapter['id']}';
    return GestureDetector(
      behavior: HitTestBehavior.opaque,
      onTap: () => selectScheme(adapter),
      child: Container(
        height: 62,
        color: active ? _soft : const Color(0xFFFFFFFF),
        padding: const EdgeInsets.symmetric(horizontal: 24),
        child: Row(
          children: [
            Expanded(
              flex: 4,
              child: Row(
                children: [
                  Flexible(child: _tableCell(name, bold: true)),
                  if (adapter['is_default'] == true) ...[
                    const SizedBox(width: 8),
                    Container(
                      padding: const EdgeInsets.symmetric(
                        horizontal: 8,
                        vertical: 3,
                      ),
                      decoration: BoxDecoration(
                        color: const Color(0xFFEAE3FF),
                        borderRadius: BorderRadius.circular(5),
                      ),
                      child: const Text(
                        '默认',
                        style: TextStyle(
                          color: _purple,
                          fontSize: 11,
                          fontWeight: FontWeight.w700,
                        ),
                      ),
                    ),
                  ],
                ],
              ),
            ),
            const SizedBox(width: 12),
            Expanded(flex: 5, child: _tableCell(_providerLabel(adapter))),
            SizedBox(
              width: 40,
              child: IconButton.ghost(
                density: ButtonDensity.icon,
                icon: const Icon(LucideIcons.ellipsisVertical, size: 18),
                onPressed: () => options(adapter),
              ),
            ),
          ],
        ),
      ),
    );
  }

  Widget _leftFooter() {
    final ds = DesktopStatus.parse(widget.status);
    final paired = widget.status?['paired'] == true;
    final connected = ds.multi
        ? ds.anyConnected
        : widget.status?['connected'] == true;
    final device =
        '${widget.status?['device_name'] ?? widget.status?['device_id'] ?? '本机设备'}';
    final count = widget.status?['running_tasks'] ?? 0;
    final statusLabel = ds.multi || ds.stale
        ? ds.label
        : (connected ? '已连接' : '未连接');
    return Container(
      height: 70,
      padding: const EdgeInsets.symmetric(horizontal: 22),
      decoration: const BoxDecoration(
        color: Color(0xFFFFFFFF),
        border: Border(top: BorderSide(color: _border)),
      ),
      child: Row(
        children: [
          MouseRegion(
            cursor: SystemMouseCursors.click,
            child: GestureDetector(
              behavior: HitTestBehavior.opaque,
              onTap: widget.onConnectionTap,
              child: Container(
                padding: const EdgeInsets.symmetric(
                  horizontal: 12,
                  vertical: 7,
                ),
                decoration: BoxDecoration(
                  color: connected
                      ? const Color(0xFFECFDF5)
                      : const Color(0xFFF6F5FF),
                  border: Border.all(
                    color: connected
                        ? const Color(0xFFA7F3D0)
                        : const Color(0xFFD8CEFF),
                  ),
                  borderRadius: BorderRadius.circular(8),
                ),
                child: Row(
                  mainAxisSize: MainAxisSize.min,
                  children: [
                    _statusDot(connected),
                    const SizedBox(width: 8),
                    Text(
                      statusLabel,
                      style: TextStyle(
                        color: connected ? _green : _ink,
                        fontSize: 13,
                        fontWeight: FontWeight.w600,
                      ),
                    ),
                    const SizedBox(width: 10),
                    Container(
                      padding: const EdgeInsets.symmetric(
                        horizontal: 10,
                        vertical: 4,
                      ),
                      decoration: BoxDecoration(
                        color: paired ? const Color(0xFFFFFFFF) : _purple,
                        borderRadius: BorderRadius.circular(6),
                        border: paired
                            ? Border.all(color: const Color(0xFFD8CEFF))
                            : null,
                      ),
                      child: Row(
                        mainAxisSize: MainAxisSize.min,
                        children: [
                          Icon(
                            LucideIcons.link,
                            size: 13,
                            color: paired ? _purple : const Color(0xFFFFFFFF),
                          ),
                          const SizedBox(width: 5),
                          Text(
                            paired ? '连接详情  ›' : '立即连接  ›',
                            style: TextStyle(
                              color: paired ? _purple : const Color(0xFFFFFFFF),
                              fontSize: 12,
                              fontWeight: FontWeight.w600,
                            ),
                          ),
                        ],
                      ),
                    ),
                  ],
                ),
              ),
            ),
          ),
          const SizedBox(width: 16),
          Container(width: 1, height: 22, color: _border),
          const SizedBox(width: 16),
          const Icon(LucideIcons.monitor, size: 16, color: _muted),
          const SizedBox(width: 8),
          ConstrainedBox(
            constraints: const BoxConstraints(maxWidth: 240),
            child: Text(
              device,
              maxLines: 1,
              overflow: TextOverflow.ellipsis,
              style: const TextStyle(color: _ink, fontSize: 13),
            ),
          ),
          const SizedBox(width: 14),
          Container(width: 1, height: 22, color: _border),
          const SizedBox(width: 14),
          const Icon(LucideIcons.list, size: 16, color: _muted),
          const SizedBox(width: 7),
          Text('$count 个任务', style: const TextStyle(color: _ink, fontSize: 13)),
          const Spacer(),
          OutlineButton(
            size: ButtonSize.small,
            onPressed: widget.onUsageTap,
            leading: const Icon(LucideIcons.chartColumn, size: 14),
            child: const Text('用量统计'),
          ),
        ],
      ),
    );
  }

  Widget _leftPane() => Column(
    crossAxisAlignment: CrossAxisAlignment.stretch,
    children: [
      SizedBox(
        height: _headerHeight,
        child: Padding(
          padding: const EdgeInsets.symmetric(horizontal: 20),
          child: Row(
            children: [
              Image.asset(
                'assets/brand/localsubproxy_mark.png',
                width: 28,
                height: 28,
              ),
              const SizedBox(width: 10),
              const Expanded(
                child: Text(
                  '配置方案',
                  style: TextStyle(
                    color: _ink,
                    fontSize: 16,
                    fontWeight: FontWeight.w700,
                  ),
                ),
              ),
              PrimaryButton(
                onPressed: addScheme,
                leading: const Icon(LucideIcons.plus, size: 16),
                child: const Text('添加方案'),
              ),
            ],
          ),
        ),
      ),
      _separator(),
      Container(
        height: 46,
        padding: const EdgeInsets.symmetric(horizontal: 24),
        color: const Color(0xFFFAFBFD),
        child: Row(
          children: [
            Expanded(flex: 4, child: _tableCell('方案名称', bold: true)),
            const SizedBox(width: 12),
            Expanded(flex: 5, child: _tableCell('服务', bold: true)),
            SizedBox(width: 40, child: _tableCell('操作', bold: true)),
          ],
        ),
      ),
      _separator(),
      Expanded(
        child: schemes.isEmpty
            ? const Center(
                child: Text(
                  '还没有模型方案，点击右上角添加。',
                  style: TextStyle(color: _muted, fontSize: 13),
                ),
              )
            : ListView.separated(
                itemCount: schemes.length,
                separatorBuilder: (_, _) => _separator(),
                itemBuilder: (_, index) => _schemeRow(schemes[index]),
              ),
      ),
    ],
  );

  Widget _formField(String label, Widget child) => Padding(
    padding: const EdgeInsets.only(bottom: 18),
    child: Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        Text(
          label,
          style: const TextStyle(
            color: _ink,
            fontSize: 13,
            fontWeight: FontWeight.w600,
          ),
        ),
        const SizedBox(height: 8),
        child,
      ],
    ),
  );

  Widget _providerSelect() => Select<String>(
    value: provider,
    constraints: const BoxConstraints(minWidth: 280),
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

  Widget _modelSelect() {
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
    if (choices.isEmpty) {
      return TextField(controller: model, placeholder: const Text('输入模型 ID'));
    }
    return Select<String>(
      value: model.text.isEmpty ? null : model.text,
      placeholder: const Text('选择模型'),
      constraints: const BoxConstraints(minWidth: 230),
      popupWidthConstraint: PopoverConstraint.anchorMinSize,
      popupConstraints: const BoxConstraints(maxHeight: 280, minWidth: 230),
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

  Future<void> _customModel() async {
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

  Widget _reasoningSelect() => Container(
    height: 38,
    decoration: BoxDecoration(
      border: Border.all(color: _border),
      borderRadius: BorderRadius.circular(7),
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
              behavior: HitTestBehavior.opaque,
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
                      : const Color(0xFFFFFFFF),
                  borderRadius: BorderRadius.circular(6),
                ),
                child: Text(
                  item.$2,
                  style: TextStyle(
                    color: reasoning == item.$1
                        ? const Color(0xFFFFFFFF)
                        : _muted,
                    fontSize: 12,
                    fontWeight: FontWeight.w600,
                  ),
                ),
              ),
            ),
          ),
      ],
    ),
  );

  Widget _editorFields() => SingleChildScrollView(
    padding: const EdgeInsets.fromLTRB(26, 24, 26, 28),
    child: Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        _formField(
          '方案名称',
          Column(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              TextField(
                controller: name,
                placeholder: const Text('默认：模型名，重名时加 _2、_3'),
                onChanged: (_) => setState(() => nameTouched = true),
              ),
              if (nameError != null) ...[
                const SizedBox(height: 6),
                Text(
                  nameError!,
                  style: const TextStyle(
                    color: Color(0xFFDC2626),
                    fontSize: 12,
                  ),
                ),
              ],
            ],
          ),
        ),
        _formField('API 提供商', _providerSelect()),
        if (provider == 'codex') ...[
          Padding(
            padding: const EdgeInsets.only(bottom: 18),
            child: Row(
              children: [
                _statusDot(codexConnected),
                const SizedBox(width: 10),
                Expanded(
                  child: Text(
                    codexConnected
                        ? '本地 Codex 已连接'
                        : checkingCodex
                        ? '正在检查本机 Codex 登录态…'
                        : '本地 Codex 尚未连接',
                    style: TextStyle(
                      color: codexConnected ? _green : _muted,
                      fontSize: 12,
                      fontWeight: FontWeight.w600,
                    ),
                  ),
                ),
                GhostButton(
                  onPressed: working
                      ? null
                      : (codexConnected ? logoutCodex : connectCodex),
                  child: Text(codexConnected ? '断开连接' : '连接订阅'),
                ),
              ],
            ),
          ),
          if (loginPending) ...[
            const Text(
              '请在浏览器完成登录，然后等待自动检测。',
              style: TextStyle(color: _muted, fontSize: 12),
            ),
            if (loginURL != null)
              SelectableText(
                loginURL!,
                style: const TextStyle(color: _purple, fontSize: 11),
              ),
            Row(
              children: [
                OutlineButton(onPressed: checkLogin, child: const Text('检查登录')),
                const SizedBox(width: 8),
                OutlineButton(onPressed: cancelLogin, child: const Text('取消')),
              ],
            ),
            const SizedBox(height: 18),
          ],
          _formField(
            'Codex CLI 路径',
            TextField(controller: codexPath, placeholder: const Text('codex')),
          ),
        ] else ...[
          _formField(
            'API 地址',
            TextField(
              controller: baseURL,
              placeholder: const Text('https://example.com/v1'),
            ),
          ),
          if (provider != 'legacy_ollama')
            _formField(
              'API 密钥',
              TextField(
                controller: apiKey,
                placeholder: Text(hasReusableKey ? '已保存密钥，留空则保留' : '输入 API 密钥'),
                features: const [InputFeature.passwordToggle()],
              ),
            ),
        ],
        const SizedBox(height: 4),
        _separator(),
        const SizedBox(height: 22),
        _formField(
          '模型',
          Row(
            children: [
              Expanded(child: _modelSelect()),
              const SizedBox(width: 8),
              GhostButton(
                onPressed: _customModel,
                leading: const Icon(LucideIcons.pencil, size: 15),
                child: const Text('自定义模型'),
              ),
            ],
          ),
        ),
        _formField(
          '上下文窗口',
          TextField(controller: contextWindow, placeholder: const Text('自动检测')),
        ),
        _formField(
          '最大输出 Token',
          TextField(controller: maxTokens, placeholder: const Text('自动检测')),
        ),
        if (provider != 'anthropic' && provider != 'legacy_ollama')
          _formField('思考强度', _reasoningSelect()),
        if (error != null)
          Text(
            error!,
            style: const TextStyle(color: Color(0xFFB33535), fontSize: 12),
          ),
      ],
    ),
  );

  Widget _readOnlyField(String label, String value) => Padding(
    padding: const EdgeInsets.only(bottom: 18),
    child: Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text(
          label,
          style: const TextStyle(
            color: _muted,
            fontSize: 12,
            fontWeight: FontWeight.w600,
          ),
        ),
        const SizedBox(height: 6),
        Text(
          value.isEmpty ? '—' : value,
          style: const TextStyle(color: _ink, fontSize: 14),
        ),
      ],
    ),
  );

  Widget _detailView(Map<String, dynamic> scheme) {
    final models = (scheme['models'] as List?)?.cast<String>() ?? [];
    final modelName = models.isEmpty ? '未设置' : models.first;
    final contextWindow = (scheme['context_window'] as int? ?? 0) == 0
        ? '自动检测'
        : '${scheme['context_window']}';
    final maxTokens = (scheme['max_output_tokens'] as int? ?? 0) == 0
        ? '自动检测'
        : '${scheme['max_output_tokens']}';
    final reasoning = switch (scheme['reasoning_effort']) {
      'low' => '低',
      'medium' => '中',
      'high' => '高',
      'off' => '关闭',
      _ => '关闭',
    };
    final codex = scheme['type'] == 'codex_app_server';
    return SingleChildScrollView(
      padding: const EdgeInsets.fromLTRB(26, 24, 26, 28),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          _readOnlyField('方案名称', '${scheme['name'] ?? scheme['id']}'),
          _readOnlyField('服务', _providerLabel(scheme)),
          if (codex)
            _readOnlyField('Codex CLI 路径', '${scheme['codex_bin'] ?? 'codex'}')
          else
            _readOnlyField('API 地址', '${scheme['base_url'] ?? ''}'),
          _readOnlyField('模型', modelName),
          _readOnlyField('上下文窗口', contextWindow),
          _readOnlyField('最大输出 Token', maxTokens),
          if (scheme['type'] != 'anthropic_compatible' &&
              scheme['type'] != 'ollama_http')
            _readOnlyField('思考强度', reasoning),
        ],
      ),
    );
  }

  Widget _rightPane() => Column(
    crossAxisAlignment: CrossAxisAlignment.stretch,
    children: [
      SizedBox(
        height: _headerHeight,
        child: Padding(
          padding: const EdgeInsets.symmetric(horizontal: 20),
          child: Row(
            children: [
              Text(
                editorOpen ? '编辑方案' : '方案详情',
                style: const TextStyle(
                  color: _ink,
                  fontSize: 16,
                  fontWeight: FontWeight.w700,
                ),
              ),
              if (!editorOpen && highlighted != null) ...[
                const Spacer(),
                PrimaryButton(
                  onPressed: () => openEditor(highlighted!),
                  leading: const Icon(LucideIcons.pencil, size: 16),
                  child: const Text('编辑'),
                ),
              ],
              if (editorOpen) ...[
                const SizedBox(width: 16),
                OutlineButton(
                  onPressed: working ? null : test,
                  leading: const Icon(LucideIcons.play, size: 16),
                  child: const Text('测试连接'),
                ),
                const SizedBox(width: 8),
                _statusDot(testPassed, size: 8),
                const SizedBox(width: 6),
                Expanded(
                  child: Text(
                    testStatus ?? '尚未测试',
                    maxLines: 1,
                    overflow: TextOverflow.ellipsis,
                    style: const TextStyle(color: _muted, fontSize: 12),
                  ),
                ),
                const SizedBox(width: 8),
                OutlineButton(
                  onPressed: working ? null : closeEditor,
                  child: const Text('取消'),
                ),
                const SizedBox(width: 8),
                PrimaryButton(
                  onPressed: working ? null : save,
                  child: Text(working ? '处理中…' : '保存'),
                ),
                IconButton.ghost(
                  density: ButtonDensity.icon,
                  icon: const Icon(LucideIcons.x, size: 16),
                  onPressed: closeEditor,
                ),
              ],
            ],
          ),
        ),
      ),
      _separator(),
      Expanded(
        child: editorOpen
            ? _editorFields()
            : highlighted == null
            ? const Center(
                child: Text(
                  '选择左侧方案查看详情，或添加新方案。',
                  style: TextStyle(color: _muted, fontSize: 13),
                ),
              )
            : _detailView(highlighted!),
      ),
    ],
  );

  @override
  Widget build(BuildContext context) => LayoutBuilder(
    builder: (context, size) {
      final leftWidth = size.maxWidth * (size.maxWidth < 1100 ? 0.45 : 0.52);
      return ColoredBox(
        color: const Color(0xFFFFFFFF),
        child: Column(
          children: [
            Expanded(
              child: Row(
                children: [
                  SizedBox(width: leftWidth, child: _leftPane()),
                  const SizedBox(width: 1, child: ColoredBox(color: _border)),
                  Expanded(child: _rightPane()),
                ],
              ),
            ),
            _leftFooter(),
          ],
        ),
      );
    },
  );
}

/// Converts an AdapterView (list_adapters output) into an edit body. The view
/// carries computed, display-only values (effective capabilities incl. param
/// markers, effective concurrency, health…); only the stored configuration
/// may be sent back, so the kernel never persists derived data.
Map<String, dynamic> adapterEditBody(
  Map<String, dynamic> view,
  Map<String, dynamic> changes,
) {
  const editable = {
    'id',
    'type',
    'name',
    'provider',
    'base_url',
    'codex_bin',
    'models',
    'context_window',
    'max_output_tokens',
    'reasoning_effort',
    'is_default',
    'enabled',
    'skip_model_probe',
  };
  final body = <String, dynamic>{
    for (final e in view.entries)
      if (editable.contains(e.key)) e.key: e.value,
  };
  if (view.containsKey('configured_capabilities')) {
    body['capabilities'] = view['configured_capabilities'] ?? <String>[];
  }
  if (view.containsKey('configured_concurrency')) {
    body['concurrency'] = view['configured_concurrency'] ?? 0;
  }
  body.addAll(changes);
  return body;
}
