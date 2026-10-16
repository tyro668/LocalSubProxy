import 'dart:io';
import 'dart:ui' as ui;

import 'package:flutter/rendering.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:localsubproxy_ui/main.dart';
import 'package:shadcn_flutter/shadcn_flutter.dart';

import 'connection_dialog_scroll_test.dart' as conn;

Future<void> open(
  WidgetTester t,
  Widget Function() dialog, {
  Size size = const Size(1024, 700),
}) async {
  t.view.physicalSize = size;
  t.view.devicePixelRatio = 1;
  addTearDown(t.view.resetPhysicalSize);
  addTearDown(t.view.resetDevicePixelRatio);
  await t.pumpWidget(
    ShadcnApp(
      debugShowCheckedModeBanner: false,
      home: Builder(
        builder: (context) => Center(
          child: PrimaryButton(
            onPressed: () => showOverlay<void>(
              context,
              DialogConfiguration<void>(builder: (_) => dialog()),
            ),
            child: const Text('open'),
          ),
        ),
      ),
    ),
  );
  await t.tap(find.text('open'));
  await t.pumpAndSettle();
}

Future<void> shot(WidgetTester t, String name) async {
  final dir = Platform.environment['DIALOG_PNG_DIR'];
  if (dir == null) return;
  final ro = t.renderObject<RenderRepaintBoundary>(
    find.byType(RepaintBoundary).first,
  );
  final img = await t.runAsync(
    () async => (await (await ro.toImage()).toByteData(
      format: ui.ImageByteFormat.png,
    ))!,
  );
  File('$dir/$name.png')
    ..createSync(recursive: true)
    ..writeAsBytesSync(img!.buffer.asUint8List());
}

Widget longDialog() => AppDialog(
  title: const Text('新建项目'),
  content: Column(
    crossAxisAlignment: CrossAxisAlignment.stretch,
    children: [for (var i = 0; i < 60; i++) Text('第 $i 行')],
  ),
  actions: [PrimaryButton(onPressed: () {}, child: const Text('保存'))],
);

void main() {
  setUpAll(conn.loadFonts);

  testWidgets(
    'header: bold 18px navy title, compact ~52px header, grey X, full-width divider',
    (t) async {
      await open(t, longDialog);
      final dialog = t.getRect(find.byKey(const ValueKey('app-dialog')));
      final divider = t.getRect(
        find.byKey(const ValueKey('app-dialog-divider')),
      );
      expect(divider.height, 1);
      expect(divider.left, dialog.left);
      expect(divider.right, dialog.right);
      final header = t.getRect(find.byKey(const ValueKey('app-dialog-header')));
      expect(header.bottom, divider.top);
      // header + divider ≈ 52px (was 77px)
      expect(header.height + divider.height, inInclusiveRange(48, 54));
      expect(
        t.getSize(find.byKey(const ValueKey('app-dialog-close'))).height,
        lessThanOrEqualTo(32),
      );
      final title = t.widget<RichText>(
        find
            .descendant(
              of: find.byKey(const ValueKey('app-dialog-header')),
              matching: find.byType(RichText),
            )
            .first,
      );
      final style = title.text.style!;
      // design tokens: 16px semibold title, secondary-grey close icon
      final tokens = AppTokens.light;
      expect(style.fontSize, 16);
      expect(style.fontWeight, FontWeight.w600);
      expect(style.color, tokens.fg);
      final x = t.widget<Icon>(find.byIcon(LucideIcons.x));
      expect(x.color, tokens.fg2);
      expect(
        t.getRect(find.byIcon(LucideIcons.x)).center.dx,
        greaterThan(dialog.center.dx),
      );
      final box = t.widget<Container>(find.byKey(const ValueKey('app-dialog')));
      final deco = box.decoration as BoxDecoration;
      expect(deco.borderRadius, BorderRadius.circular(AppRadius.card));
      expect(deco.color, tokens.surface);
    },
  );

  testWidgets('close X pops the dialog', (t) async {
    await open(t, longDialog);
    expect(find.text('新建项目'), findsOneWidget);
    await t.tap(find.byKey(const ValueKey('app-dialog-close')));
    await t.pumpAndSettle();
    expect(find.text('新建项目'), findsNothing);
  });

  testWidgets('header, divider and actions stay fixed while content scrolls', (
    t,
  ) async {
    await open(t, longDialog, size: const Size(800, 600));
    final dialog = t.getRect(find.byKey(const ValueKey('app-dialog')));
    expect(dialog.top >= 0 && dialog.bottom <= 600, isTrue);
    expect(find.byType(Scrollbar), findsOneWidget);
    final header0 = t.getRect(find.text('新建项目'));
    final div0 = t.getRect(find.byKey(const ValueKey('app-dialog-divider')));
    final save0 = t.getRect(find.text('保存'));
    final row0 = t.getRect(find.text('第 0 行'));
    await t.drag(find.text('第 5 行'), const Offset(0, -300));
    await t.pumpAndSettle();
    expect(t.getRect(find.text('新建项目')), header0);
    expect(t.getRect(find.byKey(const ValueKey('app-dialog-divider'))), div0);
    expect(t.getRect(find.text('保存')), save0);
    expect(t.getRect(find.text('第 0 行')).top, lessThan(row0.top));
    // content never draws over the header: scrolled rows are clipped below the divider
    expect(t.getRect(find.text('第 0 行')).top, lessThan(div0.bottom));
  });

  testWidgets('screenshots', (t) async {
    await conn.openDialog(t, const Size(1024, 700), 5);
    expect(find.byKey(const ValueKey('app-dialog-divider')), findsOneWidget);
    expect(find.byKey(const ValueKey('app-dialog-close')), findsOneWidget);
    await shot(t, 'connections_5_1024x700');
  });

  testWidgets('screenshot: models', (t) async {
    final selected = <String>{'codex/gpt-6.1-sol'};
    await open(
      t,
      () => AppDialog(
        title: const Text('「Acme」可用模型'),
        maxWidth: 540,
        content: ModelChecklist(
          models: const ['*', 'codex/gpt-6.1-sol', 'deepseek/deepseek-flash'],
          labels: const {
            '*': '全部本机模型',
            'codex/gpt-6.1-sol': 'gpt-6.1-sol',
            'deepseek/deepseek-flash': 'deepseek-flash',
          },
          selected: selected,
          onChanged: (_, _) {},
        ),
        actions: [
          OutlineButton(onPressed: () {}, child: const Text('取消')),
          PrimaryButton(onPressed: () {}, child: const Text('保存')),
        ],
      ),
    );
    expect(find.byKey(const ValueKey('app-dialog-divider')), findsOneWidget);
    await shot(t, 'models_1024x700');
  });

  testWidgets('screenshot: confirm', (t) async {
    await open(
      t,
      () => AppDialog(
        title: const Text('删除 deepseek-flash？'),
        content: const Text('删除后，使用该方案的连接将无法再调用这个模型。'),
        actions: [
          OutlineButton(onPressed: () {}, child: const Text('取消')),
          DestructiveButton(onPressed: () {}, child: const Text('删除')),
        ],
      ),
    );
    await shot(t, 'confirm_1024x700');
  });
}
