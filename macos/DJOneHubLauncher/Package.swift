// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "DJOneHubLauncher",
    platforms: [.macOS(.v13)],
    products: [
        .executable(name: "DJOneHubLauncher", targets: ["DJOneHubLauncher"]),
    ],
    targets: [
        .executableTarget(
            name: "DJOneHubLauncher",
            path: "Sources/DJOneHubLauncher"
        ),
    ]
)
