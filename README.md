# DJOneHub

> 让大疆第一代 4G 模块成为 Mac 上长期可用的实体 SIM 终端。

DJOneHub 是一个非官方开源项目。它通过模块已有 USB 接口提供短信、4G、GPS、eSIM、来电及通话控制，不修改模块固件。

## 文档导航

- [当前版本与更新](#v1210iphone--ipad-网络唤醒)
- [版本演进](#版本演进)
- [下载与安装](#下载与平台状态)
- [完整使用说明](#完整使用说明)
- [常用命令](#常用命令)
- [日志卸载与故障排查](#日志与本地数据)
- [从源码构建](#从源码构建)

主页同时保留当前版本说明和早期使用文档。标有“历史”的内容用于说明版本演进；当前安装与操作请以 v1.2.10 章节为准。

## v1.2.10：iPhone / iPad 网络唤醒

[下载 v1.2.10](https://github.com/rogerbush007-a11y/DJOneHub-mac-enhanced/releases/tag/v1.2.10)

- 切换到 iPhone/iPad 模式时，由 Mac 一次性安装公开的模块侧网络唤醒服务；手机无需安装 DJOneHub App。
- 手机锁屏、普通 App 被系统挂起后，模块仍维持 USB ECM 与蜂窝数据链路，减少长时间空闲后断网。
- 拔出移动设备或恢复 Mac 完整模式后自动停止活跃保留并释放 wake lock。
- 首次启用需要模块当前以带 ADB 的 Mac 模式连接；安装完成后可脱离 Mac 使用。

## v1.2.9：v1.2.5 — v1.2.9 更新汇总

[下载 v1.2.9](https://github.com/rogerbush007-a11y/DJOneHub-mac-enhanced/releases/tag/v1.2.9)

### 通话与首次启用

- 独立 macOS App 集中管理拨号、接听、拒接、挂断、DTMF、通话记录、录音入口、短信、通讯录与设置；来电、短信提醒不需要网页常驻。
- 新模块、原始 DJI 配置、旧 UAC 与其他工具留下的完整 USB 配置均可识别。启用前先备份，验证失败自动回滚。
- 旧 UAC `…1,1,1,1,1,0,1` 已具备 USB Audio，不再强写 ADB 位，只补 IMS / VoLTE，避免模块返回 `OK` 但配置保持原样时误报失败。
- USB 模式切换、模块重启和重新枚举期间的临时 `USBCFG ERROR` 会自动重试；重新连接后会读取实际配置，不会沿用旧“已就绪”状态。

### 语音运行时

- 首次在「设置 → 语音运行时」确认后，App 从固定上游获取指定版本并校验 SHA-256，缓存到本机；后续模块重启或重插可复用缓存。
- 下载包含 Raw → GitHub Contents API → Raw 重试链路，并使用独立等待窗口，避免上游短暂失败或通用接口超时造成初始化中断。
- 模块侧语音运行时不包含在源码、DMG、ZIP 或 Release 中。

### iPhone / iPad 模式与发布包

- 「设置 → 连接模式」可切换 iPhone / iPad 模式：仅关闭 USB Audio，保留 USB 4G、AT 与短信，避免移动设备占用系统音频输出；接回运行 DJOneHub 的 Mac 后会恢复完整音频接口。
- 修复安装包误带入旧通知 App、设置页遗留固定版本号及连接模式入口缺失的问题。
- v1.2.9 同步提供 macOS Universal（Apple Silicon + Intel）和 Windows x64（含 `DJOneHub.exe`）安装包；Windows 仍未完成真实模块验证。

| 功能 | 说明 |
| --- | --- |
| 电话 | 拨号、接听、拒接、挂断、DTMF、通话记录和录音入口。 |
| 短信 | 收发短信、验证码预览；读取后可自动清理模块存储。 |
| 通讯录 | 可同步本机通讯录，用姓名或号码拨号、发短信。 |
| 网络与 GPS | USB 4G、Wi-Fi 优先、4G 兜底、GPS/GNSS 状态与菜单栏提示。 |
| iPhone / iPad 模式 | 关闭 USB Audio、保留上网和短信；下次接回 Mac 自动恢复完整模式。 |
| 模块工具 | eUICC Profile、AT 调试、网络诊断和初始化状态。 |

## 版本演进

DJOneHub 从本机网页工具逐步演进为独立 macOS App。早期能力没有从仓库历史中删除，相关标签、安装包和完整说明仍可访问。

| 版本 | 主要变化 | 历史入口 |
| --- | --- | --- |
| 初始预览 | 模块状态、短信、eSIM、USB 4G、AT 调试和本机网页管理。 | [最初提交](https://github.com/rogerbush007-a11y/DJOneHub-mac-enhanced/commit/1b8a33ff2ae115b4bb14919e695274cbed6e1f3f) |
| v0.1.1-preview | 增加一键 DMG 安装包，保留菜单栏实时网速。 | [Release](https://github.com/rogerbush007-a11y/DJOneHub-mac-enhanced/releases/tag/v0.1.1-preview) |
| v0.1.2-preview | 移除菜单栏网速显示，保留 GPS 与 4G 状态。 | [Release](https://github.com/rogerbush007-a11y/DJOneHub-mac-enhanced/releases/tag/v0.1.2-preview) |
| v0.1.3-preview | 提供 Apple Silicon 与 Intel 通用安装包。 | [Release](https://github.com/rogerbush007-a11y/DJOneHub-mac-enhanced/releases/tag/v0.1.3-preview) |
| v0.1.4-preview | 增加 Windows x86-64 实验版。 | [Release](https://github.com/rogerbush007-a11y/DJOneHub-mac-enhanced/releases/tag/v0.1.4-preview) |
| v0.1.5-preview | 模块重连后自动续租 USB 4G DHCP。 | [Release](https://github.com/rogerbush007-a11y/DJOneHub-mac-enhanced/releases/tag/v0.1.5-preview) |
| v0.1.6-preview | 增加信号自检、自动找回和 USB 打开超时保护。 | [Release](https://github.com/rogerbush007-a11y/DJOneHub-mac-enhanced/releases/tag/v0.1.6-preview) |
| v0.1.7-preview | 在新 Mac 上自动创建、启用模块网卡并续租 DHCP。 | [Release](https://github.com/rogerbush007-a11y/DJOneHub-mac-enhanced/releases/tag/v0.1.7-preview) |
| v1.2.4 | 重构为独立 App，整合拨号、通话、短信、通讯录、设置和系统提醒。 | [Release](https://github.com/rogerbush007-a11y/DJOneHub-mac-enhanced/releases/tag/v1.2.4) · [发布说明](docs/RELEASE_NOTES_v1.2.4.md) |
| v1.2.5 — v1.2.8 | 增加语音运行时确认下载、移动设备模式，并持续修复首次启用与下载恢复。 | [Releases](https://github.com/rogerbush007-a11y/DJOneHub-mac-enhanced/releases) |
| v1.2.9 | 修复 USB 配置识别、安装包混入旧通知 App 和连接模式入口问题。 | [Release](https://github.com/rogerbush007-a11y/DJOneHub-mac-enhanced/releases/tag/v1.2.9) |
| v1.2.10 | 增加不依赖手机 App 的模块侧 USB/蜂窝网络唤醒。 | [Release](https://github.com/rogerbush007-a11y/DJOneHub-mac-enhanced/releases/tag/v1.2.10) |

v0.1.7-preview 时期的 487 行完整主页已原样保存在 [`docs/history/README-v0.1.7-preview.md`](docs/history/README-v0.1.7-preview.md)，可用于核对早期安装方式、界面和设计边界。

## 接入原理与架构演进

大疆第一代 4G 模块会通过不同 USB 组合向主机暴露管理串口、USB 网卡和音频接口。DJOneHub 围绕模块已有接口工作，不刷写或替换模块固件。

早期 v0.1.x 主要通过本机网页管理模块：短信模式负责状态、短信、eSIM 和 AT，上网模式负责 USB 4G。切换模式时模块重新枚举，网页会短暂离线。

从 v1.2.4 起，核心能力逐步收拢到独立 macOS App：

1. Go 后台负责 USB/AT、短信、网络、GPS、eSIM 和通话状态。
2. 原生 macOS App 负责电话、短信、通讯录、设置和系统通知。
3. Mac 双向通话使用模块 USB Audio，并按用户确认加载经过 SHA-256 校验的模块侧语音运行时。
4. v1.2.6 增加 iPhone / iPad USB 组合；模块接回 Mac 后恢复完整音频接口。
5. 每次高风险 USB 配置调整前保存实际配置，验证失败时恢复，避免把短暂重枚举误判为永久故障。

本机 HTTP 服务只监听 `127.0.0.1:7575`。SIM、短信、联系人、录音和卡片资料不应离开用户设备。

## 界面预览

> 以下均为真实界面截图；号码、联系人、头像、验证码和时间已遮蔽。

### 电话

<p align="center">
  <img src="docs/images/v1.2.4/dial-pad-empty.png" alt="拨号界面" width="31%" />
  <img src="docs/images/v1.2.4/call-dialing.png" alt="正在拨号" width="31%" />
  <img src="docs/images/v1.2.4/call-active.png" alt="通话中" width="31%" />
</p>

拨号、接听、拒接、挂断、DTMF、通话记录与录音入口统一在电话页。

<p align="center">
  <img src="docs/images/v1.2.4/call-history.png" alt="通话记录" width="48%" />
  <img src="docs/images/v1.2.4/incoming-call-notification.png" alt="来电通知" width="38%" />
</p>

### 短信与通讯录

<p align="center">
  <img src="docs/images/v1.2.4/sms-compose.png" alt="短信编辑" width="45%" />
  <img src="docs/images/v1.2.4/contacts.png" alt="通讯录" width="45%" />
</p>

短信支持收发、验证码预览和自动清理；通讯录可同步本机联系人并用于检索。

<p align="center">
  <img src="docs/images/v1.2.4/sms-notification.png" alt="短信通知" width="44%" />
</p>

### 设置与版本

<p align="center">
  <img src="docs/images/v1.2.4/about.png" alt="关于页" width="42%" />
</p>

## 下载与平台状态

| 平台 | 包 | 当前状态 |
| --- | --- | --- |
| macOS 13+ | `DJOneHub-macOS-universal-v1.2.10.dmg` | Apple Silicon 构建验证；模块网络唤醒仍需 iPhone/iPad 实机长时间锁屏验收。 |
| Windows x86-64 | `DJOneHub-Windows-amd64-v1.2.9.zip` | 内含 `DJOneHub.exe`；尚未在真实 Windows + 模块上验证。 |

Windows 目前不承诺模块功能可用；它不提供 macOS 专用的 USB AT/eSIM、USB 4G 自动策略、原生通知、MapKit 或双向通话音频。

## macOS 安装

1. 打开 DMG，运行“安装 DJOneHub.command”。
2. 日常直接打开 DJOneHub App；也可在安装目录运行 `djonehub start`。

本地服务只监听 `127.0.0.1:7575`。首次通话时，App 会请求麦克风权限。

### 接入准备

#### 硬件

- 大疆第一代 4G 模块（USB 设备标识通常为 `2ca3:4006`）
- 可正常使用的实体 SIM，或与当前实现兼容的实体 eUICC/eSIM 卡片
- 支持数据传输的 USB-C 线缆
- Apple Silicon 或 Intel Mac

如果连接后 macOS 完全没有发现 USB 设备，应先排除仅支持充电的线缆、供电不足和转接器兼容问题。

#### 系统

- macOS 13 Ventura 或更新版本
- Apple Silicon 已实机验证
- Intel 包含在 Universal 构建中，但仍应结合真实设备验证模块连接与通话

发行包已包含运行所需的 libusb。普通用户不需要安装 Homebrew、Go、Node.js 或其他开发环境。

#### 模块指示灯

| 状态 | 常见含义 |
| --- | --- |
| 红色常亮 | 未插入 SIM 卡 |
| 红色闪烁 | SIM 卡未被正常识别 |
| 绿色常亮 | SIM 已识别，蜂窝信号通常较好 |
| 绿色闪烁 | SIM 已识别，信号较弱或仍在注册 |

不同固件的灯光行为可能不同，最终以 App 中的 SIM、信号和网络注册状态为准。

### 首次启动

1. 将 SIM 或 eUICC 卡片插入模块。
2. 使用支持数据传输的 USB 线连接模块与 Mac。
3. 等待 macOS 完成 USB 枚举。
4. 打开 DJOneHub App。
5. 按设置页提示完成首次模块检查；启用语音运行时时应阅读来源和校验信息后再确认。

后台服务默认只监听：

```text
http://127.0.0.1:7575
```

如需打开兼容管理网页，可执行：

```sh
djonehub open
```

### macOS 阻止打开时

若 macOS 提示无法验证开发者，请打开“系统设置 → 隐私与安全性”，在对应安全提示处选择“仍要打开”。

只有在安装包来自本项目 Release 且已核对 SHA-256 时，才可考虑移除隔离属性。不要对来源不明的 App、脚本或模块运行时执行该操作。

### 历史 ZIP 安装方式

v0.1.x 预览版曾提供 ZIP/命令行安装方式：完整解压后执行 `./install`，默认安装到 `/usr/local/libexec/djonehub` 并创建 `/usr/local/bin/djonehub`。该说明仅用于维护旧部署；新安装优先使用当前 DMG 内的“安装 DJOneHub.command”。

旧发行目录也支持免安装运行：保留完整目录并执行 `./djonehub start`。不要只复制单个可执行文件，因为运行时还依赖包内的 `bin`、`lib` 和资源目录。

## 完整使用说明

### 电话与通话

- 电话页提供拨号、接听、拒接、挂断、DTMF、静音、通话记录和录音入口。
- 来电与通话状态由 App 和本机后台共同维护，不要求网页常驻。
- 双向通话依赖模块侧语音运行时、USB Audio、运营商 IMS/VoLTE 和当前 SIM 状态；控制命令成功不等于音频链路一定就绪。
- 首次通话前请授予麦克风权限。出现单向无声时，应先检查设置页的通话支持、语音运行时和模块实际 USB 配置。

### 短信

- 支持接收、发送、自动轮询、验证码预览和模块旧短信清理。
- 发送国际短信时应填写完整国际号码，例如 `+86138XXXXXXXX`。
- 短信能否收发取决于套餐、漫游、网络注册、短信中心和模块固件兼容性。
- 发布截图或日志前必须隐藏手机号、短信正文和验证码。

### 通讯录

App 可读取本机通讯录，用姓名或号码拨号、回拨和发送短信。通讯录数据保存在本机，不应随日志、Issue 或测试包上传。

### eSIM 与卡片管理

这里管理的是插入模块实体 SIM 卡槽的兼容 eUICC/eSIM 卡片，不是 Mac 内置 eSIM。主要能力包括：

- 读取 EID、固件、空间和已安装 Profile
- 下载、启用、改名和删除 Profile
- 检测卡片通讯录兼容性
- 将号码资料按 ICCID 关联保存

> [!WARNING]
> 下载、启用、改名和删除 Profile 都会写入实体卡片。操作过程中不要拔出模块或切换 USB 模式；删除通常不可撤销。

不同 eUICC 产品即使遵循同一规范，也可能在证书链、SM-DP+、扩展 APDU 和运营商策略上存在差异。

### USB 4G 与网络策略

- DJOneHub 可识别模块 USB 网卡，并维持 Wi-Fi 优先、4G 兜底的本机网络策略。
- 模块网卡通常获得 `192.168.225.x` 地址；实际接口名称和网络服务名称可能因系统与连接历史不同。
- 页面流量只用于观察当前会话，不等同于运营商账单。
- 代理软件可能按网络服务分别生效。切换到模块网卡后代理失效时，应检查系统代理、TUN/增强模式和 VPN 路由。
- 手动关闭 4G 时，短信、AT 和来电控制是否保留取决于当前 USB 组合与模块 Agent 状态。

### iPhone / iPad 模式

该模式用于把模块交给移动设备使用：关闭 USB Audio，保留 USB 网络、AT 与短信相关接口。模式切换会导致 USB 重新枚举，应等待完成后再拔插。

模块接回运行 DJOneHub 的 Mac 后，应用会尝试恢复完整 Mac 配置。恢复过程中短暂离线属于正常现象；若持续失败，应查看设置页显示的实际 USB 配置和后台日志，不要连续反复切换。

### GPS / GNSS

GPS 默认关闭。启用后定位结果只在本机读取和展示，停止后应同步清理菜单栏状态。室内、弱信号和首次冷启动可能需要更长时间。

### AT 调试

AT 调试可发送基础只读命令，例如：

```text
AT
AT+CSQ
AT+COPS?
AT+CPIN?
AT+CNUM
```

AT 指令可能改变网络注册、PDP、USB 模式、短信存储和 SIM 状态。不了解作用的写入命令不要执行，也不要照搬来源不明的刷机指令。

## 常用命令

```text
djonehub start          启动并自动打开管理网页
djonehub start --demo   启动无硬件演示模式
djonehub stop           停止正在运行的程序
djonehub status         查看运行状态
djonehub logs           查看实时日志（Control+C 退出）
djonehub open           打开管理网页
```

DMG 安装版通常由 LaunchAgent 自动维护后台，日常优先使用 App。上述命令主要用于兼容安装、诊断和开发。

建议停止 DJOneHub 后再拔出模块。如果直接拔出，后台会等待设备重新连接。

## 日志与本地数据

日志目录：

```text
~/Library/Logs/DJOneHub
```

运行状态、缓存和本地数据目录：

```text
~/Library/Application Support/DJOneHub
```

常见日志包括 `launchd.log`、`notifier.log` 和兼容命令行部署生成的 `djonehub.log`。日志可能包含号码、短信摘要、ICCID、EID、网络地址或模块状态，分享前必须脱敏。

### 本机安全与短信保存

- 后台首次启动会在 `~/Library/Application Support/DJOneHub/api-token` 创建仅当前用户可读的随机令牌。原生 App 会自动读取；请勿上传、截图或分享该文件。
- 本机 API 会拒绝外部网页来源、异常 Host、缺少令牌的请求，以及使用非 JSON 格式调用修改类接口的请求。
- 兼容网页由后台设置同站点、仅 HTTP 可读的会话 Cookie，无需手工填写令牌。
- AT 调试仅保留信号、运营商、SIM、GPS 和 USB 状态等只读命令。拨号、重启、USB 配置等写入操作必须使用 App 中对应的专用功能。
- 收到的短信会以 `0600` 权限保存到 `~/Library/Application Support/DJOneHub/sms-cache.json`。只有确认本地保存成功后，才会执行已启用的模块短信自动清理。
- 电话号码和 eSIM Matching ID 不再完整写入普通运行日志。日志仍可能包含设备与网络诊断信息，分享前仍应检查和脱敏。

## 卸载

当前 DMG 提供“卸载 DJOneHub.command”，应优先使用该脚本，以同时停止 LaunchAgent 并清理应用运行目录。

卸载脚本会先尝试通过已认证的本机后台停止并移除模块侧网络唤醒服务。执行卸载时应保持模块连接并处于可用的 Mac/ADB 模式。如果模块没有连接，脚本会明确提示模块侧仍待清理；此时应重新安装同版本、连接模块并再次执行卸载。

旧 ZIP/命令行安装可先执行：

```sh
djonehub stop
sudo rm -f /usr/local/bin/djonehub
sudo rm -rf /usr/local/libexec/djonehub
```

如需一并删除本地日志和运行数据：

```sh
rm -rf "$HOME/Library/Logs/DJOneHub"
rm -rf "$HOME/Library/Application Support/DJOneHub"
```

删除本地数据不可恢复；如需保留录音、日志或配置，应先手动备份。

## 常见问题

### 模块连接后没有反应

先确认线缆支持数据传输，再检查系统 USB 信息、模块供电和 App 设置页。若系统能看到 `2ca3:4006` 但 AT 超时，应记录实际 USB 配置后再处理，不要仅凭“检测到 USB”判断模块已可用。

### 切换模式后设备短暂消失

USB 模式切换会触发重新枚举，短暂断开通常不是故障。等待系统重新识别；长时间未恢复时停止 App、重新插拔模块并查看日志。

### 换卡或切换 Profile 后仍显示旧信息

模块重新读取卡片和注册网络需要时间。刷新后仍未更新时，可在确认没有写卡任务后重启模块或重新插拔。

### SIM 在手机中可用，但模块不能通话或收发短信

手机与模块可能使用不同的运营商配置、MBN、IMS、VoLTE、漫游和短信中心。手机可用不代表模块固件一定兼容。

### USB 4G 可连接但无法上网

依次检查 SIM 数据能力、模块 PDP、USB 网卡地址、默认路由、DNS、VPN/TUN 和代理配置。取得 `192.168.225.x` 地址只说明本地 USB 网络建立，不代表蜂窝出口一定可用。

### 代理或 VPN 开启后模块控制超时

确认访问 `192.168.225.1` 的流量仍走模块 USB 网卡，而不是被 VPN 的 `utun` 路由接管。必要时暂时关闭 VPN 做对照，再检查按接口绑定和路由策略。

### 能否用于其他型号的 4G 模块

不能保证。USB 识别、端点、AT 接口、语音运行时和模式切换均围绕大疆第一代 4G 模块实现。

## 当前限制

- Windows 版本尚未完成真实 Windows + 模块全功能验证。
- Intel Universal 包已构建，但仍需更多真实 Intel 设备验证。
- 双向通话依赖外部模块语音运行时与具体运营商环境。
- eSIM 兼容性取决于卡片、证书链、SM-DP+ 和运营商策略。
- 不同模块批次、固件、SIM 和 macOS 版本的行为可能不同。
- 流量统计仅供参考，不等同于运营商账单。

## 通话与开源边界

源码包含 macOS App、Go 后端、Windows 控制台、MaVo MIT 音频适配代码和构建脚本。

Mac 双向通话仍需要模块侧语音运行时。该运行时**不随本仓库、Release、DMG 或 Windows ZIP 提供，也不会由 DJOneHub 镜像**。用户一次明确确认后，App 才会从固定上游来源获取指定版本，逐项校验 SHA-256 后保存到本机。上游文件、模块型号、固件、SIM 和运营商条件均可能影响双向语音可用性。

请不要把未知来源的二进制提交到 Issue、PR 或衍生 Release。完整边界见 [OPEN_SOURCE_SCOPE.md](OPEN_SOURCE_SCOPE.md)。

## 从源码构建

```sh
# macOS Universal
scripts/package-macos-universal.sh v1.2.10
scripts/build-dmg-universal.sh v1.2.10

# Windows x86-64
scripts/package-windows-amd64.sh v1.2.10
```

构建 macOS 包需要完整 Xcode、Go、`pkg-config` 与网络下载官方 libusb 源码。Windows 包在 Mac 上只能交叉编译，不能替代 Windows 真机验证。

## 使用提醒

- 使用蜂窝数据、短信、通话与 eSIM 前，请确认运营商协议、资费及当地法律要求。
- GPS 默认关闭；定位信息仅在本机读取和展示。
- 本项目不会上传 SIM、短信、联系人、录音或卡片资料。
- 与 DJI、Quectel、运营商及 eSIM 厂商不存在隶属或授权关系。

## 项目来源、许可证与致谢

DJOneHub 是在研究大疆第一代 4G 模块和原 VoHive 项目的基础上继续开发的非官方工具。仓库包含基于原 VoHive 演进的代码，以及为 macOS USB 通信、设备热插拔、本机管理、短信、eSIM、网络诊断、原生通知和发行打包新增或修改的实现。

本项目不代表 DJI、Quectel、任何运营商或 eSIM 卡片厂商。相关商标和产品名称归各自权利人所有。

仓库继续遵循 [PolyForm Noncommercial License 1.0.0](LICENSE)，仅允许许可证定义的非商业用途。必须保留的上游声明：

```text
Required Notice: Copyright iniwex5 (https://github.com/iniwex5/vohive)
```

随发行包提供的 libusb 1.0.30 使用 GNU Lesser General Public License v2.1 or later；其他组件遵循各自许可证。完整来源、第三方声明和公开边界见：

- [LICENSE](LICENSE)
- [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)
- [OPEN_SOURCE_SCOPE.md](OPEN_SOURCE_SCOPE.md)
- 各 `third_party` 目录中的许可证与声明

感谢原 VoHive 项目及作者 iniwex5、libusb 与其他开源组件贡献者，以及参与大疆第一代 4G 模块研究、测试和资料分享的用户。

如果 DJOneHub 对你有帮助，欢迎通过 Issue 分享兼容性结果、问题日志或改进建议。提交截图和日志前，请隐藏手机号、EID、ICCID、IMSI、短信验证码和其他隐私信息。
