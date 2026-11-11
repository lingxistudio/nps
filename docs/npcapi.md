# 客户端 REST API（npc）

客户端（`npc`）可以开一个本地管理 API，用脚本或面板完成这些事：

- 查看每个服务端的连接状态（在线 / 重连中 / 失败原因、连接次数）
- 增删服务端、整体替换服务端列表 → 配置文件模式下自动写回 `npc.conf`
- 修改 `vkey` / `conn_type` / `proxy_url` / `tls_verify` / `auto_reconnection` / `disconnect_timeout` 并热重连
- 强制重连，用于排障

接口风格与 nps 服务端 `/api/v1` 一致：默认关闭（需要 `api_key`）、Bearer 鉴权、来源白名单、每 IP 限流、只读模式、请求体 1MB 上限。

---

## 一、开启

```bash
# 命令行（推荐只监听本机）
./npc -server=nps.example.com:8024 -vkey=123 \
      -api_addr=127.0.0.1:8089 -api_key=change-me
```

```ini
# conf/npc.conf
[common]
server_addr=nps.example.com:8024
vkey=123
api_addr=127.0.0.1:8089
api_key=change-me
api_allow_ip=127.0.0.1,10.0.0.0/8
api_read_only=false
```

优先级：命令行参数 > 配置文件 `[common]` > 环境变量 `NPC_API_ADDR` / `NPC_API_KEY`。

| 配置项 | 命令行 | 说明 |
| --- | --- | --- |
| `api_addr` | `-api_addr` | 监听地址，如 `127.0.0.1:8089`；留空不启动 |
| `api_key` | `-api_key` | 调用凭据；留空时即使配了地址也不会启动（避免无凭据暴露） |
| `api_allow_ip` | `-api_allow_ip` | 来源白名单，支持 IP / CIDR，逗号分隔 |
| `api_read_only` | `-api_read_only` | 只读模式，禁止所有写操作 |

启动成功后日志会有一行 `client api enabled on 127.0.0.1:8089 (read_only=false)`。

---

## 二、认证与错误

```bash
curl -s -H "X-Api-Key: change-me" http://127.0.0.1:8089/api/v1/status
curl -s -H "Authorization: Bearer change-me" http://127.0.0.1:8089/api/v1/status
```

统一响应格式：

```json
{
  "ok": true,
  "data": { },
  "request_id": "a1b2c3d4"
}
```

```json
{
  "ok": false,
  "error": { "code": "invalid_argument", "message": "server address \"b.com\" must be host:port: missing port in address" },
  "request_id": "e5f6a7b8"
}
```

| HTTP | code | 场景 |
| --- | --- | --- |
| 400 | `invalid_argument` | 参数非法、JSON 字段拼错（未知字段直接拒绝） |
| 401 | `unauthorized` | 凭据缺失或错误 |
| 403 | `forbidden` | 来源 IP 不在 `api_allow_ip` |
| 403 | `read_only` | 只读模式下的写操作 |
| 404 | `not_found` | 路径不存在 / 要删除的服务端不存在 |
| 409 | `conflict` | 服务端地址已存在 / 删除最后一个服务端未加 `force` |
| 413 | `payload_too_large` | 请求体超过 1MB |
| 429 | `rate_limited` | 单 IP 超过 300 次/分钟 |

---

## 三、接口

### GET /api/v1/health

无需凭据，用于探活。

```json
{"ok":true,"data":{"status":"ok","role":"npc","version":"0.26.14","uptime_seconds":32,"time":1762900000}}
```

### GET /api/v1/status

```bash
curl -s -H "X-Api-Key: change-me" http://127.0.0.1:8089/api/v1/status
```

```json
{
  "ok": true,
  "data": {
    "version": "0.26.14",
    "uptime_seconds": 128,
    "mode": "file",
    "config_path": "conf/npc.conf",
    "conn_type": "tcp",
    "vkey_masked": "1***",
    "tls_verify": false,
    "auto_reconnection": true,
    "server_count": 2,
    "online_count": 1,
    "hosts": 1,
    "tasks": 4,
    "servers": [
      {
        "addr": "nps-a.example.com:8024",
        "primary": true,
        "state": "online",
        "state_since": "2026-11-11T22:31:02+08:00",
        "connects": 1,
        "reconnects": 0
      },
      {
        "addr": "nps-b.example.com:8024",
        "primary": false,
        "state": "offline",
        "message": "dial tcp 1.2.3.4:8024: i/o timeout",
        "state_since": "2026-11-11T22:31:05+08:00",
        "connects": 0,
        "reconnects": 0
      }
    ]
  }
}
```

`state`：`connecting` / `online` / `offline` / `stopped`。

### GET /api/v1/servers

返回服务端列表与当前是否落盘（`persisted` 表示配置文件模式）。

### PUT /api/v1/servers

整体替换列表。列表顺序即优先级，第一个为主节点（`npc_vkey.txt` 与 `npc status` 只认它）。

```bash
curl -s -X PUT -H "X-Api-Key: change-me" -H "Content-Type: application/json" \
  -d '{"servers":["nps-a.example.com:8024","nps-b.example.com:8024"]}' \
  http://127.0.0.1:8089/api/v1/servers
```

- 仍然存在的地址保持连接不断；新增的启动连接；被移除的连接立即断开（不等重连 sleep）。
- 地址会规范化：`host:port` 强制校验、去重、IPv6 需写成 `[::1]:8024`。
- 配置文件模式下先写 `npc.conf`，写失败直接返回 400 且运行态不变。

### POST /api/v1/servers

```bash
curl -s -X POST -H "X-Api-Key: change-me" -H "Content-Type: application/json" \
  -d '{"addr":"nps-c.example.com:8024"}' http://127.0.0.1:8089/api/v1/servers
```

已存在返回 `409 conflict`。

### DELETE /api/v1/servers?addr=host:port[&force=true]

删除一个服务端。删掉最后一个会让所有隧道中断，因此需要显式 `force=true`，否则返回 `409`。

### POST /api/v1/reconnect

按当前配置重建全部连接（不重写配置文件），用于改完参数或排查网络后立即生效。连接计数会保留累加。

### GET /api/v1/config

```json
{
  "ok": true,
  "data": {
    "mode": "file",
    "config_path": "conf/npc.conf",
    "conn_type": "tcp",
    "vkey_masked": "1***",
    "tls_verify": false,
    "auto_reconnection": true,
    "proxy_url_masked": "socks5://user:***@127.0.0.1:1080",
    "disconnect_timeout": 60,
    "servers": [ ],
    "server_count": 2,
    "api_addr": "127.0.0.1:8089",
    "api_read_only": false,
    "hosts": 1,
    "tasks": 4
  }
}
```

`vkey` 与代理密码只写不读，响应里永远是打码值。

### PATCH /api/v1/config

```bash
curl -s -X PATCH -H "X-Api-Key: change-me" -H "Content-Type: application/json" \
  -d '{"vkey":"newkey","conn_type":"tls","tls_verify":true,"disconnect_timeout":90}' \
  http://127.0.0.1:8089/api/v1/config
```

| 字段 | 校验 |
| --- | --- |
| `vkey` | 1~128 字符，不能含空白 |
| `conn_type` | `tcp` / `kcp` / `tls` |
| `proxy_url` | 空串或 `socks5://` / `http(s)://`，需带主机 |
| `tls_verify` | 布尔 |
| `auto_reconnection` | 布尔 |
| `disconnect_timeout` | 1~3600 秒 |

所有字段先校验再落盘：任何一个非法整体返回 400，不会出现“改了一半”。成功后自动重建全部连接。

---

## 四、与其他功能的配合

- **多服务端**：`PUT /servers` 就是在改 `server_addr`，等价于把多个地址写进配置；每个地址各自独立重连。
- **3389 等端口防护**：隧道级的 `ip_acl` / `auth_user` 仍在服务端或 `npc.conf` 的隧道 section 里配置，客户端 API 只管连接参数，不碰隧道定义，避免误改。
- **后台管理**：nps 服务端的 `/api/v1/tunnels` 管隧道，npc 的 `/api/v1/servers` 管连接，两者可以并存。

## 五、安全建议

1. 只监听 `127.0.0.1`；确实需要跨机调用时配 `api_allow_ip` 并用防火墙限定来源。
2. `api_key` 至少 16 位随机串，不要复用 `vkey`。
3. 面板只读展示时用 `api_read_only=true`。
4. API 会把写操作打到日志（`[api] server ... added from ...`），保留日志便于追溯。
