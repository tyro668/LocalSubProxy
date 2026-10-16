import 'package:shadcn_flutter/shadcn_flutter.dart';

import 'tokens.dart';

/// Shared dialog surface for every desktop dialog: title (+ subtitle) on the
/// left, optional header actions and a grey close (X) on the right, a thin
/// full-width divider, then the content, then an optional footer bar.
/// Header, divider and footer stay fixed; the content area is bounded by
/// the window and scrolls (with a visible scrollbar) when it is too tall.
class AppDialog extends StatelessWidget {
  const AppDialog({
    super.key,
    required this.title,
    this.subtitle,
    this.content,
    this.actions,
    this.footer,
    this.headerActions,
    this.maxWidth = 440,
    this.scrollContent = true,
    this.onClose,
    this.closeEnabled = true,
  });

  final Widget title;
  final Widget? subtitle;
  final Widget? content;

  /// Right-aligned buttons in the footer bar.
  final List<Widget>? actions;

  /// Free-form footer content (left side of the footer bar).
  final Widget? footer;

  /// Extra icon buttons left of the close button.
  final List<Widget>? headerActions;
  final double maxWidth;

  /// false: [content] manages its own scrolling (it gets a bounded height
  /// and may use Flexible/Expanded, e.g. a fixed header with a scrolling list).
  final bool scrollContent;

  /// Defaults to popping the dialog route with no result.
  final VoidCallback? onClose;

  /// false: the close (X) button is disabled, e.g. while a save is running.
  final bool closeEnabled;

  static const margin = 20.0;

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    final size = MediaQuery.sizeOf(context);
    final width = (size.width - 2 * margin).clamp(200.0, maxWidth);
    final maxHeight = (size.height - 2 * margin).clamp(160.0, double.infinity);
    final radius = BorderRadius.circular(AppRadius.card);
    final hasActions = actions != null && actions!.isNotEmpty;
    const pad = EdgeInsets.fromLTRB(24, 20, 24, 20);
    return ModalBackdrop(
      borderRadius: radius,
      barrierColor: t.scrim,
      surfaceClip: false,
      child: Container(
        key: const ValueKey('app-dialog'),
        width: width,
        constraints: BoxConstraints(maxHeight: maxHeight),
        decoration: BoxDecoration(
          color: t.surface,
          borderRadius: radius,
          boxShadow: [
            BoxShadow(
              color: t.shadow,
              blurRadius: 24,
              offset: const Offset(0, 8),
            ),
          ],
        ),
        // border drawn on top so it does not inset the full-width divider
        foregroundDecoration: BoxDecoration(
          borderRadius: radius,
          border: Border.all(color: t.border),
        ),
        clipBehavior: Clip.antiAlias,
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Padding(
              key: const ValueKey('app-dialog-header'),
              padding: EdgeInsets.fromLTRB(
                24,
                subtitle == null ? 12 : 8,
                12,
                subtitle == null ? 12 : 8,
              ),
              child: Row(
                children: [
                  Expanded(
                    child: Column(
                      crossAxisAlignment: CrossAxisAlignment.start,
                      mainAxisSize: MainAxisSize.min,
                      children: [
                        DefaultTextStyle.merge(
                          style: AppText.lg.copyWith(color: t.fg),
                          maxLines: 2,
                          overflow: TextOverflow.ellipsis,
                          child: title,
                        ),
                        if (subtitle != null)
                          DefaultTextStyle.merge(
                            style: AppText.xs.copyWith(color: t.fg2),
                            maxLines: 1,
                            overflow: TextOverflow.ellipsis,
                            child: subtitle!,
                          ),
                      ],
                    ),
                  ),
                  const Gap(8),
                  ...?headerActions,
                  IconButton.ghost(
                    key: const ValueKey('app-dialog-close'),
                    density: ButtonDensity.compact,
                    icon: Icon(LucideIcons.x, size: 18, color: t.fg2),
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
              color: t.border,
            ),
            if (content != null)
              Flexible(
                child: DefaultTextStyle.merge(
                  style: AppText.sm.copyWith(color: t.fg),
                  child: scrollContent
                      ? ScrollArea(padding: pad, child: content!)
                      : Padding(padding: pad, child: content),
                ),
              ),
            if (footer != null || hasActions)
              Container(
                key: const ValueKey('app-dialog-footer'),
                height: AppSize.actionBar,
                padding: const EdgeInsets.symmetric(horizontal: 24),
                decoration: BoxDecoration(
                  color: t.surface,
                  border: Border(top: BorderSide(color: t.border)),
                ),
                child: Row(
                  children: [
                    Expanded(child: footer ?? const SizedBox()),
                    if (hasActions)
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
