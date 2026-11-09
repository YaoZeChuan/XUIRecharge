# 3x-ui 客户端重置服务

轻量 Go 服务，为 3x-ui 主面板提供客户端查询和流量/期限重置。重置会将总流量额度设为所选套餐、清零已用流量，并从付款完成时按套餐期限重置有效期。启用 LDC 配置后，用户先创建支付订单，付款确认后才执行重置。

管理后台：`/admin`  
用户入口：`/`  
健康检查：`/api/healthz`

## 当前功能

- 用户通过订阅 ID 查询账号或切换账号，并选择 100 GB（100 LDC）、200 GB（200 LDC）或 300 GB（300 LDC）的一个月重置方案。
- 重置设置总额度、清零已用流量，并按所选套餐期限重置有效期。
- 每次重置写入可核查的订单；结果不确定时保留待核对状态，服务重启后不会自动重发面板请求。
- 面板明确拒绝写入且核验客户端额度、期限和启用状态均未变化时，订单记为失败并允许再次提交；待核对订单可由管理员实时核验后解除，记录保留以便追溯。
- 管理员可查看重置统计、重置记录和面板客户端，也可检查面板连接、修改密码并下载数据库备份。
- LDC 支付成功后才执行面板重置；支付回调与后台定时查单共用同一幂等状态转换。
- 付款已确认但面板写入失败或结果无法核实时，订单会进入待核对，不会自动重放面板写入。管理员可在后台核验并手动确认已完成，也可随时向 LDC 发起全额退款；退款请求结果不确定时，可先在 LDC 核对后手动确认，并保留审计记录。
- SQLite 使用 WAL、外键、忙等待和单连接池；页面通过 Go `embed` 内嵌，生产环境不需要安装 Node.js。

## 本地运行

需要 Go 1.26 或更新版本。

```powershell
Copy-Item .env.example .env
```

设置不同的随机 `SESSION_SECRET` 和至少 12 个字符的 `ADMIN_INITIAL_PASSWORD`。本地示例默认使用 `PANEL_MODE=mock`，不连接真实面板。Mock 订阅 ID 为 `user001-sub`、`expired-demo-sub` 和 `unlimited-demo-sub`。

PowerShell 生成会话密钥：

```powershell
[Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
```

启动服务：

```powershell
go run ./cmd/server
```

默认监听 `http://127.0.0.1:8080`。首次登录名由 `ADMIN_USERNAME` 设置，默认 `admin`。首次启动创建管理员后，可从 `.env` 移除 `ADMIN_INITIAL_PASSWORD`。

## 连接 3x-ui 主面板

设置 `PANEL_MODE=real`、面板基础地址和 API 令牌：

```text
PANEL_MODE=real
PANEL_BASE_URL=https://面板域名:端口/自定义面板路径
PANEL_API_TOKEN_ENV=PANEL_API_TOKEN
PANEL_API_TOKEN=你的面板API令牌
PANEL_COOKIE=可选的3x-ui会话Cookie
PANEL_TLS_VERIFY=true
```

`PANEL_BASE_URL` 填到自定义面板路径为止，不要附加 `/panel/api/openapi.json`。如果 API 还要求 3x-ui 登录会话，在 `PANEL_COOKIE` 中设置完整 Cookie 请求头值。令牌和 Cookie 只从环境变量读取，不会写入源码、数据库或日志。

服务在连接检查和每次写入前读取当前面板 OpenAPI，核验设置额度、期限和流量重置接口。写入后会复查主面板客户端状态；如果结果无法确认，订单进入待核对状态，不会自动重发。面板连接成功不代表远程节点已同步，节点同步状态会显示为“未知”。

不要把真实 API 令牌写进 `.env.example`、Git 或工单。曾经公开的令牌应在面板中轮换。

## LDC 积分支付

使用 `LDC.txt` 中的官方原生接口（`type=ldcpay`），不使用易支付兼容下单接口。请在 LDC 控制台创建官方应用，登记与商户 Ed25519 私钥配对的公钥，并通过环境变量配置 `LDC_CLIENT_ID`、`LDC_CLIENT_SECRET`、`LDC_ED25519_PRIVATE_KEY` 和公网可访问的 HTTPS `PUBLIC_BASE_URL`。私钥可使用 PKCS#8 PEM，或 Base64 编码的 32 字节 seed / 64 字节 Ed25519 私钥；凭据不写入 YAML、数据库或日志。`LDC_BASE_URL` 默认是 `https://credit.linux.do`，可设置为该域名或 `/epay` 网关地址；主动查单间隔默认 30 秒，可通过 `LDC_POLL_INTERVAL_SECONDS` 调整。

用户点击套餐后，服务先校验面板能力并保存待付款订单，再按官方 Ed25519 规则签名请求 `/epay/pay/submit.php`；浏览器随后跳转到 LDC 认证页面。订单级通知地址为 `/api/public/ldc/notify`，回跳地址为 `/?o=订单号`。回跳只用于显示状态，不作为支付凭据。官方文档没有明确给出通知验签规则，因此通知只作为查单触发信号：服务收到通知后立即调用共用 `/epay/api.php` 查单，并核对业务订单号、金额和支付状态；后台也会定时主动查单。只有查单确认收款后才会排队执行重置，通过数据库条件更新保证只处理一次。

查单和退款使用官方文档说明的共用 `/epay/api.php` 服务端接口。查单或通知确认付款后，服务再次读取面板客户端状态，再设置套餐额度、清零流量。管理员可在重置记录中手动确认 LDC 收款（需提供交易号和核对说明）、核验面板后确认重置完成，或对已付款订单发起全额退款。退款后若面板仍处于操作前状态，客户可重新下单；状态不一致时订单继续待核对。退款接口超时或响应不确定时，管理员可先在 LDC 核验再确认退款。所有手动确认和退款动作都会写入审计日志。请在 LDC 应用配置中保留服务的公网 HTTPS 地址，并确认反向代理可转发通知路径。`.env.example` 列出了环境变量格式。

## Docker 部署

1. 复制 `.env.example` 为 `.env`，配置真实面板地址、令牌、随机会话密钥和管理员密码。
2. 生产环境设置 `PANEL_MODE=real`，不要使用 Mock。
3. 构建并启动：

```bash
docker compose up -d --build
docker compose logs -f recharge
```

服务只绑定到宿主机 `127.0.0.1:8080`，可用 Caddy 终止 HTTPS。Docker 数据保存在命名卷 `recharge-data`，容器以非 root 用户运行并使用只读根文件系统。

## 备份与恢复

登录管理员后台，进入系统设置下载 SQLite 在线备份；服务也会在数据库目录的 `backups/` 子目录保存备份。

恢复时先停止服务，备份当前数据库文件，再替换为备份文件。应确认服务已停止，避免 WAL 文件仍在写入。

## API 摘要

用户端：

```text
POST /api/public/clients/lookup
GET  /api/public/client-reset/plans
POST /api/public/client-reset
POST /api/public/client-reset/history
POST /api/public/client-reset/orders/{orderNo}
GET  /api/public/ldc/notify
```

用户端查询账号、创建订单和读取订单状态均使用 `subscriptionId`；订阅 ID 通过 POST 请求正文发送，不再接受客户端邮箱、客户端 ID 或订阅链接作为登录标识。

管理员接口使用安全会话 Cookie、CSRF 校验和登录失败限制，主要路径包括：

```text
GET  /api/admin/dashboard
GET  /api/admin/client-resets
POST /api/admin/client-resets/{orderNo}/reconcile
POST /api/admin/client-resets/{orderNo}/confirm-payment
POST /api/admin/client-resets/{orderNo}/confirm-success
POST /api/admin/client-resets/{orderNo}/refund
POST /api/admin/client-resets/{orderNo}/confirm-refund
GET  /api/admin/clients
GET  /api/admin/panel/status
GET  /api/admin/settings
POST /api/admin/backup
```

## 数据兼容

旧版卡密/充值 API 和管理界面入口已移除。为兼容已有 SQLite 数据库，旧充值表暂时保留；当前版本不再通过界面或 HTTP API 管理、生成或兑换卡密。

## 已知边界

- LDC 支付配置缺失时，用户入口不会执行不扣积分的重置。
- 待核对订单不会被直接删除。只有确认退款并核验面板状态与操作前一致后，订单才会解除并允许客户重试。
- 无限期账号和面板特殊续期状态暂不支持重置。
- SQLite 适用于单实例部署，不支持多个服务副本共享写入。
