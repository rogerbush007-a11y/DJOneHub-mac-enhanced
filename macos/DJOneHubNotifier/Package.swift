// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "DJOneHubNotifier",
    platforms: [.macOS(.v13)],
    products: [
        .executable(name: "DJOneHubNotifier", targets: ["DJOneHubNotifier"]),
    ],
    targets: [
        .target(
            name: "CModemBridge",
            path: "Sources/CModemBridge",
            publicHeadersPath: "include",
            linkerSettings: [
                .linkedFramework("CoreFoundation"),
                .linkedFramework("IOKit"),
            ]
        ),
        .target(
            name: "CUACProbe",
            path: "Sources/CUACProbe",
            publicHeadersPath: "include",
            linkerSettings: [
                .linkedFramework("CoreAudio"),
                .linkedFramework("CoreFoundation"),
                .linkedFramework("IOKit"),
            ]
        ),
        .executableTarget(
            name: "DJOneHubNotifier",
            dependencies: ["CModemBridge", "CUACProbe"],
            path: "Sources/DJOneHubNotifier",
            swiftSettings: [.swiftLanguageMode(.v5)],
            linkerSettings: [
                .linkedFramework("ServiceManagement"),
            ]
        ),
    ]
)
