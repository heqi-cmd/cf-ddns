<div align="center">

<img src="assets/logo.svg" width="112" alt="cf-ddns">

# cf-ddns

**按运营商线路优选 Cloudflare 节点的智能 DNS 更新器**

移动、联通、电信用户访问同一个域名，各自解析到自己线路最快的 Cloudflare 节点

[![CI](https://github.com/heqi-cmd/cf-ddns/actions/workflows/ci.yml/badge.svg)](https://github.com/heqi-cmd/cf-ddns/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/heqi-cmd/cf-ddns)](https://github.com/heqi-cmd/cf-ddns/releases/latest)
![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)
![License](https://img.shields.io/badge/License-MIT-blue)
![Platform](https://img.shields.io/badge/Platform-Linux%20|%20Windows%20|%20macOS-lightgrey)
![Dependencies](https://img.shields.io/badge/Dependencies-0-success)

</div>

---

## 简介

Cloudflare 的 anycast IP 在国内不同运营商下的实际质量差异很大：对移动最快的 IP，对电信可能很慢。常见做法是定期测速换 IP，但一条 DNS 记录只能有一个值，无论怎么优选都只能照顾一部分用户。

cf-ddns 把「优选」和「智能解析」结合起来：为每条运营商线路**分别**优选节点，再借助 DNSPod 的线路解析能力写入同一个域名的不同线路。用户无需做任何事，访问同一个域名即可落到各自网络的最优节点。

```
                              ┌─ 移动用户 ──► 移动线路记录 ──► 移动优选 IP
cf.example.com ──► DNSPod ────┼─ 联通用户 ──► 联通线路记录 ──► 联通优选 IP
    同一个域名                 ├─ 电信用户 ──► 电信线路记录 ──► 电信优选 IP
                              └─ 其他用户 ──► 默认线路记录 ──► 通用优选 IP
```

## 特性

- **分线路优选** — 默认 / 移动 / 联通 / 电信各自独立测速选优，互不影响
- **双栈支持** — 同一线路可并存 A（IPv4）与 AAAA（IPv6）记录，各自独立优选
- **智能类型识别** — 候选可以是 IPv4、IPv6 或域名，自动写成 A / AAAA / CNAME
- **零依赖** — 纯 Go 标准库实现，含腾讯云 TC3 签名，单文件二进制，无需 CGO
- **容错设计** — 单条线路失败不影响其它线路；无 IPv6 网络时自动跳过 AAAA
- **幂等更新** — 结果与现有记录一致时不发起写请求，避免无谓的 API 调用
- **向后兼容** — 保留原有 Cloudflare 单记录模式，旧配置文件无需修改

## 工作原理

1. **拉取候选** — 对每条线路，从 [BestCF](https://github.com/DustinWin/BestCF) 获取该运营商对应的优选列表（上游每 12 小时汇总全国多个测速源更新）
2. **解析识别** — 逐行判断类型：IPv4 → `A`，IPv6 → `AAAA`（需开启 `ipv6`），域名 → `CNAME`
3. **并发探测** — 对候选发起 TCP 建连（80 端口）+ HTTP 请求，确认存活并从 `CF-RAY` 响应头提取数据中心代码（如 `HKG`）。同一轮内多条线路共享测速缓存，重复地址只测一次
4. **分槽位选优** — IPv4/域名候选竞争主槽位（一条 A 或 CNAME），IPv6 候选竞争 v6 槽位（一条 AAAA），按 `pick` 策略择优
5. **收敛记录** — 与线上现有记录比对，不同则创建/更新，相同则跳过
6. **循环** — 所有线路处理完毕后休眠 `interval`，重复上述流程

### 选优策略

| `pick` | 行为 | 适用场景 |
|---|---|---|
| `order`（默认） | 本机测速仅用于存活过滤，在存活候选中取 BestCF 列表排名最靠前的 | **跨运营商线路**。程序跑在电信机器上时，它实测的「移动线路延迟」没有参考价值，应信任 BestCF 的全国多点测速排名 |
| `latency` | 取本机实测建连延迟最低的 | 程序部署在与该线路**相同**运营商的网络中，例如电信家宽服务器可给「电信」线路配 `latency` |

### 记录类型规则

A 与 AAAA 可在同一线路共存，CNAME 则与两者互斥。因此当某线路的最优结果是域名（CNAME）时，工具不再维护该线路的 AAAA —— CNAME 目标域名本身已自带双栈解析。若线路上残留旧 AAAA 记录会与 CNAME 冲突，工具会跳过写入并提示手动清理，不做破坏性删除。

## 快速开始

### 1. 准备域名

Cloudflare DNS 没有「运营商线路」维度，无法按访问者运营商返回不同记录，因此承载优选记录的域名需托管到 [DNSPod](https://www.dnspod.cn/)（免费套餐即支持移动/联通/电信线路）。

> **这个域名不需要留在 Cloudflare。** 优选记录只是把一个名字指向 Cloudflare 边缘 IP，真正被访问的 CF 站点（Workers / Pages / 自建反代等）使用它自己的域名和 SNI，两者互不影响。

按主域名是否需要保留在 Cloudflare，二选一：

<table>
<tr><th>方案</th><th>做法</th><th>适用</th></tr>
<tr>
<td><b>整域托管</b></td>
<td>在 <a href="https://console.dnspod.cn/">DNSPod 控制台</a>添加域名，到注册商处把 NS 改为 DNSPod 分配的服务器（形如 <code>f1g1ns1.dnspod.net</code>）</td>
<td>域名可以整体迁走</td>
</tr>
<tr>
<td><b>子域名委派</b></td>
<td>把子域名（如 <code>opt.example.com</code>）作为独立域名添加到 DNSPod，再到 Cloudflare 为该子域添加 NS 记录指向 DNSPod</td>
<td>主域名需保留 Cloudflare 的 CDN / 防护</td>
</tr>
</table>

用以下命令确认委派生效（应返回 DNSPod 的服务器）：

```bash
nslookup -type=NS opt.example.com 1.1.1.1
```

### 2. 获取 API 密钥

DNSPod 已并入腾讯云，API 使用腾讯云密钥：

1. 打开[访问管理 → API 密钥管理](https://console.cloud.tencent.com/cam/capi)，新建密钥并立即保存
2. **建议使用子账号**：访问管理 → 用户 → 新建子用户，仅勾选编程访问，策略只授予 `QcloudDNSPodFullAccess`

### 3. 配置

```bash
cp config.example.json config.json
```

配置文件须为 **UTF-8 编码**（线路名是中文）。`config.json` 已在 `.gitignore` 中排除，不会被提交。

```json
{
  "provider": "dnspod",
  "secret_id": "你的腾讯云SecretId",
  "secret_key": "你的腾讯云SecretKey",
  "domain": "example.com",
  "record": "cf.example.com",
  "ttl": 600,
  "interval": "10m",
  "ipv6": true,
  "lines": [
    { "line": "默认", "ips_url": ".../bestcf-ip.txt" },
    { "line": "移动", "ips_url": ".../cmcc-ip.txt" },
    { "line": "联通", "ips_url": ".../cucc-ip.txt" },
    { "line": "电信", "ips_url": ".../ctcc-ip.txt" }
  ]
}
```

### 4. 构建与运行

从 [Releases](https://github.com/heqi-cmd/cf-ddns/releases/latest) 下载对应平台的压缩包解压即用，或自行编译：

```bash
go build -o cf-ddns .
```

先做一次不写 DNS 的测试：

```bash
./cf-ddns -config config.json -dry-run
```

确认各线路都能选出结果后正式运行：

```bash
./cf-ddns -config config.json
```

## 配置参考

### 通用字段

| 字段 | 类型 | 说明 |
|---|---|---|
| `provider` | string | `dnspod`（智能分线路解析）或 `cloudflare`（单记录模式）。**缺省为 `cloudflare`**，以保证旧配置行为不变 |
| `record` | string | 要维护的完整记录名，如 `cf.example.com` |
| `ttl` | int | 记录 TTL（秒）。缺省按服务商取值：dnspod `600`（免费套餐下限）、cloudflare `60` |
| `interval` | string | 扫描 + 更新间隔，Go duration 格式，如 `"10m"` |
| `task` | int | 并发测速协程数，默认 `50` |
| `colo` | string | 全局数据中心过滤，逗号分隔，如 `"HKG,SJC"`；留空不过滤 |
| `ipv6` | bool | 是否同时优选 IPv6 并维护 AAAA 记录，默认 `false`。需本机具备 IPv6 连通性——否则 IPv6 候选会全部探测失败，工具自动跳过，不影响 IPv4 |
| `dry_run` | bool | `true` 时仅测速打印结果，不写 DNS（可不填密钥） |
| `lines` | array | 线路数组，见下 |

### `lines` 元素

| 字段 | 类型 | 说明 |
|---|---|---|
| `line` | string | 线路名，须与 DNSPod 一致：`默认` / `移动` / `联通` / `电信` |
| `ips_url` | string | 该线路的候选列表地址 |
| `pick` | string | 选优策略 `order`（默认）/ `latency` |
| `colo` | string | 本线路的数据中心过滤，缺省继承全局 `colo` |

### 服务商专属字段

| 字段 | 适用 | 说明 |
|---|---|---|
| `secret_id` / `secret_key` | dnspod | 腾讯云 API 密钥 |
| `domain` | dnspod | DNSPod 中的主域名，`record` 必须等于它或是其子域名 |
| `cf_token` / `zone_id` | cloudflare | Cloudflare API Token 与 Zone ID |
| `proxied` | cloudflare | 是否开启代理（小黄云）。直连优选节点场景应为 `false` |

### 候选列表地址

BestCF 按运营商提供不同文件，完整地址形如 `https://github.com/DustinWin/BestCF/releases/download/bestcf/<文件名>`：

| 文件 | 适用线路 |
|---|---|
| `bestcf-ip.txt` | 默认（通用） |
| `cmcc-ip.txt` | 移动 |
| `cucc-ip.txt` | 联通 |
| `ctcc-ip.txt` | 电信 |
| `bestcf-domain.txt` | 优选域名（写 CNAME） |

IP 列表内混有 IPv4 与 IPv6 条目，开启 `ipv6` 后两者都会参与优选，各自写入对应记录。

### 命令行参数

分线路解析仅支持配置文件方式。命令行参数用于 cloudflare 单记录模式，或临时覆盖配置：

```bash
./cf-ddns -cf-token "xxx" -zone-id "xxx" -record "cf.example.com"
./cf-ddns -config config.json -dry-run     # 覆盖配置中的 dry_run
./cf-ddns -config config.json -ipv6        # 覆盖配置中的 ipv6
```

完整参数见 `./cf-ddns -h`。

## 验证

程序运行后，可从 DNSPod 权威服务器按不同运营商网段查询，确认各线路返回不同结果：

- **在线工具** — [itdog.cn/dns](https://www.itdog.cn/dns/) 从各地真实节点发起查询，能直观看到三网差异
- **本机查询** — `nslookup cf.example.com` 返回的是你当前宽带运营商线路的结果

> **注意**：用 `nslookup` 指定其它运营商的公共 DNS 来验证往往不可靠——这类查询容易在路径上被拦截或改道，无法把真实的运营商信息传递到权威服务器，导致所有线路都返回默认值。请以多点测试工具的结果为准。

智能解析依据递归解析器的出口 IP（或 EDNS Client Subnet）判断运营商。若用户手动将 DNS 设为某些公共解析器，可能落到默认线路——这是智能解析的固有特性。

## 部署

程序内置循环，systemd 只需保证进程存活。

**1. 交叉编译并传输**（配置文件含密钥，务必走 `scp` 等加密通道）：

```bash
GOOS=linux GOARCH=amd64 go build -o cf-ddns .
scp cf-ddns config.json user@your-server:/tmp/
```

**2. 在服务器上就位**：

```bash
sudo cp /tmp/cf-ddns /usr/local/bin/
sudo mkdir -p /etc/cf-ddns && sudo cp /tmp/config.json /etc/cf-ddns/
sudo chmod 600 /etc/cf-ddns/config.json
```

**3. 创建 systemd unit**：

```bash
sudo tee /etc/systemd/system/cf-ddns.service > /dev/null <<'EOF'
[Unit]
Description=Cloudflare 分线路优选 DDNS
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/cf-ddns -config /etc/cf-ddns/config.json
Restart=always
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF
```

**4. 启动并检查**：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now cf-ddns
systemctl status cf-ddns      # 确认 active (running)
journalctl -u cf-ddns -f      # 实时日志
```

## 常见问题

**日志显示「无存活 IPv6 候选」**
运行程序的机器没有 IPv6 出口，IPv6 候选全部探测失败。这不影响 IPv4，工具会自动跳过 AAAA。确认服务器 IPv6 可用后重试：`curl -6 https://ip.sb`。

**提示 TTL 低于下限**
DNSPod 免费套餐的最小 TTL 是 600 秒，配置更低的值会被 API 拒绝。

**某条线路写入失败，提示需要默认线路**
DNSPod 要求记录名下先存在默认线路记录，才允许添加其它线路。工具已自动将「默认」线路排在最前处理，因此保留默认线路配置即可。

**从配置中删除了某条线路**
线上已写入的该线路记录**不会**被自动删除，需到 DNSPod 控制台手动清理。

**cloudflare 模式启动报错，提示不支持线路**
Cloudflare DNS 不支持按运营商解析。分线路需将 `provider` 改为 `dnspod` 并按上文完成域名托管。

## 兼容模式

不需要分线路时，可继续使用 Cloudflare 单记录模式（示例见 `config.cloudflare.example.json`）。旧版配置文件无需任何修改即可继续运行，选优保持原有的 `latency` 行为。

Token 与 Zone ID 获取方式：

1. 打开 [API Tokens](https://dash.cloudflare.com/profile/api-tokens) → **Create Token** → **Create Custom Token**
2. 权限选择 `Zone` → `DNS` → `Edit`，Zone Resources 仅包含目标域名
3. **Zone ID** 位于 Cloudflare 后台对应域名的 Overview 页面右下角

该模式下配置「默认」以外的线路会在启动时报错。

## 代码结构

```
cf-ddns/
├── main.go            # 主流程：参数解析、循环、按线路分槽位编排
├── config.go          # 配置定义、加载与校验
└── internal/
    ├── probe/         # 候选拉取、解析、并发测速、选优
    └── dns/           # DNS 服务商抽象及 cloudflare / dnspod 实现
```

运行测试：

```bash
go test ./...
```

推送到 `master` 与提交 PR 时，CI 会执行 gofmt 检查、`go vet`、race 测试并交叉编译多个平台；推送 `v*` 开头的 tag 会自动构建七个平台的二进制并发布到 Releases。

## 致谢

候选 IP / 域名数据来自 [DustinWin/BestCF](https://github.com/DustinWin/BestCF)，该项目每 12 小时自动汇总多个测速源（CMLiussss、VPS789、微测网、CloudFlareYes、IPDB 等）生成按运营商区分的优选列表。本项目在此基础上做本地存活筛选与分线路 DNS 自动更新，感谢 BestCF 作者及上游各测速源的持续维护。

## License

[MIT](LICENSE)
