import 'package:flutter_test/flutter_test.dart';
import 'package:localsubproxy_ui/tokens.dart';
import 'package:localsubproxy_ui/ui_kit.dart';
import 'package:shadcn_flutter/shadcn_flutter.dart';

// Regression: monoStyle used a pre-prefixed family name; merged into the
// app's DefaultTextStyle (a package font) it became
// "packages/shadcn_flutter/packages/shadcn_flutter/GeistMono", which no font
// matches, so pair codes and paths fell back to another font.
void main() {
  testWidgets('mono text resolves to the bundled GeistMono once-prefixed', (
    t,
  ) async {
    await t.pumpWidget(
      ShadcnApp(
        home: Center(child: Text('K7QF-3H9X', style: monoStyle(AppText.sm))),
      ),
    );
    final rich = t.widget<RichText>(find.byType(RichText).last);
    // effective style = inherited DefaultTextStyle merged with the widget's
    final ctx = t.element(find.text('K7QF-3H9X'));
    final effective = DefaultTextStyle.of(ctx).style.merge(rich.text.style);
    expect(effective.fontFamily, 'packages/shadcn_flutter/GeistMono');
    expect(rich.text.style!.fontFamily, 'packages/shadcn_flutter/GeistMono');
  });
}
