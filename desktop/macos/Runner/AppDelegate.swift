import Cocoa
import FlutterMacOS

@main
class AppDelegate: FlutterAppDelegate {
  private var statusItem: NSStatusItem?
  private var trayChannel: FlutterMethodChannel?

  override func applicationDidFinishLaunching(_ notification: Notification) {
    if let controller = mainFlutterWindow?.contentViewController as? FlutterViewController {
      trayChannel = FlutterMethodChannel(
        name: "localsubproxy/tray", binaryMessenger: controller.engine.binaryMessenger)
      trayChannel?.setMethodCallHandler { call, result in
        switch call.method {
        case "hideDock":
          NSApp.setActivationPolicy(.accessory)
          result(nil)
        case "showDock":
          NSApp.setActivationPolicy(.regular)
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
      button.toolTip = "LocalSubProxy"
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
      ?? NSImage(systemSymbolName: "link", accessibilityDescription: "LocalSubProxy")
      ?? NSImage()
    image.size = NSSize(width: 18, height: 18)
    image.isTemplate = true
    return image
  }

  @objc private func trayConfig() { trayChannel?.invokeMethod("config", arguments: nil) }
  @objc private func trayQuit() { trayChannel?.invokeMethod("quit", arguments: nil) }

  // 托盘常驻：关闭最后一个窗口不退出进程，退出只走托盘菜单。
  override func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
    return false
  }

  override func applicationSupportsSecureRestorableState(_ app: NSApplication) -> Bool {
    return true
  }
}
