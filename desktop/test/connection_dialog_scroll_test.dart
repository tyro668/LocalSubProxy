import 'dart:io';
import 'dart:ui' as ui;

import 'package:flutter/rendering.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:localsubproxy_ui/main.dart';
import 'package:shadcn_flutter/shadcn_flutter.dart';

// The 远端服务连接 dialog must stay within the window with many connections:
// header and footer fixed, the connection list scrolls with a scrollbar.

Map<String, dynamic> statusWith(int n) => {
  'paired': true,
  'device_name': '测试电脑',
  'running_tasks': 1,
  'connections': [
    for (var i = 0; i < n; i++)
      {
        'id': 'b_$i',
        'saas_name': 'SaaS $i',
        'tenant': 't$i',
        'state': i.isEven ? 'online' : 'disconnected',
        'connected': i.isEven,
        'paused': i.isOdd,
        'remote_url': 'https://saas$i.example.com',
        'server_url': 'https://gw$i.example.com',
        'running_tasks': 0,
        'max_concurrency': 4,
        'allow': ['*'],
        'transport': 'wss',
        'e2ee': true,
      },
  ],
};

Future<void> loadFonts() async {
  Future<void> load(String family, List<String> files) async {
    final l = FontLoader(family);
    for (final f in files) {
      if (File(f).existsSync()) {
        l.addFont(
          Future.value(ByteData.sublistView(File(f).readAsBytesSync())),
        );
      }
    }
    await l.load();
  }

  final pub =
      '${Platform.environment['HOME']}/.pub-cache/hosted/pub.dev/shadcn_flutter-0.0.53/lib';
  await load('packages/shadcn_flutter/LucideIcons', [
    '$pub/icons/LucideIcons.ttf',
  ]);
  await load('packages/shadcn_flutter/GeistMono', [
    // the bundled GeistMono .otf (CFF) files render as boxes in
    // flutter_tester; a static TTF monospace stands in for screenshots
    '/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf',
  ]);
  // CJK-capable font as the UI family so screenshots are legible.
  await load('packages/shadcn_flutter/GeistSans', [
    '/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc',
  ]);
}

Future<void> openDialog(WidgetTester tester, Size size, int n) async {
  tester.view.physicalSize = size;
  tester.view.devicePixelRatio = 1;
  addTearDown(tester.view.resetPhysicalSize);
  addTearDown(tester.view.resetDevicePixelRatio);
  await tester.pumpWidget(
    ShadcnApp(
      debugShowCheckedModeBanner: false,
      home: Builder(
        builder: (context) => Center(
          child: PrimaryButton(
            onPressed: () => showOverlay<void>(
              context,
              DialogConfiguration<void>(
                builder: (_) => DialogFrame(
                  title: '远端服务连接',
                  maxWidth: 800,
                  scrollContent: false,
                  footer: ConnectionsPanel.footer(statusWith(n)),
                  child: ConnectionsPanel(
                    status: statusWith(n),
                    adapters: const [],
                    busy: false,
                    onRefresh: () {},
                    onAdd: () {},
                    onToggle: (_) {},
                    onRevoke: (_) {},
                    onModels: (_) {},
                  ),
                ),
              ),
            ),
            child: const Text('open'),
          ),
        ),
      ),
    ),
  );
  await tester.tap(find.text('open'));
  await tester.pumpAndSettle();
}

void main() {
  setUpAll(loadFonts);
  for (final size in const [Size(1024, 700), Size(800, 600)]) {
    for (final n in const [3, 5, 10]) {
      testWidgets(
        'dialog ${size.width.toInt()}x${size.height.toInt()} with $n connections',
        (tester) async {
          await openDialog(tester, size, n);
          expect(
            tester.takeException(),
            isNull,
            reason: 'no RenderFlex overflow',
          );
          final window = Offset.zero & size;
          final dialog = tester.getRect(
            find.byKey(const ValueKey('app-dialog')),
          );
          expect(
            dialog.top >= 0 && dialog.bottom <= size.height,
            isTrue,
            reason: 'dialog $dialog bounded by window $window',
          );
          bool visible(Finder f) {
            final r = tester.getRect(f);
            return r.top >= dialog.top &&
                r.bottom <= dialog.bottom &&
                r.height > 0;
          }

          for (final f in [
            find.text('远端服务连接'),
            find.byIcon(LucideIcons.refreshCw),
            find.text('添加连接'),
            find.text('测试电脑'),
            find.text('1 个任务'),
          ]) {
            expect(f, findsOneWidget); // device / tasks in the dialog footer
            expect(visible(f), isTrue, reason: '$f must stay visible');
          }
          final list = find.byKey(const ValueKey('connection-list'));
          expect(list, findsOneWidget);
          expect(
            find.descendant(
              of: find.byKey(const ValueKey('app-dialog')),
              matching: find.byType(Scrollbar),
            ),
            findsWidgets,
          );
          final pos = tester
              .state<ScrollableState>(
                find
                    .descendant(of: list, matching: find.byType(Scrollable))
                    .first,
              )
              .position;
          final dialogH = dialog.height;
          if (pos.maxScrollExtent > 0) {
            // last tile reachable by scrolling, header/footer still visible
            await tester.drag(list, const Offset(0, -5000));
            await tester.pumpAndSettle();
            expect(find.text('SaaS ${n - 1}'), findsOneWidget);
            expect(visible(find.text('SaaS ${n - 1}')), isTrue);
            expect(
              visible(find.text('添加连接')) && visible(find.text('1 个任务')),
              isTrue,
            );
          }
          expect(
            tester.getRect(find.byKey(const ValueKey('app-dialog'))).height,
            dialogH,
          );
          if (n == 10) {
            expect(
              pos.maxScrollExtent,
              greaterThan(0),
              reason: '10 tiles must scroll',
            );
          }
          final out = Platform.environment['DIALOG_PNG'];
          if (out != null && n == 5 && size.width == 1024) {
            await tester.drag(list, const Offset(0, 5000));
            await tester.pumpAndSettle();
            final ro = tester.renderObject<RenderRepaintBoundary>(
              find.byType(RepaintBoundary).first,
            );
            final img = await tester.runAsync(() async {
              final i = await ro.toImage();
              return (await i.toByteData(format: ui.ImageByteFormat.png))!;
            });
            File(out)
              ..createSync(recursive: true)
              ..writeAsBytesSync(img!.buffer.asUint8List());
          }
        },
      );
    }
  }
}
