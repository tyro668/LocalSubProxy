import 'package:flutter_test/flutter_test.dart';
import 'package:localsubproxy_ui/profile_workspace.dart';

void main() {
  // shape mirrors connector.AdapterView (list_adapters output)
  final view = <String, dynamic>{
    'id': 'lm',
    'type': 'openai_compatible',
    'name': 'LM',
    'base_url': 'http://x',
    'models': ['m'],
    'is_default': false,
    'enabled': false,
    'has_api_key': true,
    'skip_model_probe': true,
    'capabilities': ['tools', 'param:temperature'],
    'concurrency': 4,
    'health': 'ready',
    'auth_state': 'none',
    'configured_capabilities': ['tools'],
    'configured_concurrency': 0,
  };

  test('edit body drops view-only and computed values', () {
    final b = adapterEditBody(view, {'name': 'New'});
    expect(b['name'], 'New');
    expect(b['enabled'], false);
    expect(b['capabilities'], ['tools']);
    expect(b['concurrency'], 0);
    for (final k in [
      'has_api_key',
      'health',
      'auth_state',
      'configured_capabilities',
      'configured_concurrency',
    ]) {
      expect(b.containsKey(k), isFalse, reason: k);
    }
  });

  test('set default keeps everything else', () {
    final b = adapterEditBody(view, {'is_default': true});
    expect(b['is_default'], true);
    expect(b['models'], ['m']);
    expect(b['enabled'], false);
  });
}
