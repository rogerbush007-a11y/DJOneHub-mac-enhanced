# DJOneHub for macOS

This branch adds a native macOS service for the DJI Cellular Dongle / Quectel
EG25-G. It does not require UTM for AT-mode management.

## Current scope

- Automatic discovery of DJI (`2ca3`) and Quectel (`2c7c`) USB serial ports
- Modem, SIM, operator, registration and signal status
- Receive and send SMS through the modem AT port
- Execute explicit AT commands
- Read and switch physical eUICC profiles through AT APDU transport
- Local management page at `http://127.0.0.1:7575`
- Packaged Apple Silicon release (Intel packaging is planned separately)

The cellular data interface remains managed by macOS. This allows macOS to use
the dongle as its network connection while DJOneHub uses a separate USB serial
interface for management.

## Downloaded release

DJOneHub is distributed as a standard macOS Disk Image (`.dmg`).

1. Open `DJOneHub-macOS-universal-v0.1.3-preview.dmg`.
2. Drag `DJOneHub.app` into `/Applications`.
3. Open `DJOneHub` from Applications or Launchpad.

The launcher app will run in the menu bar:
- **Left click**: Opens `http://127.0.0.1:7575` in your browser.
- **Right click**: Context menu to open web interface, toggle auto-launch at login, or quit application.

Logs are stored in `~/Library/Application Support/DJOneHub/logs/`.

## Build from source

Requirements:

- macOS 13 or newer
- Go 1.26 or newer
- Xcode / Command Line Tools

Build DMG release (Universal binary arm64 + x86_64):

```sh
./scripts/build-dmg-universal.sh v0.1.3-preview
```

Release outputs:

- `dist/DJOneHub.app`
- `dist/DJOneHub-macOS-universal-v0.1.3-preview.dmg`

Build App Bundle directly:

```sh
./scripts/create-app-bundle.sh v0.1.3-preview universal
```

## Platform limitations

- Native QMI/MBIM control, Linux udev and network-namespace orchestration are
  excluded from this macOS entry point.
- eSIM behavior depends on the physical eUICC and modem firmware. Profile
  switching must be verified with real hardware.
- The release uses an ad-hoc signature rather than an Apple Developer ID. On
  first run, macOS may require approval in Privacy & Security.
