import 'package:shadcn_flutter/shadcn_flutter.dart';

/// Design tokens (docs: 客户端设计稿说明). Every colour, type size, spacing
/// step and radius used by the UI comes from here; widgets never hard-code
/// colours. [AppTokens.of] follows the active theme brightness, so the dark
/// theme is the same layout with the dark token set.
@immutable
class AppTokens {
  const AppTokens({
    required this.bg,
    this.onSolid = const Color(0xFFFFFFFF),
    this.transparent = const Color(0x00000000),
    this.pressOverlay = const Color(0x1A000000),
    required this.surface,
    required this.subtle,
    required this.mutedBg,
    required this.border,
    required this.fg,
    required this.fg2,
    required this.fg3,
    required this.brand,
    required this.brandFg,
    required this.brand50,
    required this.brand100,
    required this.success,
    required this.success50,
    required this.success200,
    required this.successFg,
    required this.warning,
    required this.warning50,
    required this.warning200,
    required this.warningFg,
    required this.danger,
    required this.danger50,
    required this.danger200,
    required this.dangerFg,
    required this.neutral,
    required this.neutral50,
    required this.checkboxBorder,
    required this.scrim,
    required this.shadow,
    required this.typeCodex,
    required this.typeOpenAI,
    required this.typeAnthropic,
    required this.typeMarkFg,
  });

  /// Text on solid danger fills; fully transparent; pressed-state tint.
  final Color onSolid, transparent, pressOverlay;
  final Color bg, surface, subtle, mutedBg, border, fg, fg2, fg3;
  final Color brand, brandFg, brand50, brand100;
  final Color success, success50, success200, successFg;
  final Color warning, warning50, warning200, warningFg;
  final Color danger, danger50, danger200, dangerFg;
  final Color neutral, neutral50, checkboxBorder, scrim, shadow;
  final Color typeCodex, typeOpenAI, typeAnthropic, typeMarkFg;

  static const light = AppTokens(
    bg: Color(0xFFFFFFFF),
    surface: Color(0xFFFFFFFF),
    subtle: Color(0xFFF8F9FB),
    mutedBg: Color(0xFFF3F4F6),
    border: Color(0xFFE5E7EB),
    fg: Color(0xFF111827),
    fg2: Color(0xFF4B5563),
    fg3: Color(0xFF9CA3AF),
    brand: Color(0xFF4D25D8),
    brandFg: Color(0xFFFFFFFF),
    brand50: Color(0xFFF1EDFF),
    brand100: Color(0xFFE4DCFF),
    success: Color(0xFF16A34A),
    success50: Color(0xFFECFDF5),
    success200: Color(0xFFBBF7D0),
    successFg: Color(0xFF14532D),
    warning: Color(0xFFD97706),
    warning50: Color(0xFFFFFBEB),
    warning200: Color(0xFFFDE68A),
    warningFg: Color(0xFF92400E),
    danger: Color(0xFFDC2626),
    danger50: Color(0xFFFEF2F2),
    danger200: Color(0xFFFECACA),
    dangerFg: Color(0xFF991B1B),
    neutral: Color(0xFF6B7280),
    neutral50: Color(0xFFF3F4F6),
    checkboxBorder: Color(0xFFC4C9D2),
    scrim: Color(0x66111827),
    shadow: Color(0x29111827),
    typeCodex: Color(0xFF111827),
    typeOpenAI: Color(0xFF10A37F),
    typeAnthropic: Color(0xFFC96442),
    typeMarkFg: Color(0xFFFFFFFF),
  );

  static const dark = AppTokens(
    bg: Color(0xFF0B0D12),
    surface: Color(0xFF12151C),
    subtle: Color(0xFF161A22),
    mutedBg: Color(0xFF1C212B),
    border: Color(0xFF262C38),
    fg: Color(0xFFE5E7EB),
    fg2: Color(0xFFA1A8B5),
    fg3: Color(0xFF6B7280),
    brand: Color(0xFFAB94FF),
    brandFg: Color(0xFF0C1224),
    brand50: Color(0xFF211A3D),
    brand100: Color(0xFF2E2457),
    success: Color(0xFF4ADE80),
    success50: Color(0xFF0F2A1B),
    success200: Color(0xFF1F5134),
    successFg: Color(0xFF4ADE80),
    warning: Color(0xFFFBBF24),
    warning50: Color(0xFF2B2210),
    warning200: Color(0xFF5A4515),
    warningFg: Color(0xFFFCD34D),
    danger: Color(0xFFF87171),
    danger50: Color(0xFF2D1414),
    danger200: Color(0xFF5B2323),
    dangerFg: Color(0xFFFCA5A5),
    neutral: Color(0xFF9CA3AF),
    neutral50: Color(0xFF1C212B),
    checkboxBorder: Color(0xFF4B5563),
    scrim: Color(0x99000000),
    shadow: Color(0x80000000),
    typeCodex: Color(0xFF374151),
    typeOpenAI: Color(0xFF10A37F),
    typeAnthropic: Color(0xFFC96442),
    typeMarkFg: Color(0xFFFFFFFF),
  );

  static AppTokens of(BuildContext context) =>
      Theme.of(context).brightness == Brightness.dark ? dark : light;

  /// shadcn colour scheme derived from the tokens, so built-in widgets
  /// (selects, text fields, toasts) match.
  ColorScheme colorScheme(Brightness b) {
    final base = b == Brightness.dark
        ? ColorSchemes.darkZinc
        : ColorSchemes.lightZinc;
    return base.copyWith(
      background: () => bg,
      foreground: () => fg,
      card: () => surface,
      cardForeground: () => fg,
      popover: () => surface,
      popoverForeground: () => fg,
      primary: () => brand,
      primaryForeground: () => brandFg,
      secondary: () => mutedBg,
      secondaryForeground: () => fg,
      muted: () => mutedBg,
      mutedForeground: () => fg2,
      accent: () => mutedBg,
      accentForeground: () => fg,
      destructive: () => danger,
      border: () => border,
      input: () => border,
      ring: () => brand,
    );
  }
}

/// Type scale: 5 steps only (12 / 13 / 14 / 16 / 20), weights 400/500/600.
abstract final class AppText {
  static const xs = TextStyle(fontSize: 12, height: 16 / 12);
  static const sm = TextStyle(fontSize: 13, height: 20 / 13);
  static const md = TextStyle(fontSize: 14, height: 20 / 14);
  static const lg = TextStyle(
    fontSize: 16,
    height: 24 / 16,
    fontWeight: FontWeight.w600,
  );
  static const xl = TextStyle(
    fontSize: 20,
    height: 28 / 20,
    fontWeight: FontWeight.w600,
  );
  static const regular = FontWeight.w400;
  static const medium = FontWeight.w500;
  static const semibold = FontWeight.w600;

  /// Monospace for pairing codes, paths and key ids (bundled with shadcn).
  static const mono = 'GeistMono';
  static const monoPackage = 'shadcn_flutter';
}

/// 4px spacing grid.
abstract final class AppSpace {
  static const s4 = 4.0;
  static const s8 = 8.0;
  static const s12 = 12.0;
  static const s16 = 16.0;
  static const s24 = 24.0;
  static const s32 = 32.0;
  static const page = 24.0;
}

/// Fixed component sizes.
abstract final class AppSize {
  static const header = 56.0;
  static const statusBar = 56.0;
  static const tableHeader = 40.0;
  static const row = 56.0;
  static const kvRow = 40.0;
  static const button = 32.0;
  static const buttonSm = 28.0;
  static const input = 36.0;
  static const actionBar = 64.0;
  static const banner = 40.0;
  static const dot = 8.0;
}

abstract final class AppRadius {
  static const badge = 6.0;
  static const control = 8.0;
  static const card = 12.0;
}
