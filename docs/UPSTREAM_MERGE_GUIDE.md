# 上游版本合并与 macOS 架构维护指南

本文档记录如何将上游项目 [rogerbush007-a11y/DJOneHub-mac-enhanced](https://github.com/rogerbush007-a11y/DJOneHub-mac-enhanced) 的最新改动合并到本 fork（[MiQieR/DJOneHub-mac-enhanced-dmg](https://github.com/MiQieR/DJOneHub-mac-enhanced-dmg)），并确保本仓库特有的**标准 macOS 应用架构（.app + 状态栏启动器 + 拖拽 DMG 安装）**不被破坏或回退。

> [!IMPORTANT]
> **给后续维护者 / AI Agent 的核心警示**：
> 1. 本 fork 的核心定位是提供**符合 macOS 规范的标准 GUI 应用体验**，彻底摒弃上游原版的命令行脚本安装与 LaunchAgent 强制开机自启。
> 2. **严禁**在合并代码时回退到上游的 `.command` 脚本打包模式或重新引入 LaunchAgent 开机自启。
> 3. 所有发布的 Release 链接和文档说明必须指向本 fork 仓库：`https://github.com/MiQieR/DJOneHub-mac-enhanced-dmg`。

---

## 一、 为什么改造？（原始痛点与设计初衷）

上游原版（v0.1.3 及之前）虽然实现了核心 4G 模组管理功能，但在 macOS 系统集成上存在以下严重痛点：

| 痛点问题 | 上游原始机制 | 本 Fork 标准化改造方案 |
| --- | --- | --- |
| **开机自启与资源常驻** | 默认通过 2 个 LaunchAgent（`KeepAlive=true`）强制开机自启，常驻占用 CPU（约 0.8%）和内存（约 70MB）。 | **移除强制开机自启**；在状态栏右键菜单中提供原生 `SMAppService` 开机自启开关，按需自选。 |
| **开机启动时序误报** | 通知助手与主服务分别自启，通知助手往往先于主服务启动，导致开机必定弹窗提示“等待主程序启动/未连接”。 | **统一生命周期调度**：Swift 启动器先启动 Go 后端，等待 HTTP (`127.0.0.1:7575`) 端口就绪后再拉起通知助手，彻底消除误报。 |
| **启动与退出途径** | 没有常规的图形界面启动/关闭途径；一旦关闭自启，用户无法通过点击图标启动；运行中也无法正常退出。 | **标准 App 启动 + 状态栏常驻**：点击 `/Applications/DJOneHub.app` 启动，状态栏右键菜单一键优雅退出（同时安全释放两个子进程）。 |
| **安装与卸载体验** | 依赖 DMG 内部的 `安装 DJOneHub.command` / `卸载 DJOneHub.command` 终端脚本，甚至需 `sudo` 提权安装到 `/usr/local/`。 | **标准 macOS 拖拽安装**：双击 DMG 拖拽到 `/Applications` 即可；卸载只需将 App 移入废纸篓。 |
| **文件分布零散** | 文件分散于 `~/Library/Application Support/`、`~/Library/Logs/`、`~/Library/LaunchAgents/`、`/usr/local/` 等处，极难彻底清理。 | **集中管理**：配置、运行时日志（`logs/`）全部收拢在 `~/Library/Application Support/DJOneHub/`，卸载一键清理。 |

---

## 二、 我们的核心架构与关键资产（切勿覆盖或删除）

### 1. 软件运行架构与 App Bundle 结构

```text
DJOneHub.app (标准 macOS Application Bundle)
├── Contents/
│   ├── Info.plist                  ← LSUIElement=true (无 Dock 图标，纯状态栏常驻)
│   ├── PkgInfo                     ← APPL????
│   ├── MacOS/
│   │   └── DJOneHub                ← Swift 启动器主程序 (DJOneHubLauncher)
│   └── Resources/
│       ├── DJOneHub.icns           ← macOS 标准应用图标 (由 DJOneHub-app.png 生成)
│       ├── StatusIcon.png          ← 状态栏图标 @1x (18×18，由 status-icon.svg 转换)
│       ├── StatusIcon@2x.png       ← 状态栏图标 @2x (36×36)
│       ├── djonehub-macos          ← Go 后端 Universal 二进制文件
│       ├── lib/
│       │   └── libusb-1.0.0.dylib  ← 运行时动态链接库
│       └── DJOneHubNotifier.app/   ← 原生通知助手 Bundle (作为内嵌辅助子应用)
```

### 2. 核心源码与资产清单

- **Swift 启动器**：`macos/DJOneHubLauncher/`
  - `AppMain.swift`: 入口点，设置 `.accessory` 激活策略（无 Dock 图标）。
  - `AppDelegate.swift`: 负责状态栏图标注册与点击事件响应（左键打开 Web 面板，右键弹出菜单）。
  - `ProcessManager.swift`: 负责 Go 后端与通知助手的顺序启动、就绪探测（轮询 `127.0.0.1:7575`）及退出时的子进程优雅终止（SIGTERM -> SIGKILL）。
  - `AutoLaunch.swift`: 封装 macOS 13+ 的 `SMAppService.mainApp`，实现菜单栏开机自启开关。
- **图标资产**：
  - `resources/DJOneHub-app.png`: 应用高清原图。
  - `resources/DJOneHub.icns`: 完整的 macOS 应用图标集合。
  - `resources/status-icon.svg`: 状态栏矢量图标。
  - `resources/StatusIcon.png` & `resources/StatusIcon@2x.png`: 纯黑透明底单色模板图像（`isTemplate = true`，macOS 自动适配深浅色模式）。
- **打包流水线脚本**：
  - `scripts/build-launcher.sh`: 编译 Universal (arm64 + x86_64) 的 `DJOneHubLauncher`。
  - `scripts/create-app-bundle.sh`: 组装 `DJOneHub.app` 并完成深度代码签名。
  - `scripts/build-dmg-universal.sh`: 生成标准的 Universal 拖拽式 DMG 安装镜像（含 `/Applications` 软链接快捷方式）。
  - `scripts/build-dmg.sh`: 生成 arm64 单架构 DMG 安装镜像。
  - `tools/libusb_stub.c`: 离线或交叉编译时提供的 x86_64 符号存根，保障 Universal 构建稳定性。

### 3. 已被废弃的上游遗留文件（合并时严禁重新引入）

- ❌ `scripts/dmg/安装 DJOneHub.command`
- ❌ `scripts/dmg/卸载 DJOneHub.command`
- ❌ `scripts/dmg/使用说明.txt`
- ❌ `packaging/install`
- ❌ `packaging/djonehub`
- ❌ `macos/DJOneHubNotifier/com.jamie.djonehub-notifier.plist`

---

## 三、 上游合并标准流程（SOP）

### 1. 准备工作

```sh
# 1. 检查并确保 upstream remote 存在
git remote -v
# 若无 upstream 则添加：
git remote add upstream https://github.com/rogerbush007-a11y/DJOneHub-mac-enhanced.git

# 2. 拉取上游最新分支与 Tag
git fetch upstream --tags

# 3. 查看上游的新增提交
git log --oneline <上次上游提交SHA>..upstream/main

# 4. 概览改动文件（排除 third_party）
git diff <上次上游提交SHA> upstream/main --stat -- . ':(exclude)third_party/**'
```

### 2. 执行合并

```sh
# 确保当前工作树干净
git merge upstream/main --no-edit
```

### 3. 冲突解决与防倒退检查

#### ① `README.md` 冲突处理
上游维护的是“脚本安装 + 历史下载列表”，我们维护的是“标准 App 拖拽安装”。
- **保留**本 fork 的标准 App 安装说明与交互介绍。
- **更新**版本号为上游新版本，并同步新版本特性介绍。
- **确保** Releases 链接指向本 fork：`https://github.com/MiQieR/DJOneHub-mac-enhanced-dmg/releases`。

示例参考模板：
```markdown
> [!IMPORTANT]
> 📦 **标准应用安装包已上传**：[Releases](https://github.com/MiQieR/DJOneHub-mac-enhanced-dmg/releases) 提供标准 macOS DMG 安装包，双击打开后拖拽 `DJOneHub.app` 到 `Applications` 目录即可完成安装：
> - `DJOneHub-macOS-universal-v<新版本>.dmg`：基于上游 v<新版本>，支持 Apple Silicon 与 Intel Mac 通用双架构；<新特性说明>
```

#### ② 检查上游是否改动了打包脚本或重新引入了废弃文件
如果上游提交重新添加了 `packaging/install`、`scripts/dmg/*.command` 或 LaunchAgent plist：
```sh
# 直接删除上游废弃文件
rm -f "scripts/dmg/安装 DJOneHub.command" "scripts/dmg/卸载 DJOneHub.command" "scripts/dmg/使用说明.txt" packaging/install packaging/djonehub macos/DJOneHubNotifier/com.jamie.djonehub-notifier.plist
```

#### ③ 确认无冲突标记残留
```sh
grep -rn '^<<<<<<<\|^>>>>>>>' cmd/ internal/ pkg/ scripts/ macos/ README.md
```

### 4. 编译与测试验证

```sh
# 1. Go 后端编译与静态检查
go build ./... && go vet ./cmd/...

# 2. 运行所有单元测试
go test ./...

# 3. 编译 Swift 启动器验证
./scripts/build-launcher.sh universal
```

### 5. 生成并验证 DMG 安装包

```sh
# 编译 Universal DMG 安装包
./scripts/build-dmg-universal.sh v<新版本>-preview
```

产物路径：`dist/DJOneHub-macOS-universal-v<新版本>-preview.dmg`

#### 产物完整性校验：
```sh
DMG=dist/DJOneHub-macOS-universal-v<新版本>-preview.dmg
hdiutil attach "$DMG" -nobrowse -quiet -mountpoint /tmp/djoh-check
APP=/tmp/djoh-check/DJOneHub.app

# 1. 验证双架构支持
file "$APP/Contents/MacOS/DJOneHub"
file "$APP/Contents/Resources/djonehub-macos"

# 2. 验证代码签名
codesign --verify --deep --strict "$APP" && echo "✅ 签名有效"

# 3. 验证 Applications 快捷方式软链接存在
[ -L /tmp/djoh-check/Applications ] && echo "✅ 拖拽快捷方式存在"

# 4. 卸载检查盘
hdiutil detach /tmp/djoh-check -quiet
```

### 6. 提交与推送

```sh
git add -A
git commit -m "Merge upstream/main into main (v<新版本>)"
git push origin main
```

---

## 四、 常见避坑指南（Gotchas & Redlines）

1. **红线：不要回退安装说明与打包逻辑**
   - 上游更新时，若上游修改了 `packaging/` 或 `scripts/`，不要直接覆盖我们的 `scripts/create-app-bundle.sh`、`scripts/build-dmg-universal.sh` 和 `packaging/Info.plist`。
2. **红线：仓库 URL 保持本 fork**
   - 不要把 `README.md` 或文档中的 Release 链接改回 `rogerbush007-a11y`。
3. **上游 API / 端口变动注意**
   - 启动器 (`ProcessManager.swift`) 默认探测 `http://127.0.0.1:7575/`。若上游调整了端口或服务就绪判断逻辑，需同步检查 `ProcessManager.swift`。
4. **编译环境变量（SDKROOT & CC）**
   - 在 macOS 上使用 CGO 交叉编译时，确保 `package-macos-universal.sh` 中包含 `SDKROOT="$(xcrun --show-sdk-path)"` 与 `CC="$(xcrun -f clang)"`，避免受第三方工具链（如 swiftly 等）拦截干扰。

---

## 五、 历史合并记录

| 版本 | 合并提交 | 上游对应 SHA | 核心更新说明 |
| --- | --- | --- | --- |
| **v0.1.3** | `12c033f` | `0adfa8c` | **【核心改造】** 实现标准 macOS .app 架构、Swift 状态栏启动器、开机自启开关、拖拽 DMG 安装。 |
| **v0.1.5** | `af9bd04` | `9644f17` | 合并上游：4G DHCP 自动续租、启动时 DHCP 修复、Windows 实验版支持。 |
| **v0.1.7** | `9b566f8` | `0bae842` | 合并上游：蜂窝信号自检与自动找回、USB 打开超时保护、自动创建/启用 4G 网卡服务。 |
| **v1.2.9** | *(本次)* | `256a719` | **【重大架构升级】** 合并上游 v1.2.4-v1.2.9：通话、短信增强、通讯录、iPhone/iPad 模式、动态状态栏图标、语音运行时。方案 A：废弃 DJOneHubLauncher，DJOneHubNotifier 升级为主 App，删除 `ensureModuleServices()` LaunchAgent 机制（待机耗电根因），新增 ProcessManager.swift 直接管理 Go 后端进程，开机自启改为 SMAppService，图标更新为新版。 |

> *下次执行合并时，请将上表作为参考，并在完成合并后在本文档底部追加新的合并记录。*
