# cf-ddns

定期测速 Cloudflare 优选 IP/域名，并自动把结果写入 Cloudflare DNS 记录（A 或 CNAME）。
典型场景：在国内服务器上常驻运行，每隔一段时间（默认 10 分钟）重新挑一个延迟最低的节点，更新到你自己的域名上。

## 工作原理

1. 从 [BestCF](https://github.com/DustinWin/BestCF) 仓库拉取一份优选地址列表（每 12 小时自动更新），每行格式为 `地址#备注`，例如：
   ```
   172.64.52.50#CF-IPv4_CFSpeedTest_1
   www.shopify.com#CF-Domain_xxx
   ```
2. 解析每一行：能解析成 IPv4 的归为 IP 候选（最终写 **A** 记录），不是 IP 但符合域名格式的归为域名候选（最终写 **CNAME** 记录），IPv6 和无法识别的条目会被忽略并打日志说明原因。
3. 并发对所有候选发起 TCP 连接（80 端口）+ HTTP 请求，用建连耗时作为延迟，用响应头里的 `CF-RAY` 提取数据中心代码（如 `HKG`），可选按数据中心过滤。
4. 取延迟最低的一个，和当前 Cloudflare 上的记录类型+内容对比，不同就创建/更新，相同就跳过。
5. 循环，按 `interval` 等待后重复。

## 构建

```bash
go build -o cf-ddns cf-ddns.go
```

## 获取 Cloudflare API Token / Zone ID

1. 打开 https://dash.cloudflare.com/profile/api-tokens → **Create Token** → **Create Custom Token**
2. 权限选 `Zone` → `DNS` → `Edit`
3. Zone Resources 选 `Include` → `Specific zone` → 你要管理的域名（不要选 All zones）
4. 创建后立刻复制保存 Token，离开页面就看不到了
5. **Zone ID** 在 Cloudflare 后台对应域名的 Overview 页面右下角直接能看到

## 配置

复制一份示例配置并填好真实值：

```bash
cp config.example.json config.json
```

字段说明：

| 字段 | 说明 |
|---|---|
| `cf_token` | Cloudflare API Token（需要该 Zone 的 DNS:Edit 权限） |
| `zone_id` | Cloudflare Zone ID |
| `record` | 要维护的域名记录，例如 `cf.yourdomain.com` |
| `proxied` | 是否开启 Cloudflare 代理（小黄云）。直连优选节点场景应为 `false` |
| `ttl` | DNS 记录 TTL（秒），`proxied=true` 时该值被 Cloudflare 忽略 |
| `interval` | 扫描+更新间隔，Go duration 格式，如 `"10m"` |
| `ips_url` | BestCF 候选列表地址，按运营商选择对应文件（见下） |
| `colo` | 只挑选指定数据中心，逗号分隔，如 `"HKG,SJC"`，留空不过滤 |
| `task` | 并发测速协程数 |
| `dry_run` | `true` 时只测速打印结果，不调用 Cloudflare API（用于本地测试） |

`config.json` 包含真实密钥，已在 `.gitignore` 中排除，**不会**被提交。

### BestCF 候选列表可选地址

| 文件 | 适用场景 |
|---|---|
| `bestcf-ip.txt` | 通用 IP 列表 |
| `cmcc-ip.txt` | 移动宽带 |
| `cucc-ip.txt` | 联通宽带 |
| `ctcc-ip.txt` | 电信宽带 |
| `bestcf-domain.txt` | 优选域名（CNAME），与 IP 列表格式相同，会被自动识别为域名 |

完整下载地址形如：
```
https://github.com/DustinWin/BestCF/releases/download/bestcf/<文件名>
```

## 运行

```bash
./cf-ddns -config config.json
```

不用 JSON 配置文件也可以纯命令行跑（两种方式二选一，指定 `-config` 后会忽略其它参数）：

```bash
./cf-ddns -cf-token "xxx" -zone-id "xxx" -record "cf.yourdomain.com"
```

本地测试（不写 DNS）：

```bash
./cf-ddns -config config.json -dry-run
```

## 常驻部署（systemd）

程序内置循环，systemd 只需保证进程存活：

```ini
[Unit]
Description=Cloudflare 优选 IP/域名 DDNS
After=network.target

[Service]
ExecStart=/usr/local/bin/cf-ddns -config /etc/cf-ddns/config.json
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
go build -o cf-ddns cf-ddns.go
sudo cp cf-ddns /usr/local/bin/
sudo mkdir -p /etc/cf-ddns && sudo cp config.json /etc/cf-ddns/
sudo systemctl enable --now cf-ddns
```
