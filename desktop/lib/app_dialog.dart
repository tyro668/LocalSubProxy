import 'package:shadcn_flutter/shadcn_flutter.dart';

/// Shared dialog surface for every desktop dialog: bold title on the left,
/// grey close (X) on the right, a thin full-width divider, then the content.
/// Header, divider and actions stay fixed; the content area is bounded by
/// the window and scrolls (with a visible scrollbar) when it is too tall.
class AppDialog extends StatelessWidget {
  const AppDialog({
    super.key,
    required this.title,
    this.content,
    this.actions,
    this.maxWidth = 440,
    this.scrollContent = true,
    this.onClose,
    this.closeEnabled = true,
  });

  final Widget title;
  final Widget? content;
  final List<Widget>? actions;
  final double maxWidth;

  /// false: [content] manages its own scrolling (it gets a bounded height
  /// and may use Flexible/Expanded, e.g. a fixed header with a scrolling list).
  final bool scrollContent;

  /// Defaults to popping the dialog route with no result.
  final VoidCallback? onClose;

  /// false: the close (X) button is disabled, e.g. while a save is running.
  final bool closeEnabled;

  static const titleColor = Color(0xFF0F172A);
  static const closeColor = Color(0xFF64748B);
  static const dividerColor = Color(0xFFE5E7EB);
  static const radius = 20.0;
  static const margin = 20.0;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final size = MediaQuery.sizeOf(context);
    final width = (size.width - 2 * margin).clamp(200.0, maxWidth);
    final maxHeight = (size.height - 2 * margin).clamp(160.0, double.infinity);
    return ModalBackdrop(
      borderRadius: BorderRadius.circular(radius),
      barrierColor: Colors.black.withValues(alpha: 0.5),
      surfaceClip: false,
      child: Container(
        key: const ValueKey('app-dialog'),
        width: width,
        constraints: BoxConstraints(maxHeight: maxHeight),
        decoration: BoxDecoration(
          color: const Color(0xFFFFFFFF),
          borderRadius: BorderRadius.circular(radius),
          boxShadow: const [
            BoxShadow(
              color: Color(0x29000000),
              blurRadius: 24,
              offset: Offset(0, 8),
            ),
          ],
        ),
        clipBehavior: Clip.antiAlias,
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Padding(
              key: const ValueKey('app-dialog-header'),
              padding: const EdgeInsets.fromLTRB(24, 14, 12, 14),
              child: Row(
                children: [
                  Expanded(
                    child: DefaultTextStyle.merge(
                      style: const TextStyle(
                        fontSize: 18,
                        fontWeight: FontWeight.w700,
                        color: titleColor,
                        height: 1.3,
                      ),
                      maxLines: 2,
                      overflow: TextOverflow.ellipsis,
                      child: title,
                    ),
                  ),
                  const Gap(8),
                  IconButton.ghost(
                    key: const ValueKey('app-dialog-close'),
                    density: ButtonDensity.compact,
                    icon: const Icon(
                      LucideIcons.x,
                      size: 20,
                      color: closeColor,
                    ),
                    onPressed: !closeEnabled
                        ? null
                        : onClose ?? () => Navigator.of(context).maybePop(),
                  ),
                ],
              ),
            ),
            Container(
              key: const ValueKey('app-dialog-divider'),
              height: 1,
              color: dividerColor,
            ),
            if (content != null)
              Flexible(
                child: DefaultTextStyle.merge(
                  style: theme.typography.small.copyWith(
                    color: theme.colorScheme.foreground,
                  ),
                  child: scrollContent
                      ? ScrollArea(
                          padding: const EdgeInsets.fromLTRB(24, 20, 24, 20),
                          child: content!,
                        )
                      : Padding(
                          padding: const EdgeInsets.fromLTRB(24, 20, 24, 20),
                          child: content,
                        ),
                ),
              ),
            if (actions != null && actions!.isNotEmpty)
              Padding(
                padding: EdgeInsets.fromLTRB(
                  24,
                  content == null ? 20 : 0,
                  24,
                  20,
                ),
                child: Row(
                  mainAxisAlignment: MainAxisAlignment.end,
                  children: [
                    for (var i = 0; i < actions!.length; i++) ...[
                      if (i > 0) const Gap(8),
                      actions![i],
                    ],
                  ],
                ),
              ),
          ],
        ),
      ),
    );
  }
}

/// Vertical scroll view with a visible scrollbar.
class ScrollArea extends StatefulWidget {
  const ScrollArea({
    super.key,
    required this.child,
    this.scrollKey,
    this.padding,
  });
  final Widget child;
  final Key? scrollKey;
  final EdgeInsetsGeometry? padding;

  @override
  State<ScrollArea> createState() => _ScrollAreaState();
}

class _ScrollAreaState extends State<ScrollArea> {
  final controller = ScrollController();

  @override
  void dispose() {
    controller.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) => Scrollbar(
    controller: controller,
    thumbVisibility: true,
    child: SingleChildScrollView(
      key: widget.scrollKey,
      controller: controller,
      padding: widget.padding,
      child: widget.child,
    ),
  );
}
