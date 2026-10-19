// ignore_for_file: curly_braces_in_flow_control_structures, implicit_call_tearoffs
import 'dart:async';
import 'dart:io';

import 'package:shadcn_flutter/shadcn_flutter.dart';
import 'app_dialog.dart';
import 'backend_catalog.dart';
import 'model_capabilities.dart';

import 'native_api.dart';
import 'status_contract.dart';
import 'tokens.dart';
import 'ui_kit.dart';

/// Result of the last connection test of a scheme (this session only).
class SchemeTest {
  const SchemeTest(this.ok, this.ms, this.at, this.message, this.models);
  final bool ok;
  final int ms;
  final DateTime at;
  final String message;
  final List<String> models;
}

class ProfileWorkspace extends StatefulWidget {
  const ProfileWorkspace({
    super.key,
    required this.api,
    required this.adapters,
    required this.onChanged,
    this.status,
    this.onConnectionTap,
    this.onUsageTap,
    this.usage = const {},
  });

  /// Total tokens of the last 7 days per scheme id (usage_stats by adapter).
  final Map<String, int> usage;

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
  final cliPath = TextEditingController(text: 'codex');
  final contextWindow = TextEditingController();
  final maxTokens = TextEditingController();
  String? selectedID;
  String provider = 'codex';
  String reasoning = '';
  String reasoningModel = '';
  List<ModelCapabilities> modelCatalog = [];
  (String?, String, String)? catalogSource;
  int catalogRequest = 0;
  bool isDefault = false;
  bool skipModelProbe = false;
  // A new draft gets a name once, from user input or a committed model choice.
  // Later model changes never rename the scheme.
  bool nameAssigned = true;
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
  // Models fetched from a provider (GET /models or /api/tags) for the picker.
  List<String> providerModels = [];
  bool fetchingModels = false;
  List<String> originalModels = [];
  String originalModel = '';
  Timer? loginPoll;
  String? highlightedID;
  final List<Map<String, dynamic>> pending = [];
  final Map<String, SchemeTest> lastTests = {};
  SchemeTest? editorTest;
  bool advancedOpen = false;

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
  BackendProfile get backend => backendForProvider(provider);
  bool get isCLI => backend.isCLI;
  String get type => backend.type;
  String get executable => cliPath.text.trim().isEmpty
      ? (backend.executable ?? '')
      : cliPath.text.trim();

  (String?, String, String) get discoverySource =>
      (selectedID, provider, executable);

  ModelCapabilities? get currentModelCapabilities {
    if (catalogSource != discoverySource) return null;
    for (final m in modelCatalog) {
      if (m.id == model.text.trim()) return m;
    }
    return null;
  }

  List<EffortOption> get effortOptions => [
    const EffortOption(''),
    if (!isCLI) ...const [
      EffortOption('off'),
      EffortOption('low'),
      EffortOption('medium'),
      EffortOption('high'),
    ] else if (currentModelCapabilities?.known == true)
      ...currentModelCapabilities!.efforts,
  ];

  String? get effortIssue {
    if (!isCLI || reasoning.isEmpty) return null;
    final info = currentModelCapabilities;
    if (info?.known == true && info!.option(reasoning) == null) {
      return '该模型不支持已配置的思考档位，请选择默认或其他可用档位。';
    }
    return null;
  }

  void syncModelEffort() {
    if (reasoningModel == model.text.trim()) return;
    reasoningModel = model.text.trim();
    final info = currentModelCapabilities;
    // Default always follows the selected model. Explicit choices follow a
    // pinned variant, or reset when the new model cannot support the old level.
    if (isCLI && reasoning.isNotEmpty) {
      reasoning = info?.hasVariants == true
          ? info!.defaultEffort
          : (info?.option(reasoning) != null ? reasoning : '');
    }
  }

  void selectEffort(String value) {
    setState(() {
      reasoning = value;
      final variant = currentModelCapabilities?.option(value)?.model ?? '';
      if (variant.isNotEmpty) {
        model.text = variant;
        reasoningModel = variant;
        _fillNameFromModel();
      }
      dirty = true;
      testStatus = null;
      editorTest = null;
    });
  }

  @override
  void initState() {
    super.initState();
    for (final c in [
      name,
      model,
      baseURL,
      apiKey,
      cliPath,
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
      cliPath,
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

  void _fillNameFromModel() {
    final previousEffort = reasoning;
    syncModelEffort();
    if (mounted && previousEffort != reasoning) setState(() {});
    if (!creating || nameAssigned || !editorOpen) return;
    final chosen = model.text.trim();
    if (chosen.isEmpty || name.text.trim().isNotEmpty) return;
    nameAssigned = true;
    name.text = defaultSchemeName(chosen, takenNames());
  }

  void _selectModel(String value) {
    setState(() {
      model.text = value;
      _fillNameFromModel();
    });
  }

  void markDirty() {
    if (!mounted) return;
    setState(() {
      dirty = true;
      testStatus = null;
      editorTest = null;
    });
  }

  void load(Map<String, dynamic> a) {
    selectedID = a['id'] as String;
    highlightedID = selectedID;
    editorOpen = true;
    provider = backendForType(a['type'] as String?).provider;
    // Untitled drafts start empty; saved or manually renamed schemes keep their name.
    nameAssigned = a['draft'] != true || a['name_custom'] == true;
    name.text = nameAssigned
        ? ((a['name'] as String?)?.isNotEmpty == true
              ? a['name'] as String
              : selectedID!)
        : '';
    originalModels = (a['models'] as List?)?.cast<String>() ?? [];
    model.text = originalModels.isEmpty ? '' : originalModels.first;
    originalModel = model.text;
    baseURL.text = a['base_url'] as String? ?? '';
    apiKey.clear();
    cliPath.text =
        (provider == 'codex' ? a['codex_bin'] : a['executable']) as String? ??
        backend.executable ??
        '';
    contextWindow.text = (a['context_window'] as int? ?? 0) == 0
        ? ''
        : '${a['context_window']}';
    maxTokens.text = (a['max_output_tokens'] as int? ?? 0) == 0
        ? ''
        : '${a['max_output_tokens']}';
    reasoning = normalizeEffort(provider, a['reasoning_effort'] as String?);
    reasoningModel = model.text;
    modelCatalog = [];
    catalogSource = null;
    catalogRequest++;
    fetchingModels = false;
    checkingCodex = false;
    codexModels = [];
    isDefault = a['is_default'] == true;
    skipModelProbe = a['skip_model_probe'] == true;
    dirty = false;
    testStatus = null;
    editorTest = null;
    advancedOpen = false;
    error = null;
    providerModels = [];
    codexConnected = a['auth_state'] != 'required' && a['health'] == 'ready';
    if (mounted) setState(() {});
    if (isCLI) fetchModels();
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
    load(draft);
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

  Future<bool> confirm(
    String title,
    String message,
    String action, {
    bool destructive = false,
    Widget? content,
    double maxWidth = 440,
  }) async {
    final result = await showOverlay<bool>(
      context,
      DialogConfiguration<bool>(
        builder: (ctx) => AppDialog(
          title: Text(title),
          maxWidth: maxWidth,
          content: content ?? Text(message),
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
    return result == true;
  }

  /// Connections whose allow list covers any model of scheme [a].
  List<ConnectionInfo> affectedConnections(Map<String, dynamic> a) {
    final models = (a['models'] as List?)?.cast<String>() ?? const <String>[];
    return [
      for (final c in DesktopStatus.parse(widget.status).connections)
        if (models.any((m) => c.allows('${a['id']}/$m'))) c,
    ];
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
          '',
          '删除方案',
          destructive: true,
          content: DeleteSchemeMessage(
            name: '${a['name'] ?? a['id']}',
            affected: [for (final c in affectedConnections(a)) c.title],
          ),
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

  /// ⋮ menu anchored to the row button (not a modal dialog).
  Future<void> options(Map<String, dynamic> a, BuildContext anchor) async {
    String? picked;
    await showDropdown<void>(
      context: anchor,
      alignment: Alignment.topRight,
      anchorAlignment: Alignment.bottomRight,
      offset: const Offset(0, 4),
      builder: (ctx) => SchemeMenu(
        onSelected: (v) {
          picked = v;
          closeOverlay(ctx);
        },
      ),
    ).future;
    if (!mounted) return;
    switch (picked) {
      case 'rename':
        await renameProfile(a);
      case 'default':
        await setDefault(a);
      case 'test':
        await testSaved(a);
      case 'delete':
        await deleteProfile(a);
    }
  }

  /// 测试连接 from the menu: tests the stored configuration of a scheme.
  Future<void> testSaved(Map<String, dynamic> a) async {
    if (a['draft'] == true) {
      openEditor(a);
      return;
    }
    final sw = Stopwatch()..start();
    try {
      final r = await testAdapterAsync(adapterEditBody(a, {}));
      if (!mounted) return;
      setState(() => lastTests['${a['id']}'] = _record(r, sw));
    } catch (e) {
      if (mounted) {
        setState(
          () => lastTests['${a['id']}'] = SchemeTest(
            false,
            sw.elapsedMilliseconds,
            DateTime.now(),
            '$e',
            const [],
          ),
        );
      }
    }
  }

  SchemeTest _record(Map<String, dynamic> r, Stopwatch sw) {
    final ok = r['health'] == 'ready';
    final models = ((r['models'] as List?) ?? const [])
        .map((e) => '$e')
        .toList();
    return SchemeTest(
      ok,
      sw.elapsedMilliseconds,
      DateTime.now(),
      ok
          ? (models.isEmpty
                ? '模型可用'
                : '发现 ${models.length} 个模型：${models.join('、')}')
          : (r['error'] is String && (r['error'] as String).isNotEmpty
                ? r['error'] as String
                : r['auth_state'] == 'required'
                ? '需要登录'
                : '请检查连接配置、登录状态和模型 ID'),
      models,
    );
  }

  Future<void> renameProfile(Map<String, dynamic> a) async {
    final input = TextEditingController(text: '${a['name'] ?? a['id']}');
    final result = await showOverlay<bool>(
      context,
      DialogConfiguration<bool>(
        builder: (ctx) => AppDialog(
          title: const Text('重命名方案'),
          content: SizedBox(
            width: 360,
            child: Column(
              mainAxisSize: MainAxisSize.min,
              crossAxisAlignment: CrossAxisAlignment.stretch,
              children: [
                appTextField(context, controller: input),
                const SizedBox(height: 4),
                Text(
                  '名称不区分大小写，不能与其他方案重复',
                  style: AppText.xs.copyWith(color: AppTokens.of(context).fg3),
                ),
              ],
            ),
          ),
          actions: [
            AppButton(
              label: '取消',
              onPressed: () => Navigator.of(ctx).pop(false),
            ),
            AppButton(
              label: '确定',
              variant: ButtonVariant.primary,
              onPressed: () => Navigator.of(ctx).pop(true),
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
      cliPath.text = backend.executable ?? '';
      baseURL.text = value == 'ollama' ? 'http://127.0.0.1:11434' : '';
      model.clear();
      reasoning = '';
      reasoningModel = '';
      modelCatalog = [];
      codexModels = [];
      catalogSource = null;
      catalogRequest++;
      fetchingModels = false;
      checkingCodex = false;
      skipModelProbe = false;
      apiKey.clear();
      testStatus = null;
      providerModels = [];
      dirty = true;
    });
    if (provider == 'codex') refreshCodex();
  }

  /// Discover models using the configured CLI or provider endpoint.
  Future<void> fetchModels() async {
    if (isCLI) return fetchCatalog();
    if (!isCLI && baseURL.text.trim().isEmpty) {
      setState(() => error = '请先填写 API 地址。');
      return;
    }
    if (!isCLI &&
        provider != 'ollama' &&
        apiKey.text.trim().isEmpty &&
        !hasReusableKey) {
      setState(() => error = '请先填写 API 密钥。');
      return;
    }
    setState(() {
      fetchingModels = true;
      error = null;
    });
    final requestedProvider = provider;
    final requestedID = selectedID;
    final requestedExecutable = executable;
    try {
      final list = await providerModelsAsync({
        'id': selectedID ?? '',
        'type': type,
        if (isCLI) 'executable': executable,
        'base_url': baseURL.text.trim(),
        'api_key': apiKey.text.trim(),
      });
      if (!mounted ||
          provider != requestedProvider ||
          selectedID != requestedID ||
          executable != requestedExecutable)
        return;
      setState(() {
        providerModels = list;
        if (list.isEmpty) error = '服务商没有返回模型，请使用自定义模型。';
      });
    } catch (e) {
      if (mounted && provider == requestedProvider && selectedID == requestedID)
        setState(() => error = '获取模型失败：$e');
    } finally {
      if (mounted) setState(() => fetchingModels = false);
    }
  }

  Future<void> refreshCodex() async {
    if (provider == 'codex') await fetchCatalog();
  }

  Future<void> fetchCatalog() async {
    final source = discoverySource;
    final request = ++catalogRequest;
    final requestedProvider = provider;
    setState(() {
      if (provider == 'codex') {
        checkingCodex = true;
      } else {
        fetchingModels = true;
      }
      error = null;
    });
    bool current() =>
        mounted && request == catalogRequest && source == discoverySource;
    try {
      final data = await providerCatalogAsync({
        'id': selectedID ?? '',
        'type': type,
        if (provider == 'codex')
          'codex_bin': executable
        else
          'executable': executable,
      });
      if (!current()) return;
      final catalog = data.map(ModelCapabilities.fromJson).toList();
      setState(() {
        modelCatalog = catalog;
        catalogSource = source;
        final ids = catalog.map((m) => m.id).toList();
        if (provider == 'codex') {
          codexModels = ids;
          codexConnected = true;
        } else {
          providerModels = ids;
        }
        // Existing variant/effort mismatches are resolved just as at execution.
        // Discovery never selects an initial model or assigns a scheme name.
        final variant =
            currentModelCapabilities?.option(reasoning)?.model ?? '';
        if (variant.isNotEmpty && variant != model.text) {
          model.text = variant;
          reasoningModel = variant;
        }
      });
    } catch (e) {
      if (current()) {
        setState(() {
          modelCatalog = [];
          catalogSource = null;
          if (provider == 'codex') {
            codexConnected = false;
            codexModels = [];
          } else {
            providerModels = [];
            error = '获取模型失败：$e';
          }
        });
      }
    } finally {
      if (mounted && request == catalogRequest) {
        setState(() {
          if (requestedProvider == 'codex') {
            checkingCodex = false;
          } else {
            fetchingModels = false;
          }
        });
      }
    }
  }

  Future<void> connectCodex() async {
    setState(() {
      working = true;
      error = null;
    });
    try {
      final url = await startCodexLoginAsync(
        cliPath.text.trim().isEmpty ? 'codex' : cliPath.text.trim(),
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
      }
    } catch (e) {
      if (mounted) setState(() => error = '检查订阅登录失败：$e');
    } finally {
      checkingCodex = false;
    }
    if (mounted && !loginPending && codexConnected) await refreshCodex();
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
          destructive: true,
        ) ||
        !mounted)
      return;
    setState(() => working = true);
    try {
      await logoutCodexAsync(
        cliPath.text.trim().isEmpty ? 'codex' : cliPath.text.trim(),
      );
      if (mounted)
        setState(() {
          codexConnected = false;
          codexModels = [];
          modelCatalog = [];
          catalogSource = null;
          catalogRequest++;
          checkingCodex = false;
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
      'name': name.text.trim(),
      'type': type,
      'provider':
          selected != null &&
              selected?['type'] == type &&
              selected?['base_url'] == baseURL.text.trim()
          ? (selected?['provider'] ?? provider)
          : provider,
      'models': models,
      'base_url': isCLI ? '' : baseURL.text.trim(),
      'api_key': apiKey.text.trim(),
      if (provider == 'codex') 'codex_bin': executable,
      if (isCLI && provider != 'codex') 'executable': executable,
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
    _fillNameFromModel(); // Testing/saving also commits a manually entered model.
    if (name.text.trim().isEmpty) return '请输入方案名称。';
    final dup = nameError;
    if (dup != null) return dup;
    if (contextWindow.text.trim().isNotEmpty &&
        (int.tryParse(contextWindow.text.trim()) ?? -1) < 1)
      return '上下文窗口必须是正整数。';
    if (maxTokens.text.trim().isNotEmpty &&
        (int.tryParse(maxTokens.text.trim()) ?? -1) < 1)
      return '最大输出 Token 必须是正整数。';
    if (effortIssue != null) return effortIssue;
    if (!isCLI && baseURL.text.trim().isEmpty) return '请输入 API 根地址。';
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
    final sw = Stopwatch()..start();
    try {
      final draft = body();
      final result = await testAdapterAsync(draft);
      if (!mounted) return;
      setState(() {
        editorTest = _record(result, sw);
        if (selectedID != null && !creating) {
          lastTests[selectedID!] = editorTest!;
        }
        testPassed = result['health'] == 'ready';
        testStatus = testPassed
            ? '连接成功，模型可用'
            : '连接不可用：${result['auth_state'] == 'required' ? '需要登录' : '请检查地址、密钥和模型 ID'}';
      });
    } catch (e) {
      if (mounted)
        setState(() {
          testStatus = '连接测试失败';
          editorTest = SchemeTest(
            false,
            sw.elapsedMilliseconds,
            DateTime.now(),
            '$e',
            const [],
          );
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
        'grok_build' => 'Grok Build（CLI）',
        'antigravity_cli' => 'Antigravity CLI',
        'anthropic_compatible' => 'Anthropic 兼容协议',
        'ollama_http' => 'Ollama 协议（本地模型）',
        _ => 'OpenAI 兼容协议',
      };

  AppTokens get t => AppTokens.of(context);

  Widget _schemeRow(Map<String, dynamic> adapter) {
    final active = adapter['id'] == highlightedID;
    final h = schemeHealth(adapter);
    final editingDraft =
        adapter['draft'] == true && editorOpen && selectedID == adapter['id'];
    final name = editingDraft && name_.isNotEmpty
        ? name_
        : '${adapter['name'] ?? adapter['id']}';
    final off = adapter['enabled'] != true;
    return GestureDetector(
      behavior: HitTestBehavior.opaque,
      onTap: () => selectScheme(adapter),
      child: Container(
        key: ValueKey('scheme-row-${adapter['id']}'),
        height: AppSize.row,
        decoration: BoxDecoration(
          color: active ? t.brand50 : t.surface,
          border: Border(
            left: BorderSide(color: active ? t.brand : t.transparent, width: 3),
            bottom: BorderSide(color: t.border),
          ),
        ),
        padding: const EdgeInsets.fromLTRB(21, 0, 24, 0),
        child: Opacity(
          opacity: off ? 0.55 : 1,
          child: Row(
            children: [
              Expanded(
                flex: 4,
                child: Row(
                  children: [
                    StatusDot(h.dot),
                    const SizedBox(width: 8),
                    Flexible(
                      child: Text(
                        name,
                        maxLines: 1,
                        overflow: TextOverflow.ellipsis,
                        style: AppText.sm.copyWith(
                          color: t.fg,
                          fontWeight: AppText.medium,
                        ),
                      ),
                    ),
                    if (adapter['is_default'] == true) ...[
                      const SizedBox(width: 8),
                      const StatusBadge('默认', tone: Tone.brand),
                    ],
                    if (h.badge != null) ...[
                      const SizedBox(width: 8),
                      StatusBadge(h.badge!, tone: h.badgeTone),
                    ],
                    if (adapter['draft'] == true) ...[
                      const SizedBox(width: 8),
                      const StatusBadge('草稿'),
                    ],
                  ],
                ),
              ),
              const SizedBox(width: 12),
              Expanded(
                flex: 5,
                child: Row(
                  children: [
                    TypeMark(adapter['type'] as String?),
                    const SizedBox(width: 8),
                    Flexible(
                      child: Text(
                        _providerLabel(adapter),
                        maxLines: 1,
                        overflow: TextOverflow.ellipsis,
                        style: AppText.sm.copyWith(color: t.fg2),
                      ),
                    ),
                  ],
                ),
              ),
              Builder(
                builder: (anchor) => AppIconButton(
                  key: ValueKey('scheme-menu-${adapter['id']}'),
                  icon: LucideIcons.ellipsisVertical,
                  tooltip: '更多操作',
                  onPressed: () => options(adapter, anchor),
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }

  String get name_ => name.text.trim();

  Widget _statusBar() {
    final ds = DesktopStatus.parse(widget.status);
    final paired = ds.paired;
    final bar = barStatus(ds);
    final tone = toneOf(bar.level);
    final c = ToneColors.of(context, tone);
    final device =
        '${widget.status?['device_name'] ?? widget.status?['device_id'] ?? '本机设备'}';
    final count = ds.stale ? '—' : '${widget.status?['running_tasks'] ?? 0}';
    final pillFg = tone == Tone.neutral ? t.neutral : c.fg;
    return Container(
      key: const ValueKey('status-bar'),
      height: AppSize.statusBar,
      padding: const EdgeInsets.symmetric(horizontal: AppSpace.page),
      decoration: BoxDecoration(
        color: t.surface,
        border: Border(top: BorderSide(color: t.border)),
      ),
      child: Row(
        children: [
          MouseRegion(
            cursor: SystemMouseCursors.click,
            child: GestureDetector(
              behavior: HitTestBehavior.opaque,
              onTap: widget.onConnectionTap,
              child: Container(
                key: ValueKey('status-pill-${bar.level.name}'),
                height: 32,
                padding: const EdgeInsets.symmetric(horizontal: 12),
                decoration: BoxDecoration(
                  color: c.bg,
                  border: Border.all(color: c.border),
                  borderRadius: BorderRadius.circular(AppRadius.control),
                ),
                child: Row(
                  mainAxisSize: MainAxisSize.min,
                  children: [
                    if (bar.level == StatusLevel.warn ||
                        bar.level == StatusLevel.error)
                      Icon(LucideIcons.triangleAlert, size: 16, color: pillFg)
                    else
                      StatusDot(tone),
                    const SizedBox(width: 8),
                    Text(
                      bar.label,
                      style: AppText.sm.copyWith(
                        color: pillFg,
                        fontWeight: AppText.semibold,
                      ),
                    ),
                    const SizedBox(width: 8),
                    Container(
                      width: 1,
                      height: 16,
                      color: pillFg.withValues(alpha: 0.25),
                    ),
                    const SizedBox(width: 8),
                    Text(
                      paired ? '连接详情 ›' : '立即连接 ›',
                      style: AppText.sm.copyWith(
                        color: pillFg,
                        fontWeight: AppText.medium,
                      ),
                    ),
                  ],
                ),
              ),
            ),
          ),
          const SizedBox(width: 16),
          const Hairline(vertical: true, length: 20),
          const SizedBox(width: 16),
          Icon(LucideIcons.monitor, size: 16, color: t.fg2),
          const SizedBox(width: 6),
          ConstrainedBox(
            constraints: const BoxConstraints(maxWidth: 240),
            child: Text(
              device,
              maxLines: 1,
              overflow: TextOverflow.ellipsis,
              style: AppText.sm.copyWith(color: t.fg2),
            ),
          ),
          const SizedBox(width: 16),
          const Hairline(vertical: true, length: 20),
          const SizedBox(width: 16),
          Icon(LucideIcons.list, size: 16, color: t.fg2),
          const SizedBox(width: 6),
          Text('$count 个任务', style: AppText.sm.copyWith(color: t.fg2)),
          const Spacer(),
          AppButton(
            label: '用量统计',
            icon: LucideIcons.chartColumn,
            variant: ButtonVariant.ghost,
            onPressed: widget.onUsageTap,
          ),
        ],
      ),
    );
  }

  Widget _paneHeader({
    required Widget title,
    List<Widget> actions = const [],
  }) => Container(
    height: AppSize.header,
    padding: const EdgeInsets.symmetric(horizontal: AppSpace.page),
    decoration: BoxDecoration(
      border: Border(bottom: BorderSide(color: t.border)),
    ),
    child: Row(
      children: [
        Expanded(child: title),
        for (final a in actions) ...[const SizedBox(width: 12), a],
      ],
    ),
  );

  Widget _title(String s) => Text(s, style: AppText.lg.copyWith(color: t.fg));

  Widget _leftPane() => Column(
    crossAxisAlignment: CrossAxisAlignment.stretch,
    children: [
      _paneHeader(
        title: Row(
          children: [
            ClipRRect(
              borderRadius: BorderRadius.circular(AppRadius.control),
              child: Image.asset(
                'assets/brand/avora_mark.png',
                width: 28,
                height: 28,
              ),
            ),
            const SizedBox(width: 12),
            Flexible(child: _title('配置方案')),
          ],
        ),
        actions: [
          AppButton(
            label: '添加方案',
            icon: LucideIcons.plus,
            variant: ButtonVariant.primary,
            onPressed: addScheme,
          ),
        ],
      ),
      if (schemes.isEmpty)
        Expanded(
          child: EmptyState(
            icon: LucideIcons.layers,
            title: '添加第一个模型方案',
            message: '方案描述本机可用的 CLI、Ollama 模型或兼容 API。远端服务只能调用你授权的方案。',
            action: AppButton(
              key: const ValueKey('empty-add-scheme'),
              label: '添加方案',
              icon: LucideIcons.plus,
              variant: ButtonVariant.primary,
              onPressed: addScheme,
            ),
          ),
        )
      else ...[
        Container(
          height: AppSize.tableHeader,
          padding: const EdgeInsets.fromLTRB(24, 0, 24, 0),
          decoration: BoxDecoration(
            color: t.subtle,
            border: Border(bottom: BorderSide(color: t.border)),
          ),
          child: Row(
            children: [
              Expanded(flex: 4, child: _th('方案名称')),
              const SizedBox(width: 12),
              Expanded(flex: 5, child: _th('服务')),
              const SizedBox(width: AppSize.button),
            ],
          ),
        ),
        Expanded(
          child: ListView.builder(
            itemCount: schemes.length,
            itemBuilder: (_, index) => _schemeRow(schemes[index]),
          ),
        ),
      ],
    ],
  );

  Widget _th(String s) => Text(
    s,
    style: AppText.xs.copyWith(color: t.fg2, fontWeight: AppText.medium),
  );

  Widget _field(String label, Widget child, {String? hint, String? error}) =>
      Padding(
        padding: const EdgeInsets.only(bottom: 16),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Text(
              label,
              style: AppText.sm.copyWith(
                color: t.fg,
                fontWeight: AppText.medium,
              ),
            ),
            const SizedBox(height: 6),
            child,
            if (error != null) ...[
              const SizedBox(height: 4),
              Text(error, style: AppText.xs.copyWith(color: t.danger)),
            ] else if (hint != null) ...[
              const SizedBox(height: 4),
              Text(hint, style: AppText.xs.copyWith(color: t.fg3)),
            ],
          ],
        ),
      );

  Widget _providerSelect() => _fieldSelect(
    Select<String>(
      expandIcon: Icon(
        LucideIcons.chevronDown,
        size: 16,
        color: AppTokens.of(context).fg2,
      ),
      value: provider,
      constraints: const BoxConstraints(minHeight: AppSize.input),
      padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 7),
      borderRadius: BorderRadius.circular(AppRadius.control),
      popupWidthConstraint: PopoverConstraint.anchorMinSize,
      itemBuilder: (context, value) => Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          TypeMark(backendForProvider(value).type),
          const SizedBox(width: 8),
          Text(
            backendForProvider(value).label,
            style: AppText.md.copyWith(color: t.fg),
          ),
        ],
      ),
      onChanged: changeProvider,
      popup: SelectPopup(
        items: SelectItemList(
          children: [
            for (final item in backendProfiles)
              SelectItemButton(value: item.provider, child: Text(item.label)),
          ],
        ),
      ),
    ),
  );

  static const _customItem = '__custom_model__';

  List<String> get modelChoices => isCLI && catalogSource != discoverySource
      ? []
      : provider == 'codex'
      ? codexModels
      : <String>{...providerModels, ...?editorTest?.models}.toList();

  /// Selects drawn like the text fields: white surface, hairline border
  /// (the default outline style tints the background grey).
  Widget _fieldSelect(Widget select) {
    final theme = Theme.of(context);
    final t = AppTokens.of(context);
    return Theme(
      data: theme.copyWith(
        colorScheme: () => theme.colorScheme.copyWith(
          input: () => t.surface,
          border: () => t.checkboxBorder,
        ),
      ),
      child: select,
    );
  }

  Widget _modelSelect() {
    final choices = <String>{
      if (model.text.isNotEmpty) model.text,
      ...modelChoices,
    }.toList();
    if (modelChoices.isEmpty) {
      return Focus(
        onFocusChange: (focused) {
          if (!focused && mounted) _fillNameFromModel();
        },
        child: appTextField(
          context,
          controller: model,
          onSubmitted: (_) => _fillNameFromModel(),
          placeholder: switch (provider) {
            'codex' => '输入模型 ID',
            'ollama' => '输入本地模型 ID（如 qwen3:8b）',
            _ => '输入供应商提供的模型 ID',
          },
        ),
      );
    }
    return _fieldSelect(
      Select<String>(
        expandIcon: Icon(
          LucideIcons.chevronDown,
          size: 16,
          color: AppTokens.of(context).fg2,
        ),
        value: model.text.isEmpty ? null : model.text,
        placeholder: const Text('选择模型'),
        constraints: const BoxConstraints(minHeight: AppSize.input),
        padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 7),
        borderRadius: BorderRadius.circular(AppRadius.control),
        popupWidthConstraint: PopoverConstraint.anchorMinSize,
        popupConstraints: const BoxConstraints(maxHeight: 280, minWidth: 230),
        itemBuilder: (context, value) => Text(
          value == _customItem ? '+ 自定义模型…' : value,
          style: AppText.md.copyWith(color: t.fg),
        ),
        onChanged: (value) {
          if (value == null || value == model.text) return;
          if (value == _customItem) {
            _customModel();
            return;
          }
          _selectModel(value);
        },
        popup: SelectPopup.noVirtualization(
          items: SelectItemList(
            children: [
              for (final choice in choices)
                SelectItemButton(value: choice, child: Text(choice)),
              const SelectItemButton(
                value: _customItem,
                child: Text('+ 自定义模型…'),
              ),
            ],
          ),
        ),
      ),
    );
  }

  Future<void> _customModel() async {
    final input = TextEditingController(text: model.text);
    final result = await showOverlay<bool>(
      context,
      DialogConfiguration<bool>(
        builder: (ctx) => AppDialog(
          title: const Text('自定义模型'),
          content: SizedBox(
            width: 380,
            child: appTextField(
              ctx,
              controller: input,
              placeholder: '输入供应商提供的模型 ID',
            ),
          ),
          actions: [
            AppButton(
              label: '取消',
              onPressed: () => Navigator.of(ctx).pop(false),
            ),
            AppButton(
              label: '确定',
              variant: ButtonVariant.primary,
              onPressed: () => Navigator.of(ctx).pop(true),
            ),
          ],
        ),
      ),
    ).future;
    if (result == true && mounted) _selectModel(input.text.trim());
    input.dispose();
  }

  Widget _reasoningSelect() {
    final info = currentModelCapabilities;
    final hint =
        effortIssue ??
        (isCLI
            ? (info?.known != true
                  ? (reasoning.isEmpty
                        ? '获取模型能力后可选择支持的档位；默认由模型决定。'
                        : '保留已配置档位（${reasoningLabel(reasoning)}）；获取模型能力后可确认支持情况。')
                  : info!.efforts.isEmpty
                  ? '该模型未提供可调整的思考档位，使用模型默认行为。'
                  : info.hasVariants
                  ? '选择档位会同步切换模型变体；默认沿用当前变体。'
                  : '默认由模型决定${info.defaultEffort.isEmpty ? '' : '（${reasoningLabel(info.defaultEffort)}）'}。')
            : '默认不覆盖服务商设置；其他档位需服务商支持。');
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Wrap(
          spacing: 4,
          runSpacing: 4,
          children: [
            for (final option in effortOptions)
              GestureDetector(
                key: ValueKey(
                  'effort-${option.value.isEmpty ? 'default' : option.value}',
                ),
                behavior: HitTestBehavior.opaque,
                onTap: () => selectEffort(option.value),
                child: Container(
                  padding: const EdgeInsets.symmetric(
                    horizontal: 12,
                    vertical: 8,
                  ),
                  decoration: BoxDecoration(
                    color: reasoning == option.value ? t.brand : t.surface,
                    border: Border.all(color: t.border),
                    borderRadius: BorderRadius.circular(AppRadius.control),
                  ),
                  child: Text(
                    reasoningLabel(option.value),
                    style: AppText.xs.copyWith(
                      color: reasoning == option.value ? t.brandFg : t.fg2,
                      fontWeight: AppText.medium,
                    ),
                  ),
                ),
              ),
          ],
        ),
        const SizedBox(height: 6),
        Text(hint, style: AppText.xs.copyWith(color: t.fg2)),
      ],
    );
  }

  String get _advancedSummary {
    String v(TextEditingController c) =>
        c.text.trim().isEmpty ? '自动' : c.text.trim();
    final parts = ['上下文窗口 ${v(contextWindow)}', '最大输出 ${v(maxTokens)}'];
    if (provider != 'anthropic' && provider != 'ollama') {
      parts.add('思考强度 ${_reasoningLabel(reasoning)}');
    }
    return parts.join(' · ');
  }

  static String _reasoningLabel(String? r) => reasoningLabel(r);

  Widget _testAlert() {
    final r = editorTest;
    if (r == null) return const SizedBox.shrink();
    final failedCall = !r.ok && r.models.isEmpty && testStatus == '连接测试失败';
    return Padding(
      padding: const EdgeInsets.only(bottom: 16),
      child: AppAlert(
        key: const ValueKey('test-result'),
        tone: r.ok ? Tone.success : Tone.danger,
        inline: true,
        title: r.ok ? '连接测试通过' : (failedCall ? '连接测试失败' : '连接不可用'),
        message: '${r.ms} ms · ${r.message}',
      ),
    );
  }

  Widget _editorFields() {
    final codex = provider == 'codex';
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        Expanded(
          child: SingleChildScrollView(
            padding: const EdgeInsets.fromLTRB(24, 16, 24, 16),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.stretch,
              children: [
                _testAlert(),
                if (error != null)
                  Padding(
                    padding: const EdgeInsets.only(bottom: 16),
                    child: AppAlert(
                      key: const ValueKey('editor-error'),
                      tone: Tone.danger,
                      message: error,
                    ),
                  ),
                const SectionLabel('基本信息', top: 0),
                Row(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    Expanded(
                      child: _field(
                        '方案名称',
                        appTextField(
                          context,
                          controller: name,
                          placeholder: '输入名称，或选定模型后自动填充',
                          onChanged: (value) {
                            if (value.trim().isNotEmpty) nameAssigned = true;
                          },
                        ),
                        error: nameError,
                      ),
                    ),
                    const SizedBox(width: 16),
                    Expanded(child: _field('服务', _providerSelect())),
                  ],
                ),
                const SectionLabel('连接', top: 4),
                if (codex) ...[
                  Padding(
                    padding: const EdgeInsets.only(bottom: 12),
                    child: Row(
                      children: [
                        StatusDot(codexConnected ? Tone.success : Tone.neutral),
                        const SizedBox(width: 8),
                        Expanded(
                          child: Text(
                            codexConnected
                                ? '本地 Codex 已连接'
                                : checkingCodex
                                ? '正在检查本机 Codex 登录态…'
                                : '本地 Codex 尚未连接',
                            style: AppText.sm.copyWith(
                              color: codexConnected ? t.success : t.fg2,
                              fontWeight: AppText.medium,
                            ),
                          ),
                        ),
                        AppButton(
                          label: codexConnected ? '断开连接' : '连接订阅',
                          variant: ButtonVariant.ghost,
                          small: true,
                          onPressed: working
                              ? null
                              : (codexConnected ? logoutCodex : connectCodex),
                        ),
                      ],
                    ),
                  ),
                  if (loginPending) ...[
                    Text(
                      '请在浏览器完成登录，然后等待自动检测。',
                      style: AppText.xs.copyWith(color: t.fg2),
                    ),
                    if (loginURL != null)
                      SelectableText(
                        loginURL!,
                        style: AppText.xs.copyWith(color: t.brand),
                      ),
                    const SizedBox(height: 8),
                    Row(
                      children: [
                        AppButton(label: '检查登录', onPressed: checkLogin),
                        const SizedBox(width: 8),
                        AppButton(label: '取消', onPressed: cancelLogin),
                      ],
                    ),
                    const SizedBox(height: 16),
                  ],
                  _field(
                    'Codex CLI 路径',
                    appTextField(
                      context,
                      controller: cliPath,
                      placeholder: 'codex',
                    ),
                  ),
                ] else if (isCLI) ...[
                  _field(
                    '${backend.short} CLI 路径',
                    appTextField(
                      context,
                      controller: cliPath,
                      placeholder: backend.executable,
                    ),
                    hint:
                        '请先在终端运行 ${backend.loginCommand} 完成登录，再点击「获取模型」。登录凭据由官方 CLI 管理。',
                  ),
                ] else ...[
                  _field(
                    'API 地址',
                    appTextField(
                      context,
                      controller: baseURL,
                      placeholder: provider == 'ollama'
                          ? 'http://127.0.0.1:11434'
                          : 'https://example.com/v1',
                    ),
                    hint: provider == 'ollama'
                        ? '填写 Ollama 服务根地址，无需添加 /api 或 /v1；本地服务无需 API 密钥。'
                        : null,
                  ),
                  if (provider != 'ollama')
                    _field(
                      'API 密钥',
                      appTextField(
                        context,
                        controller: apiKey,
                        placeholder: hasReusableKey
                            ? '已保存密钥，留空则保留'
                            : '输入 API 密钥',
                        password: true,
                      ),
                    ),
                ],
                const SectionLabel('模型', top: 4),
                _field(
                  '模型',
                  Row(
                    children: [
                      Expanded(child: _modelSelect()),
                      const SizedBox(width: 8),
                      AppButton(
                        label: fetchingModels || checkingCodex
                            ? '获取中…'
                            : '获取模型',
                        icon: LucideIcons.refreshCw,
                        variant: ButtonVariant.ghost,
                        onPressed: working || fetchingModels || checkingCodex
                            ? null
                            : fetchModels,
                      ),
                      AppButton(
                        label: '自定义模型',
                        icon: LucideIcons.pencil,
                        variant: ButtonVariant.ghost,
                        onPressed: working ? null : _customModel,
                      ),
                    ],
                  ),
                  hint: modelChoices.isNotEmpty
                      ? (isCLI
                            ? '列表来自本机 ${backend.short} CLI；最后一项可输入自定义模型 ID'
                            : provider == 'ollama'
                            ? '列表来自 Ollama /api/tags；请选择已下载的本地模型'
                            : '列表来自 /v1/models；最后一项可输入自定义模型 ID')
                      : (isCLI
                            ? null
                            : provider == 'ollama'
                            ? '请先启动 Ollama 并下载模型，再点击“获取模型”或输入完整模型 ID（含标签）'
                            : '点击“获取模型”可从服务商列表中选择，也可直接输入模型 ID'),
                ),
                Container(
                  decoration: BoxDecoration(
                    border: Border(top: BorderSide(color: t.border)),
                  ),
                  padding: const EdgeInsets.only(top: 12),
                  child: GestureDetector(
                    key: const ValueKey('advanced-toggle'),
                    behavior: HitTestBehavior.opaque,
                    onTap: () => setState(() => advancedOpen = !advancedOpen),
                    child: MouseRegion(
                      cursor: SystemMouseCursors.click,
                      child: SizedBox(
                        height: 32,
                        child: Row(
                          children: [
                            Icon(
                              advancedOpen
                                  ? LucideIcons.chevronDown
                                  : LucideIcons.chevronRight,
                              size: 16,
                              color: t.fg2,
                            ),
                            const SizedBox(width: 8),
                            Text(
                              '高级',
                              style: AppText.sm.copyWith(
                                color: t.fg,
                                fontWeight: AppText.semibold,
                              ),
                            ),
                            Flexible(
                              child: Text(
                                ' · $_advancedSummary',
                                maxLines: 1,
                                overflow: TextOverflow.ellipsis,
                                style: AppText.sm.copyWith(color: t.fg2),
                              ),
                            ),
                          ],
                        ),
                      ),
                    ),
                  ),
                ),
                if (advancedOpen) ...[
                  const SizedBox(height: 12),
                  Row(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      Expanded(
                        child: _field(
                          '上下文窗口',
                          appTextField(
                            context,
                            controller: contextWindow,
                            placeholder: '自动检测',
                          ),
                        ),
                      ),
                      const SizedBox(width: 16),
                      Expanded(
                        child: _field(
                          '最大输出 Token',
                          appTextField(
                            context,
                            controller: maxTokens,
                            placeholder: '自动检测',
                          ),
                        ),
                      ),
                    ],
                  ),
                  if (provider != 'anthropic' && provider != 'ollama')
                    _field('思考强度', _reasoningSelect()),
                  if (type == 'openai_compatible' ||
                      type == 'anthropic_compatible')
                    Padding(
                      padding: const EdgeInsets.only(bottom: 16),
                      child: Row(
                        crossAxisAlignment: CrossAxisAlignment.start,
                        children: [
                          Padding(
                            padding: const EdgeInsets.only(top: 2),
                            child: AppCheckbox(
                              key: const ValueKey('skip-model-probe'),
                              checked: skipModelProbe,
                              onChanged: (v) => setState(() {
                                skipModelProbe = v;
                                dirty = true;
                              }),
                            ),
                          ),
                          const SizedBox(width: 8),
                          Expanded(
                            child: Column(
                              crossAxisAlignment: CrossAxisAlignment.start,
                              children: [
                                Text(
                                  '跳过模型列表探测',
                                  style: AppText.sm.copyWith(
                                    color: t.fg,
                                    fontWeight: AppText.medium,
                                  ),
                                ),
                                Text(
                                  '服务能正常推理但没有 /models 接口时勾选：不再用模型列表判断是否可用，方案会直接上报给已连接的服务。',
                                  style: AppText.xs.copyWith(color: t.fg3),
                                ),
                              ],
                            ),
                          ),
                        ],
                      ),
                    ),
                ],
              ],
            ),
          ),
        ),
        _actionBar(),
      ],
    );
  }

  Widget _actionBar() {
    final r = editorTest;
    final testing = working && testStatus == '正在测试连接…';
    final (String label, Color color) = testing
        ? ('正在测试…', t.fg2)
        : r == null
        ? ('尚未测试', t.fg3)
        : r.ok
        ? ('✓ 已通过', t.success)
        : ('✗ 未通过', t.danger);
    return Container(
      key: const ValueKey('editor-action-bar'),
      height: AppSize.actionBar,
      padding: const EdgeInsets.symmetric(horizontal: AppSpace.page),
      decoration: BoxDecoration(
        color: t.surface,
        border: Border(top: BorderSide(color: t.border)),
      ),
      child: Row(
        children: [
          AppButton(
            label: '测试连接',
            icon: LucideIcons.play,
            onPressed: working ? null : test,
          ),
          const SizedBox(width: 8),
          Text(label, style: AppText.xs.copyWith(color: color)),
          const Spacer(),
          AppButton(label: '取消', onPressed: working ? null : closeEditor),
          const SizedBox(width: 8),
          AppButton(
            label: working && !testing ? '处理中…' : '保存',
            variant: ButtonVariant.primary,
            onPressed: working ? null : save,
          ),
        ],
      ),
    );
  }

  Widget _detailView(Map<String, dynamic> scheme) {
    final models = (scheme['models'] as List?)?.cast<String>() ?? [];
    Widget auto(String v) => Text(v, style: AppText.sm.copyWith(color: t.fg3));
    Widget val(String v) => Text(v.isEmpty ? '—' : v);
    final cw = (scheme['context_window'] as int? ?? 0);
    final mt = (scheme['max_output_tokens'] as int? ?? 0);
    final profile = backendForType(scheme['type'] as String?);
    final cli = profile.isCLI;
    final h = schemeHealth(scheme);
    final usage = widget.usage['${scheme['id']}'];
    final last = lastTests['${scheme['id']}'];
    String hm(DateTime d) =>
        '${d.hour.toString().padLeft(2, '0')}:${d.minute.toString().padLeft(2, '0')}';
    return SingleChildScrollView(
      padding: const EdgeInsets.all(AppSpace.page),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          const SectionLabel('基本信息', top: 0),
          KvTable([
            ('方案名称', val('${scheme['name'] ?? scheme['id']}')),
            (
              '服务',
              Row(
                mainAxisSize: MainAxisSize.min,
                children: [
                  TypeMark(scheme['type'] as String?),
                  const SizedBox(width: 8),
                  Text(_providerLabel(scheme)),
                ],
              ),
            ),
            ('状态', StatusBadge(h.detail, tone: h.dot, dot: true)),
            if (cli)
              (
                '${profile.short} CLI 路径',
                Text(
                  '${scheme['type'] == 'codex_app_server' ? (scheme['codex_bin'] ?? 'codex') : (scheme['executable'] ?? profile.executable)}',
                  style: monoStyle(AppText.sm.copyWith(color: t.fg)),
                ),
              )
            else ...[
              ('API 地址', val('${scheme['base_url'] ?? ''}')),
              if (scheme['type'] != 'ollama_http')
                (
                  'API 密钥',
                  scheme['has_api_key'] == true
                      ? const Text('已保存')
                      : auto('未设置'),
                ),
            ],
            ('模型', models.isEmpty ? auto('未设置') : val(models.join('、'))),
            if (scheme['type'] != 'anthropic_compatible' &&
                scheme['type'] != 'ollama_http')
              (
                '思考强度',
                val(
                  _reasoningLabel(
                    normalizeEffort(
                      backendForType(scheme['type'] as String?).provider,
                      scheme['reasoning_effort'] as String?,
                    ),
                  ),
                ),
              ),
          ]),
          const SectionLabel('高级'),
          KvTable([
            ('上下文窗口', cw == 0 ? auto('自动检测') : val('$cw')),
            ('最大输出 Token', mt == 0 ? auto('自动检测') : val('$mt')),
          ]),
          const SectionLabel('运行情况'),
          KvTable([
            (
              '近 7 天用量',
              usage == null || usage == 0
                  ? auto('暂无记录')
                  : Text('${fmtTokens(usage)} tokens'),
            ),
            (
              '最近测试',
              last == null
                  ? auto('本次运行尚未测试')
                  : Text(
                      last.ok
                          ? '✓ 通过 · ${last.ms} ms · ${hm(last.at)}'
                          : '✗ 未通过 · ${last.message} · ${hm(last.at)}',
                      style: AppText.sm.copyWith(
                        color: last.ok ? t.success : t.danger,
                      ),
                    ),
            ),
          ]),
        ],
      ),
    );
  }

  Widget _rightPane() => Column(
    crossAxisAlignment: CrossAxisAlignment.stretch,
    children: [
      _paneHeader(
        title: Row(children: [_title(editorOpen ? '编辑方案' : '方案详情')]),
        actions: [
          // right-aligned, as in the design
          if (editorOpen && (dirty || creating))
            const StatusBadge(
              '未保存',
              key: ValueKey('unsaved-badge'),
              tone: Tone.warning,
            ),
          if (!editorOpen && highlighted != null)
            AppButton(
              label: '编辑',
              icon: LucideIcons.pencil,
              onPressed: () => openEditor(highlighted!),
            ),
        ],
      ),
      Expanded(
        child: editorOpen
            ? _editorFields()
            : highlighted == null
            ? Center(
                child: Text(
                  schemes.isEmpty ? '添加方案后在这里查看详情' : '选择左侧方案查看详情，或添加新方案。',
                  style: AppText.sm.copyWith(color: t.fg2),
                ),
              )
            : _detailView(highlighted!),
      ),
    ],
  );

  @override
  Widget build(BuildContext context) => LayoutBuilder(
    builder: (context, size) {
      final leftWidth = size.maxWidth * (size.maxWidth < 1100 ? 0.45 : 0.5);
      return ColoredBox(
        color: t.bg,
        child: Column(
          children: [
            Expanded(
              child: Row(
                children: [
                  SizedBox(width: leftWidth, child: _leftPane()),
                  const Hairline(vertical: true),
                  Expanded(child: _rightPane()),
                ],
              ),
            ),
            _statusBar(),
          ],
        ),
      );
    },
  );
}

/// Scheme health for the list dot, badge and detail status.
class SchemeHealth {
  const SchemeHealth(
    this.dot,
    this.detail, {
    this.badge,
    this.badgeTone = Tone.neutral,
  });
  final Tone dot;
  final String detail;
  final String? badge;
  final Tone badgeTone;
}

SchemeHealth schemeHealth(Map<String, dynamic> a) {
  if (a['draft'] == true) return const SchemeHealth(Tone.neutral, '未保存');
  if (a['enabled'] != true) {
    return const SchemeHealth(Tone.neutral, '已停用', badge: '已停用');
  }
  if (a['auth_state'] == 'required') {
    return const SchemeHealth(
      Tone.warning,
      '需要登录',
      badge: '需要登录',
      badgeTone: Tone.warning,
    );
  }
  return switch (a['health']) {
    'ready' => SchemeHealth(
      Tone.success,
      a['type'] == 'codex_app_server' ? '可用 · 已登录' : '可用',
    ),
    'unavailable' => const SchemeHealth(
      Tone.danger,
      '不可用',
      badge: '不可用',
      badgeTone: Tone.danger,
    ),
    'storage_full' => const SchemeHealth(
      Tone.danger,
      '存储已满',
      badge: '存储已满',
      badgeTone: Tone.danger,
    ),
    _ => const SchemeHealth(Tone.neutral, '检测中'),
  };
}

Tone toneOf(StatusLevel l) => switch (l) {
  StatusLevel.ok => Tone.success,
  StatusLevel.warn => Tone.warning,
  StatusLevel.error => Tone.danger,
  StatusLevel.off => Tone.neutral,
};

String fmtTokens(num n) {
  String trim(double v) {
    final s = v.toStringAsFixed(n >= 1000000 ? 2 : 1);
    var r = s;
    while (r.contains('.') && (r.endsWith('0') || r.endsWith('.'))) {
      r = r.substring(0, r.length - 1);
    }
    return r;
  }

  if (n >= 1000000) return '${trim(n / 1000000)}M';
  if (n >= 1000) return '${trim(n / 1000)}k';
  return '$n';
}

/// Outlined 36px text field from the tokens.
Widget appTextField(
  BuildContext context, {
  Key? key,
  required TextEditingController controller,
  String? placeholder,
  ValueChanged<String>? onChanged,
  ValueChanged<String>? onSubmitted,
  bool password = false,
  bool clear = false,
  bool enabled = true,
}) {
  final t = AppTokens.of(context);
  return TextField(
    key: key,
    controller: controller,
    enabled: enabled,
    onChanged: onChanged,
    onSubmitted: onSubmitted,
    filled: false,
    // white field with a hairline border (the default tints it grey)
    decoration: BoxDecoration(
      color: enabled ? t.surface : t.subtle,
      borderRadius: BorderRadius.circular(AppRadius.control),
      border: Border.all(color: t.checkboxBorder),
    ),
    padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 8),
    style: AppText.md.copyWith(color: t.fg),
    placeholder: placeholder == null
        ? null
        : Text(placeholder, style: AppText.md.copyWith(color: t.fg3)),
    features: [
      if (password) const InputFeature.passwordToggle(),
      if (clear) const InputFeature.clear(),
    ],
  );
}

/// Anchored ⋮ menu of a scheme row.
class SchemeMenu extends StatelessWidget {
  const SchemeMenu({super.key, required this.onSelected});
  final ValueChanged<String> onSelected;

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    Widget item(String v, IconData icon, String label, {bool danger = false}) =>
        _MenuItem(
          key: ValueKey('menu-$v'),
          icon: icon,
          label: label,
          color: danger ? t.danger : null,
          onTap: () => onSelected(v),
        );
    return Container(
      key: const ValueKey('scheme-menu'),
      width: 184,
      padding: const EdgeInsets.all(4),
      decoration: BoxDecoration(
        color: t.surface,
        borderRadius: BorderRadius.circular(AppRadius.control),
        border: Border.all(color: t.border),
        boxShadow: [
          BoxShadow(
            color: t.shadow,
            blurRadius: 16,
            offset: const Offset(0, 4),
          ),
        ],
      ),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          item('rename', LucideIcons.pencil, '重命名'),
          item('default', LucideIcons.star, '设为默认方案'),
          item('test', LucideIcons.play, '测试连接'),
          Container(
            height: 1,
            margin: const EdgeInsets.symmetric(vertical: 4),
            color: t.border,
          ),
          item('delete', LucideIcons.trash2, '删除方案', danger: true),
        ],
      ),
    );
  }
}

class _MenuItem extends StatefulWidget {
  const _MenuItem({
    super.key,
    required this.icon,
    required this.label,
    required this.onTap,
    this.color,
  });
  final IconData icon;
  final String label;
  final VoidCallback onTap;
  final Color? color;

  @override
  State<_MenuItem> createState() => _MenuItemState();
}

class _MenuItemState extends State<_MenuItem> {
  bool hover = false;

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    return MouseRegion(
      cursor: SystemMouseCursors.click,
      onEnter: (_) => setState(() => hover = true),
      onExit: (_) => setState(() => hover = false),
      child: GestureDetector(
        behavior: HitTestBehavior.opaque,
        onTap: widget.onTap,
        child: Container(
          height: 32,
          padding: const EdgeInsets.symmetric(horizontal: 8),
          decoration: BoxDecoration(
            color: hover ? t.mutedBg : null,
            borderRadius: BorderRadius.circular(AppRadius.badge),
          ),
          child: Row(
            children: [
              Icon(widget.icon, size: 16, color: widget.color ?? t.fg2),
              const SizedBox(width: 8),
              Text(
                widget.label,
                style: AppText.sm.copyWith(color: widget.color ?? t.fg),
              ),
            ],
          ),
        ),
      ),
    );
  }
}

/// Body of the 删除方案 confirmation: red icon, name, impact, irreversible.
class DeleteSchemeMessage extends StatelessWidget {
  const DeleteSchemeMessage({
    super.key,
    required this.name,
    required this.affected,
  });
  final String name;
  final List<String> affected;

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    final impact = affected.isEmpty
        ? '目前没有远端服务被授权使用它。此操作不可撤销。'
        : '已授权它的 ${affected.length} 个远端服务（${affected.join('、')}）将不能再调用这个模型。此操作不可撤销。';
    return Row(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Container(
          width: 40,
          height: 40,
          alignment: Alignment.center,
          decoration: BoxDecoration(color: t.danger50, shape: BoxShape.circle),
          child: Icon(LucideIcons.trash2, size: 20, color: t.danger),
        ),
        const SizedBox(width: 16),
        Expanded(
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text.rich(
                TextSpan(
                  children: [
                    const TextSpan(text: '确定删除方案 '),
                    TextSpan(
                      text: name,
                      style: const TextStyle(fontWeight: AppText.semibold),
                    ),
                    const TextSpan(text: '？'),
                  ],
                ),
                style: AppText.sm.copyWith(color: t.fg),
              ),
              const SizedBox(height: 4),
              Text(impact, style: AppText.sm.copyWith(color: t.fg2)),
            ],
          ),
        ),
      ],
    );
  }
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
    'executable',
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
