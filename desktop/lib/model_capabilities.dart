/// Model-specific metadata returned by the local backend, never a model-name
/// heuristic in the UI. Empty/unknown capabilities offer only provider default.
class ModelCapabilities {
  ModelCapabilities.fromJson(Map<String, dynamic> json)
    : id = json['id'] as String,
      known = json['reasoning_known'] == true,
      defaultEffort = json['default_reasoning_effort'] as String? ?? '',
      efforts = [
        for (final e in json['reasoning_efforts'] as List? ?? const [])
          EffortOption(
            e['value'] as String,
            model: e['model'] as String? ?? '',
          ),
      ];
  final String id;
  final bool known;
  final String defaultEffort;
  final List<EffortOption> efforts;
  bool get hasVariants => efforts.any((e) => e.model.isNotEmpty);
  EffortOption? option(String value) {
    for (final e in efforts) {
      if (e.value == value) return e;
    }
    return null;
  }
}

class EffortOption {
  const EffortOption(this.value, {this.model = ''});
  final String value;
  final String model;
}

/// The kernel stores efforts normalised; only "default" means unset.
String normalizeEffort(String? effort) =>
    effort == null || effort == 'default' ? '' : effort;

String reasoningLabel(String? value) => switch (value) {
  null || '' || 'default' => '默认',
  'off' || 'none' => '关闭',
  'minimal' => '极低',
  'low' => '低',
  'medium' => '中',
  'high' => '高',
  'xhigh' => '超高',
  'max' => '最大',
  'ultra' => '极致',
  _ => value,
};
