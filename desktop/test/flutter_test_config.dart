import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:avora_ui/backend_catalog.dart';

/// Every widget test draws scheme forms from the kernel's `backends` answer,
/// recorded in fixtures/backends.json by the Go test TestDesktopBackendsFixture.
Future<void> testExecutable(FutureOr<void> Function() testMain) async {
  final file = File(
    'test/fixtures/backends.json',
  ); // flutter test runs in the package root
  loadBackendProfiles(jsonDecode(file.readAsStringSync()) as List);
  await testMain();
}
