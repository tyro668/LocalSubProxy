import 'dart:math' as math;

import 'package:shadcn_flutter/shadcn_flutter.dart';

import 'tokens.dart';

/// Semantic tone shared by badges, dots, alerts and the status bar.
enum Tone { success, warning, danger, neutral, brand }

class ToneColors {
  const ToneColors(this.fg, this.bg, this.border);
  final Color fg, bg, border;

  static ToneColors of(BuildContext context, Tone tone) {
    final t = AppTokens.of(context);
    return switch (tone) {
      Tone.success => ToneColors(t.success, t.success50, t.success200),
      Tone.warning => ToneColors(t.warning, t.warning50, t.warning200),
      Tone.danger => ToneColors(t.danger, t.danger50, t.danger200),
      Tone.brand => ToneColors(t.brand, t.brand100, t.brand100),
      Tone.neutral => ToneColors(t.neutral, t.neutral50, t.border),
    };
  }
}

enum ButtonVariant { primary, outline, ghost, destructive, dangerOutline }

/// Button with the design's sizes: 32 high (28 small), radius 8, 13px/500.
class AppButton extends StatefulWidget {
  const AppButton({
    super.key,
    required this.label,
    required this.onPressed,
    this.variant = ButtonVariant.outline,
    this.icon,
    this.small = false,
    this.trailingIcon,
  });

  final String label;
  final VoidCallback? onPressed;
  final ButtonVariant variant;
  final IconData? icon;
  final IconData? trailingIcon;
  final bool small;

  @override
  State<AppButton> createState() => _AppButtonState();
}

class _AppButtonState extends State<AppButton> {
  bool hover = false;

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    final enabled = widget.onPressed != null;
    final (Color bg, Color fg, Color? border) = switch (widget.variant) {
      ButtonVariant.primary => (t.brand, t.brandFg, null),
      ButtonVariant.destructive => (t.danger, t.onSolid, null),
      ButtonVariant.outline => (t.surface, t.fg, t.border),
      ButtonVariant.dangerOutline => (t.surface, t.danger, t.danger200),
      ButtonVariant.ghost => (t.transparent, t.fg2, null),
    };
    final solid =
        widget.variant == ButtonVariant.primary ||
        widget.variant == ButtonVariant.destructive;
    final hoverBg = solid ? Color.alphaBlend(t.pressOverlay, bg) : t.mutedBg;
    final h = widget.small ? AppSize.buttonSm : AppSize.button;
    final style = (widget.small ? AppText.xs : AppText.sm).copyWith(
      color: fg,
      fontWeight: AppText.medium,
    );
    final iconSize = widget.small ? 14.0 : 16.0;
    return Semantics(
      button: true,
      enabled: enabled,
      label: widget.label,
      child: MouseRegion(
        cursor: enabled ? SystemMouseCursors.click : SystemMouseCursors.basic,
        onEnter: (_) => setState(() => hover = true),
        onExit: (_) => setState(() => hover = false),
        child: GestureDetector(
          behavior: HitTestBehavior.opaque,
          onTap: widget.onPressed,
          child: Opacity(
            opacity: enabled ? 1 : 0.5,
            child: Container(
              height: h,
              padding: EdgeInsets.symmetric(horizontal: widget.small ? 10 : 12),
              decoration: BoxDecoration(
                color: hover && enabled ? hoverBg : bg,
                borderRadius: BorderRadius.circular(AppRadius.control),
                border: border == null ? null : Border.all(color: border),
              ),
              child: Row(
                mainAxisSize: MainAxisSize.min,
                children: [
                  if (widget.icon != null) ...[
                    Icon(widget.icon, size: iconSize, color: fg),
                    const SizedBox(width: 6),
                  ],
                  Text(widget.label, style: style),
                  if (widget.trailingIcon != null) ...[
                    const SizedBox(width: 4),
                    Icon(widget.trailingIcon, size: iconSize, color: fg),
                  ],
                ],
              ),
            ),
          ),
        ),
      ),
    );
  }
}

/// Square icon button (32×32), optional outline and tooltip.
class AppIconButton extends StatefulWidget {
  const AppIconButton({
    super.key,
    required this.icon,
    required this.onPressed,
    this.outline = false,
    this.tooltip,
    this.size = AppSize.button,
    this.color,
    this.active = false,
  });

  final IconData icon;
  final VoidCallback? onPressed;
  final bool outline;
  final String? tooltip;
  final double size;
  final Color? color;
  final bool active;

  @override
  State<AppIconButton> createState() => _AppIconButtonState();
}

class _AppIconButtonState extends State<AppIconButton> {
  bool hover = false;

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    final enabled = widget.onPressed != null;
    Widget b = Semantics(
      button: true,
      label: widget.tooltip,
      child: MouseRegion(
        cursor: enabled ? SystemMouseCursors.click : SystemMouseCursors.basic,
        onEnter: (_) => setState(() => hover = true),
        onExit: (_) => setState(() => hover = false),
        child: GestureDetector(
          behavior: HitTestBehavior.opaque,
          onTap: widget.onPressed,
          child: Opacity(
            opacity: enabled ? 1 : 0.5,
            child: Container(
              width: widget.size,
              height: widget.size,
              alignment: Alignment.center,
              decoration: BoxDecoration(
                color: (hover && enabled) || widget.active
                    ? t.mutedBg
                    : (widget.outline ? t.surface : null),
                borderRadius: BorderRadius.circular(AppRadius.control),
                border: widget.outline ? Border.all(color: t.border) : null,
              ),
              child: Icon(widget.icon, size: 16, color: widget.color ?? t.fg2),
            ),
          ),
        ),
      ),
    );
    if (widget.tooltip != null) {
      b = Tooltip(
        tooltip: (context) => TooltipContainer(child: Text(widget.tooltip!)),
        child: b,
      );
    }
    return b;
  }
}

class StatusDot extends StatelessWidget {
  const StatusDot(this.tone, {super.key, this.size = AppSize.dot});
  final Tone tone;
  final double size;

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    final c = tone == Tone.neutral ? t.fg3 : ToneColors.of(context, tone).fg;
    return Container(
      width: size,
      height: size,
      decoration: BoxDecoration(color: c, shape: BoxShape.circle),
    );
  }
}

/// 20px pill: 50 background, 200 border, tone text (12/500).
class StatusBadge extends StatelessWidget {
  const StatusBadge(
    this.text, {
    super.key,
    this.tone = Tone.neutral,
    this.dot = false,
    this.icon,
    this.height = 20,
  });

  final String text;
  final Tone tone;
  final bool dot;
  final IconData? icon;
  final double height;

  @override
  Widget build(BuildContext context) {
    final c = ToneColors.of(context, tone);
    return Container(
      height: height,
      padding: const EdgeInsets.symmetric(horizontal: 8),
      decoration: BoxDecoration(
        color: c.bg,
        borderRadius: BorderRadius.circular(AppRadius.badge),
        border: Border.all(color: c.border),
      ),
      child: Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          if (dot) ...[StatusDot(tone, size: 6), const SizedBox(width: 4)],
          if (icon != null) ...[
            Icon(icon, size: 12, color: c.fg),
            const SizedBox(width: 4),
          ],
          Text(
            text,
            style: AppText.xs.copyWith(
              color: c.fg,
              fontWeight: AppText.medium,
              height: 1.2,
            ),
          ),
        ],
      ),
    );
  }
}

/// 20px coloured square with the provider letter (C / O / A).
class TypeMark extends StatelessWidget {
  const TypeMark(this.type, {super.key, this.dim = false});
  final String? type;
  final bool dim;

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    final (Color c, String l) = switch (type) {
      'codex_app_server' => (t.typeCodex, 'C'),
      'grok_build' => (t.typeCodex, 'G'),
      'antigravity_cli' => (t.typeCodex, 'A'),
      'anthropic_compatible' => (t.typeAnthropic, 'A'),
      'ollama_http' => (t.neutral, 'L'),
      _ => (t.typeOpenAI, 'O'),
    };
    return Container(
      width: 20,
      height: 20,
      alignment: Alignment.center,
      decoration: BoxDecoration(
        color: c,
        borderRadius: BorderRadius.circular(AppRadius.badge),
      ),
      child: Text(
        l,
        style: TextStyle(
          fontSize: 11,
          height: 1,
          fontWeight: AppText.semibold,
          color: t.typeMarkFg,
        ),
      ),
    );
  }
}

class AppCheckbox extends StatelessWidget {
  const AppCheckbox({super.key, required this.checked, this.onChanged});
  final bool checked;
  final ValueChanged<bool>? onChanged;

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    final enabled = onChanged != null;
    return MouseRegion(
      cursor: enabled ? SystemMouseCursors.click : SystemMouseCursors.basic,
      child: GestureDetector(
        behavior: HitTestBehavior.opaque,
        onTap: enabled ? () => onChanged!(!checked) : null,
        child: Opacity(
          opacity: enabled ? 1 : 0.6,
          child: Container(
            width: 16,
            height: 16,
            alignment: Alignment.center,
            decoration: BoxDecoration(
              color: checked ? t.brand : t.surface,
              borderRadius: BorderRadius.circular(4),
              border: Border.all(color: checked ? t.brand : t.checkboxBorder),
            ),
            child: checked
                ? Icon(LucideIcons.check, size: 12, color: t.brandFg)
                : null,
          ),
        ),
      ),
    );
  }
}

/// Small upper section heading (12/600, fg3).
class SectionLabel extends StatelessWidget {
  const SectionLabel(this.text, {super.key, this.top = 24});
  final String text;
  final double top;

  @override
  Widget build(BuildContext context) => Padding(
    padding: EdgeInsets.only(top: top, bottom: 8),
    child: Text(
      text,
      style: AppText.xs.copyWith(
        color: AppTokens.of(context).fg3,
        fontWeight: AppText.semibold,
        letterSpacing: 0.5,
      ),
    ),
  );
}

/// Key/value table: 120px label column, 40px rows with a bottom divider.
class KvTable extends StatelessWidget {
  const KvTable(this.rows, {super.key, this.labelWidth = 120});
  final List<(String, Widget)> rows;
  final double labelWidth;

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        for (final (k, v) in rows)
          Container(
            height: AppSize.kvRow,
            decoration: BoxDecoration(
              border: Border(bottom: BorderSide(color: t.border)),
            ),
            child: Row(
              children: [
                SizedBox(
                  width: labelWidth,
                  child: Text(k, style: AppText.sm.copyWith(color: t.fg2)),
                ),
                Expanded(
                  child: DefaultTextStyle.merge(
                    style: AppText.sm.copyWith(color: t.fg),
                    maxLines: 1,
                    overflow: TextOverflow.ellipsis,
                    child: Align(alignment: Alignment.centerLeft, child: v),
                  ),
                ),
              ],
            ),
          ),
      ],
    );
  }
}

/// 48px tinted icon block, 16px title, 13px text (max 380), action.
class EmptyState extends StatelessWidget {
  const EmptyState({
    super.key,
    required this.icon,
    required this.title,
    required this.message,
    this.action,
  });
  final IconData icon;
  final String title;
  final String message;
  final Widget? action;

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    return Center(
      child: Padding(
        padding: const EdgeInsets.all(AppSpace.s24),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            IconTile(icon, size: 48),
            const SizedBox(height: 16),
            Text(title, style: AppText.lg.copyWith(color: t.fg)),
            const SizedBox(height: 4),
            ConstrainedBox(
              constraints: const BoxConstraints(maxWidth: 380),
              child: Text(
                message,
                textAlign: TextAlign.center,
                style: AppText.sm.copyWith(color: t.fg2),
              ),
            ),
            if (action != null) ...[const SizedBox(height: 16), action!],
          ],
        ),
      ),
    );
  }
}

/// Rounded tinted square with an icon (brand by default).
class IconTile extends StatelessWidget {
  const IconTile(this.icon, {super.key, this.size = 40, this.tone});
  final IconData icon;
  final double size;
  final Tone? tone;

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    final c = tone == null ? null : ToneColors.of(context, tone!);
    return Container(
      width: size,
      height: size,
      alignment: Alignment.center,
      decoration: BoxDecoration(
        color: c?.bg ?? t.brand50,
        borderRadius: BorderRadius.circular(AppRadius.card),
      ),
      child: Icon(icon, size: size / 2, color: c?.fg ?? t.brand),
    );
  }
}

/// Inline alert: tone background/border, 16px icon, bold title + text.
class AppAlert extends StatelessWidget {
  const AppAlert({
    super.key,
    required this.tone,
    this.title,
    this.message,
    this.icon,
    this.compact = false,
    this.inline = false,
    this.trailing,
  });
  final bool inline;
  final Tone tone;
  final String? title;
  final String? message;
  final IconData? icon;
  final bool compact;
  final Widget? trailing;

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    final c = ToneColors.of(context, tone);
    final text = switch (tone) {
      Tone.success => t.successFg,
      Tone.warning => t.warningFg,
      Tone.danger => t.dangerFg,
      _ => t.fg,
    };
    final ic =
        icon ??
        (tone == Tone.success ? LucideIcons.check : LucideIcons.triangleAlert);
    final inline = this.inline && title != null && message != null;
    return Container(
      padding: compact
          ? const EdgeInsets.symmetric(horizontal: 12, vertical: 8)
          : const EdgeInsets.symmetric(horizontal: 16, vertical: 12),
      decoration: BoxDecoration(
        color: c.bg,
        borderRadius: BorderRadius.circular(AppRadius.control),
        border: Border.all(color: c.border),
      ),
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Padding(
            padding: const EdgeInsets.only(top: 2),
            child: Icon(
              ic,
              size: 16,
              color: tone == Tone.success ? c.fg : text,
            ),
          ),
          const SizedBox(width: 12),
          Expanded(
            child: inline
                ? Text.rich(
                    TextSpan(
                      children: [
                        TextSpan(
                          text: title,
                          style: const TextStyle(fontWeight: AppText.semibold),
                        ),
                        TextSpan(text: ' · $message'),
                      ],
                    ),
                    style: AppText.sm.copyWith(color: text),
                  )
                : Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      if (title != null)
                        Text(
                          title!,
                          style: AppText.sm.copyWith(
                            color: text,
                            fontWeight: AppText.semibold,
                          ),
                        ),
                      if (message != null)
                        Text(message!, style: AppText.sm.copyWith(color: text)),
                    ],
                  ),
          ),
          if (trailing != null) ...[const SizedBox(width: 8), trailing!],
        ],
      ),
    );
  }
}

class AppCard extends StatelessWidget {
  const AppCard({
    super.key,
    required this.child,
    this.padding = const EdgeInsets.all(16),
    this.subtle = false,
  });
  final Widget child;
  final EdgeInsetsGeometry padding;
  final bool subtle;

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    return Container(
      padding: padding,
      decoration: BoxDecoration(
        color: subtle ? t.subtle : t.surface,
        borderRadius: BorderRadius.circular(AppRadius.card),
        border: Border.all(color: t.border),
      ),
      child: child,
    );
  }
}

/// Static progress arc (no animation, so it never blocks settling).
class SpinnerArc extends StatelessWidget {
  const SpinnerArc({super.key, this.size = 14});
  final double size;

  @override
  Widget build(BuildContext context) {
    final t = AppTokens.of(context);
    return SizedBox(
      width: size,
      height: size,
      child: CustomPaint(painter: _ArcPainter(t.brand100, t.brand)),
    );
  }
}

class _ArcPainter extends CustomPainter {
  _ArcPainter(this.track, this.head);
  final Color track, head;

  @override
  void paint(Canvas canvas, Size size) {
    final p = Paint()
      ..style = PaintingStyle.stroke
      ..strokeWidth = 2;
    final r = Offset.zero & size;
    canvas.drawArc(r.deflate(1), 0, 2 * math.pi, false, p..color = track);
    canvas.drawArc(
      r.deflate(1),
      -math.pi / 2,
      math.pi / 2,
      false,
      p..color = head,
    );
  }

  @override
  bool shouldRepaint(_ArcPainter old) => old.track != track || old.head != head;
}

/// 1px divider in the border colour.
class Hairline extends StatelessWidget {
  const Hairline({super.key, this.vertical = false, this.length});
  final bool vertical;
  final double? length;

  @override
  Widget build(BuildContext context) {
    final c = AppTokens.of(context).border;
    return vertical
        ? Container(width: 1, height: length, color: c)
        : Container(height: 1, width: length, color: c);
  }
}

/// Monospace text style (GeistMono from shadcn_flutter).
// Built with `package:` (not a pre-prefixed family name): an inherited
// DefaultTextStyle from a package font would otherwise prefix it twice.
TextStyle monoStyle(TextStyle base) => base.merge(
  const TextStyle(fontFamily: AppText.mono, package: AppText.monoPackage),
);
