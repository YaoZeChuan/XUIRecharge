# API 接口文档

最后更新：2026-04-20

LINUX DO Credit 提供简单、强大的 API 接口，支持多种编程语言和开发环境。通过标准化的 HTTP 接口，您可以轻松将积分服务集成到您的应用中。

## 官方 LDC 接口

官方原生接口，使用 Ed25519 签名算法，安全性更高

### 1.1 概览

- **协议：**官方 LDC 支付协议
- **服务类型：**支持`type=ldcpay`
- **网关基址：**`https://credit.linux.do/epay`
- **签名方式：**Ed25519 非对称加密

### 1.2 对接流程

1. 控制台创建应用，配置`client_id`并在应用设置中上传商户 Ed25519 公钥
2. 根据“签名算法”及商户私钥生成`sign`
3. 调用`/pay/submit`发起积分流转请求
4. 认证完成后，通过异步回调或轮询接口同步状态

### 1.3 鉴权与签名

#### 1.3.1 签名算法

1. 取除`sign`以外的所有非空请求参数
2. 将参数按参数名 ASCII 码从到大排序（字典序）
3. 使用`k1=v1&k2=v2...`格式拼接成字符串
4. 将**应用密钥 (Client Secret)**直接追加到字符串末尾
5. 使用商户私钥对最终字符串进行**Ed25519**签名
6. 将签名结果转换成 Base64 编码作为`sign`参数

```
// 示例：client_id=1&money=10.00&order_name=Test&out_trade_no=M1&type=ldcpay{SECRET}
data = "client_id=1&money=10.00&order_name=Test&out_trade_no=M1&type=ldcpay" + client_secret
signature = ed25519.Sign(privateKey, []byte(data))
signBase64 = base64.StdEncoding.EncodeToString(signature)
```

### 1.4 积分流转服务

- **方法：**POST`/epay/pay/submit.php`
- **编码：**`application/json`或`application/x-www-form-urlencoded`

| 参数 | 必填 | 说明 |
| --- | --- | --- |
| client_id | 是 | 应用客户端 ID |
| type | 是 | 固定 ldcpay |
| out_trade_no | 是 | 业务单号 |
| money | 是 | 积分数量，必须保留两位小数（比如，10.00） |
| order_name | 是 | 商品名称 |
| notify_url | 否 | 会参与签名；可选订单级异步通知地址。长度不超过 100，需为合法 URL。传入后支付成功优先回调该地址，未传则使用应用 notify_url |
| return_url | 否 | 会参与签名；可选订单级回跳地址。长度不超过 100，需为合法 URL。传入后支付成功页面优先跳转该地址，未传则使用应用 redirect_uri |
| sign | 是 | 按“签名算法”生成的 Base64 签名串 |

### 1.5 其他接口

其他接口定义请参考 [3. 其他接口](#3-common-services)。

- **订单查询：**详见[3.1 订单查询](#3-1-order)
- **订单退款：**详见[3.2 订单退款](#3-2-refund)
- **异步通知：**详见[3.3 异步通知](#3-3-notify)

## 易支付兼容接口

兼容易支付、CodePay、VPay 等支付协议

### 2.1 概览

- **协议：**EasyPay / CodePay / VPay 兼容协议
- **服务类型：**仅支持`type=epay`
- **网关基址：**`https://credit.linux.do/epay`
- **订单有效期：**取系统配置`merchant_order_expire_minutes`（平台端设置）

### 2.2 常见错误

- `不支持的请求类型`：`type`仅允许`epay`
- `签名验证失败`：参与签名字段与请求体需一致，密钥直接拼接
- `金额必须大于0`/`积分小数位数不能超过2位`
- `订单已过期`：超出系统配置有效期
- `订单不存在或已完成`：订单号错误、已退回或已完成
- `余额不足`：余额退回时用户积分不足

### 2.3 对接流程

1. 控制台创建 API Key，记录`pid`、`key`，配置回调地址
2. 按“签名算法”生成`sign`，调用`/epay/pay/submit.php`创建积分流转服务并跳转认证界面
3. 可通过`/epay/api.php`轮询结果，或等待异步回调
4. 退回服务时，携带同一`trade_no`和原积分数量，调用积分退回接口
5. 回调验签通过后返回`success`完成闭环

### 2.4 鉴权与签名

#### 2.4.1 API Key

- `pid`：Client ID
- `key`：Client Secret（妥善保管）
- `notify_url`/`return_url`：应用级默认回调地址（兜底）；创建订单时可在请求中传同名字段作为订单级覆盖，未传时回退应用配置。

#### 2.4.2 签名算法

1. 取所有非空字段（排除`sign`、`sign_type`字段）
2. 将上述字段按 ASCII 升序，依次拼成`k1=v1&k2=v2`
3. 在末尾追加应用密钥：`k1=v1&k2=v2{secret}`
4. 整体进行 MD5，取小写十六进制作为`sign`

```
payload="money=10&name=Test&out_trade_no=M20250101&pid=001&type=epay"
sign=$(echo -n "${payload}${SECRET}" | md5)  # 输出小写
```

### 2.5 积分流转服务

- **方法：**POST`/epay/pay/submit.php`
- **编码：**`application/json`或`application/x-www-form-urlencoded`
- **成功：**验签通过后，平台自动创建积分流转服务，并跳转到认证界面（Location=`https://credit.linux.do/paying?order_no=...`）
- **失败：**返回 JSON`{"error_msg":"...", "data":null}`

| 参数 | 必填 | 说明 |
| --- | --- | --- |
| pid | 是 | Client ID |
| type | 是 | 固定 epay |
| out_trade_no | 否 | 业务单号，建议全局唯一 |
| name | 是 | 标题，最多 64 字符 |
| money | 是 | 积分数量，最多 2 位小数 |
| notify_url | 否 | 会参与签名；可选订单级异步通知地址。长度不超过 100，需为合法 URL。传入后支付成功优先回调该地址，未传则使用应用 notify_url |
| return_url | 否 | 会参与签名；可选订单级回跳地址。长度不超过 100，需为合法 URL。传入后支付成功页面优先跳转该地址，未传则使用应用 redirect_uri |
| device | 否 | 终端标识，可选 |
| sign | 是 | 按“签名算法”生成 |
| sign_type | 否 | 固定 MD5 |

请求示例：

```
curl -X POST https://credit.linux.do/epay/pay/submit.php \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "pid=001" \
  -d "type=epay" \
  -d "out_trade_no=M20250101" \
  -d "name=Test" \
  -d "money=10" \
  -d "sign=${SIGN}" \
  -d "sign_type=MD5"
```

### 2.6 其他接口

其他接口定义请参考 [3. 其他接口](#3-common-services)。

- **订单查询：**详见[3.1 订单查询](#3-1-order)
- **订单退款：**详见[3.2 订单退款](#3-2-refund)
- **异步通知：**详见[3.3 异步通知](#3-3-notify)

## 其他接口

官方接口与易支付兼容接口公用接口。

### 3.1 订单查询

- **方法：**GET`/epay/api.php`
- **认证：**`pid`+`key`
- **说明：**`out_trade_no`必填；`act`可传`order`，后端不强校验。

| 参数 | 必填 | 说明 |
| --- | --- | --- |
| act | 否 | 可选字段，建议 order |
| pid | 是 | Client ID |
| key | 是 | Client Secret |
| out_trade_no | 是 | 业务单号 |

成功响应：

```
{
  "code": 1,
  "msg": "查询订单号成功！",
  "trade_no": "M20250101",
  "out_trade_no": "M20250101",
  "type": "epay",
  "pid": "001",
  "addtime": "2025-01-01 12:00:00",
  "endtime": "2025-01-01 12:01:30",
  "name": "Test",
  "money": "10",
  "status": 1
}
```

**补充：**`status` 1=成功，0=失败/处理中；不存在会返回 HTTP 404 且 `{"code":-1,"msg":"服务不存在或已完成"}`。

### 3.2 订单退款

- **方法：**POST`/epay/api.php`
- **编码：**`application/json`或`application/x-www-form-urlencoded`
- **限制：**仅支持对已成功的积分流转服务进行积分的全额退回

| 参数 | 必填 | 说明 |
| --- | --- | --- |
| pid | 是 | Client ID |
| key | 是 | Client Secret |
| trade_no | 是 | 编号 |
| money | 是 | 必须等于原积分流转服务的积分数量 |
| out_trade_no | 否 | 业务单号（兼容） |

响应：

```
{ "code": 1, "msg": "退款成功" }
```

**常见失败：**服务不存在/未认证、金额不合法（<=0 或小数超过 2 位）。

### 3.3 异步通知

- **触发：**认证成功后；失败自动重试，最多 5 次（单次 30s 超时）
- **目标：**订单级 notify_url（如有）优先，否则回退到创建应用时设置的 notify_url
- **方式：**HTTP GET

| 参数 | 说明 |
| --- | --- |
| pid | Client ID |
| trade_no | 编号 |
| out_trade_no | 业务单号 |
| type | 固定 epay |
| name | 标题 |
| money | 积分数量，最多 2 位小数 |
| trade_status | 固定 TRADE_SUCCESS |
| sign | 按“签名算法”生成 |

应用需返回 HTTP 200 且响应体为 `success`（大小写不敏感），否则视为失败并继续重试。

### 3.4 商户分发接口

- **方法：**POST`/lpay/distribute`
- **编码：**`application/json`
- **认证：**Basic Auth (使用`client_id:client_secret`进行 Base64 编码)

| 参数 | 必填 | 说明 |
| --- | --- | --- |
| user_id | 是 | 收款人用户 ID (数字) |
| username | 是 | 收款人用户名 (用于二次校验) |
| amount | 是 | 分发积分数量，最多 2 位小数 |
| out_trade_no | 否 | 商户自定义单号 |
| remark | 否 | 分发备注 |

**成功响应：**`{"code":1, "data":{"trade_no":"...", "out_trade_no":"..."}}`

### 3.5 用户余额统计

- **方法：**GET`/api/v1/dashboard/stats/user-balance`
- **认证：**无需鉴权（公开接口）
- **说明：**获取平台所有用户可用余额的统计数据，结果有缓存，TTL 由系统配置决定

成功响应：

```
{
  "error_msg": "",
  "data": {
    "total_count": 1234,
    "total_amount": "98765.00",
    "avg_amount": "80.03",
    "median_amount": "50.00",
    "min_amount": "0.00",
    "max_amount": "9999.00",
    "std_dev": "123.45"
  }
}
```

| 字段 | 说明 |
| --- | --- |
| total_count | 统计用户总数 |
| total_amount | 所有用户可用余额之和 |
| avg_amount | 平均余额 |
| median_amount | 余额中位数 |
| min_amount | 最小余额 |
| max_amount | 最大余额 |
| std_dev | 余额标准差 |
