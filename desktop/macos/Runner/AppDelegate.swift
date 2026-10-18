import Cocoa
import FlutterMacOS

@main
class AppDelegate: FlutterAppDelegate {
  private var statusItem: NSStatusItem?
  private var trayChannel: FlutterMethodChannel?
  /// Dart finished the graceful kernel shutdown: terminating is allowed now.
  private var quitReady = false
  /// A Cmd-Q / Dock / system quit is waiting for that shutdown.
  private var terminatePending = false
  /// Upper bound for the Dart quit flow (its own kernel stop timeout is 5 s).
  private static let quitFallbackSeconds: TimeInterval = 8

  override func applicationDidFinishLaunching(_ notification: Notification) {
    if let controller = mainFlutterWindow?.contentViewController as? FlutterViewController {
      trayChannel = FlutterMethodChannel(
        name: "avora/tray", binaryMessenger: controller.engine.binaryMessenger)
      trayChannel?.setMethodCallHandler { [weak self] call, result in
        switch call.method {
        case "hideDock":
          NSApp.setActivationPolicy(.accessory)
          result(nil)
        case "showDock":
          NSApp.setActivationPolicy(.regular)
          result(nil)
        case "quitReady":
          self?.allowTerminate()
          result(nil)
        default:
          result(FlutterMethodNotImplemented)
        }
      }
    }
    setupStatusItem()
    super.applicationDidFinishLaunching(notification)
  }

  // 系统托盘：左键点击直接弹出「配置 / 退出」菜单。
  private func setupStatusItem() {
    statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
    if let button = statusItem?.button {
      button.image = Self.trayImage()
      button.toolTip = "Avora"
    }
    let menu = NSMenu()
    let configItem = NSMenuItem(title: "配置", action: #selector(trayConfig), keyEquivalent: "")
    configItem.target = self
    let quitItem = NSMenuItem(title: "退出", action: #selector(trayQuit), keyEquivalent: "")
    quitItem.target = self
    menu.addItem(configItem)
    menu.addItem(.separator())
    menu.addItem(quitItem)
    statusItem?.menu = menu
  }

  // 与应用图标使用同一连接标识；模板图由系统适配菜单栏深浅色。
  private static func trayImage() -> NSImage {
    let image = NSImage(named: NSImage.Name("TrayIcon"))
      ?? NSImage(systemSymbolName: "link", accessibilityDescription: "Avora")
      ?? NSImage()
    image.size = NSSize(width: 18, height: 18)
    image.isTemplate = true
    return image
  }

  @objc private func trayConfig() { trayChannel?.invokeMethod("config", arguments: nil) }
  @objc private func trayQuit() { trayChannel?.invokeMethod("quit", arguments: nil) }

  // Cmd-Q, the app menu, the Dock and logout all end here (audit3 P2-11):
  // they must run the same timeout-protected graceful kernel shutdown as
  // the tray's 退出, not kill the process with the kernel still running.
  override func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
    if quitReady || trayChannel == nil {
      return .terminateNow
    }
    if !terminatePending {
      terminatePending = true
      trayChannel?.invokeMethod("quit", arguments: nil)
      DispatchQueue.main.asyncAfter(deadline: .now() + Self.quitFallbackSeconds) { [weak self] in
        self?.allowTerminate()  // the Dart side hung: quit anyway
      }
    }
    return .terminateLater
  }

  private func allowTerminate() {
    quitReady = true
    if terminatePending {
      terminatePending = false
      NSApp.reply(toApplicationShouldTerminate: true)
    }
  }

  // 托盘常驻：关闭最后一个窗口不退出进程，退出只走托盘菜单。
  override func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
    return false
  }

  override func applicationSupportsSecureRestorableState(_ app: NSApplication) -> Bool {
    return true
  }
}
