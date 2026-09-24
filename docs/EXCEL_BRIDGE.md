# Linux / Docker Excel Responses 桥接（实验性）

## 同一 OAuth 账号双路由（开发中）

### 后台开关（推荐）

在 **账号 → 指纹与快捷配置 → Excel 上游（实验性）** 打开开关，
选择「使用此账号 OAuth」，点击保存即可；不再需要手写 `routes.json`。
设置保存在账号数据库中，保存后立即更新运行时，重启后仍保留。
显式关闭优先于旧 `routes.json`，不会被旧文件重新开启。

服务器仍需部署账号模式的 bridge，并设置 `EXCEL_BRIDGE_URL`、
`EXCEL_BRIDGE_API_KEY`。未配置时界面显示提示并禁止从关闭状态开启；
这仅检查服务器配置是否存在，不代表上游登录有效或 bridge 已健康。
选择「服务器独立会话文件」仍需管理员提供 `<账号ID>.json`。
该开关不提供 Word profile，也不表示真实上游已通过验证。

已新增按现有数据库账号 ID 绑定的路由入口，不再需要新建中转账号。
目前 Go 主工作区存在其他编译错误，尚未完成完整端到端和 Docker 验证，
此模式不要直接作为已验证的生产配置部署。

在凭证目录放置 `routes.json`（参考 `deploy/excel-bridge/routes.example.json`）：
键是后台现有 OAuth 账号的数据库 ID，不是 ChatGPT workspace ID。
`credential_mode: "oauth"` 在每次请求时读取该账号当前 access token；
该 token 能否被 Excel 接受需要真实账号验证，不会自动回退。
`credential_mode: "session_file"` 则读取同目录 `<数据库账号ID>.json`，
格式同 session.example.json，且会校验 ChatGPT account ID 与原账号一致。
不要直接使用示例中的账号 ID。

使用自行构建、包含本次 Go 修改的 codex2api 镜像；官方 latest 不包含这些修改。
叠加顺序：

```sh
docker compose -f docker-compose.yml -f docker-compose.excel.yml \
  -f docker-compose.excel-accounts.yml up -d --build
```

SQLite 用户替换第一个文件名。此命令只会构建 bridge，
codex2api 镜像需先自行构建并在基础 Compose 中指定。

此模式沿用原账号调度身份，不复制账号，因此同一账号的入场、释放及用量
归属仍使用原 Account ID。尚未通过并发争抢/失败重试的全链路测试。
目前原生接入仅针对 HTTP `/v1/responses`，客户端使用三个 `*-excel` 别名。
模型自动发现、后台路由编辑 UI、compact 和其他协议入口尚待完善。

工具缓存以账号、会话、凭证指纹隔离。凭证轮换、缓存淘汰或进程重启后，
缺失原生工具记录的续接会返回 409，要求新建会话，不伪造续链。
需提供稳定 `prompt_cache_key` 或会话标识。没有共享持久缓存，
因此不要部署多个 bridge 副本轮询同一工具会话。

---

这是 **codex2api → 独立 Excel bridge → Excel 插件后端** 的部署集成，
不是 Go 原生 Excel 账号类型。使用现有「OpenAI Responses」账号入口，
无需改动 codex2api 的数据库或替换现有账号。

桥接复用 Nonary/ghcp_proxy 的固定提交
`ad23ce2db3b5212c0355762d981c3877322fb160` 的协议转换代码。
构建时需要能访问 GitHub 和 PyPI。其许可证与来源见
`deploy/excel-bridge/NOTICE`。这是非官方、实验性接口，
不能保证后端长期兼容；仅使用自己的合法会话，遵守上游计费及配额限制。

## 1. 准备你自己的 Excel 会话

Linux 容器不安装 Excel，也不读取 Windows/macOS 浏览器缓存。
需要在已登录官方 Excel 插件的设备上取得自己的有效会话，
通过可信的私有通道将请求头保存到服务器。不要把 Token 发到聊天、
工单或仓库，也不要使用普通 Codex API Key/refresh token 替代 Excel 会话。

服务器创建项目目录以外的凭证目录，例如 `/opt/codex2api-excel`。
复制 `deploy/excel-bridge/session.example.json` 为该目录中的 `session.json`，
用自己的实际请求头替换占位符：

```json
{
  "headers": {
    "authorization": "Bearer YOUR_EXCEL_SESSION_TOKEN",
    "chatgpt-account-id": "YOUR_ACCOUNT_ID",
    "x-openai-account-user-id": "YOUR_ACCOUNT_USER_ID"
  }
}
```

如果自己的插件请求带有 `bps_tools_version_id`，可将它作为顶层
`"tools_version_id": "实际值"` 导入。不要编造账号 ID 或工具版本号。

容器以 UID/GID `10001:10001` 运行，目录和文件需允许它读取：

```sh
sudo chown -R 10001:10001 /opt/codex2api-excel
sudo chmod 700 /opt/codex2api-excel
sudo chmod 600 /opt/codex2api-excel/session.json
```

这里只读挂载目录，不复制凭证到镜像。每次请求重新加载文件；
过期、损坏或文件消失会拒绝请求，不会回退到旧 Token。
没有自动续期：过期后从自己的已登录插件刷新会话并替换文件即可，
不必重建镜像。替换后的文件也须保留上述所有权与权限。

## 2. 设置部署变量

在现有 `.env` 中增加（不要重复已有项）：

```dotenv
EXCEL_BRIDGE_API_KEY=替换为至少32字符的随机内部密钥
EXCEL_SESSION_DIR=/opt/codex2api-excel
```

可用 `openssl rand -hex 32` 生成内部密钥。这个密钥只用于
**codex2api → bridge**，不是 Excel Token，也不是最终用户调用 codex2api 的 Key。

## 3. 启动 Compose 叠加配置

在仓库根目录执行。PostgreSQL 版：

```sh
docker compose -f docker-compose.yml -f docker-compose.excel.yml up -d --build
```

SQLite 版：

```sh
docker compose -f docker-compose.sqlite.yml -f docker-compose.excel.yml up -d --build
```

后续 `logs`、`ps`、`down` 等命令也使用同一组 `-f` 参数。
叠加配置为 codex2api 增加专用桥接网络，保留原配置中的其他网络。
bridge 不向宿主机暴露端口，不开放 GHCP 管理面板、凭证写入接口、
Copilot 路由或桌面自动更新。镜像只读、非 root 运行，TLS 校验开启。

## 4. 在 codex2api 后台添加账号

使用现有 **OpenAI Responses** 类型：

| 项目 | 值 |
| --- | --- |
| Base URL | `http://excel-bridge:8000/v1` |
| API Key | 上面的 `EXCEL_BRIDGE_API_KEY` |
| 上游 WebSocket | 关闭，使用 HTTP/SSE |
| 模型 | 获取模型列表，或手动填下面三个别名 |

```text
gpt-5.6-luna-excel
gpt-5.6-terra-excel
gpt-5.6-sol-excel
```

不要将这些别名映射成去掉 `-excel` 的名字：bridge 按别名识别路由。
按现有流程给账号配置分组及可访问的客户端 API Key，并执行连接测试。
客户端仍连接 codex2api，选择上述别名即可；不要把 bridge Key 发给客户端。

## 5. 状态检查与限制

`/health` 仅表示进程存活，不证明会话有效。
以下命令在容器内部查询鉴权保护的会话状态，不输出 Token：

```sh
docker compose -f docker-compose.yml -f docker-compose.excel.yml exec excel-bridge \
  python -c 'import os,urllib.request; r=urllib.request.Request("http://127.0.0.1:8000/session/status",headers={"Authorization":"Bearer "+os.environ["EXCEL_BRIDGE_API_KEY"]}); print(urllib.request.urlopen(r).read().decode())'
```

SQLite 用户替换第一个 Compose 文件名。

- 返回 `{"ready":true}` 仅表示文件校验通过且 JWT 未知过期/尚未过期，
  上游是否接受会话仍以实际连接测试为准。
- 503：检查文件、权限和会话有效期；401：检查内部 bridge Key
  或实际请求返回的上游认证错误。
- 原生入口仅提供 `/v1/models` 和 `/v1/responses`，不提供
  `/v1/responses/compact`、WebSocket、Chat Completions 或 Anthropic 入口。
  不要给该上游开启原生 compact/WebSocket 功能。
- 支持流式/非流式，复用固定版本的工具调用和历史回放转换。
  工具仍由客户端执行，bridge 不执行 OfficeJS。
- 请求必须带完整 `input` 历史，不支持 `previous_response_id`；
  单次请求最大 16 MiB。
- 一个 bridge 对应一个 Excel 会话。使用一个 worker 保留进程内工具调用缓存。
  重启或切换账号时应结束在途工具循环，并在客户端开启新会话。
  多账号应分别部署独立实例，不共用会话文件或工具缓存。

## 开发测试

创建独立 Python 3.12+ 虚拟环境，下载上述固定提交到单独目录，
安装其 `requirements.txt` 和 `pytest`，然后运行：

```sh
EXCEL_UPSTREAM_SOURCE=/absolute/path/to/ghcp_proxy \
  /absolute/path/to/venv/bin/python -m pytest deploy/excel-bridge/test_bridge.py -q
```

不设置 `EXCEL_UPSTREAM_SOURCE` 时跳过真实适配器 MockTransport 测试，
其他测试仍验证鉴权、路由隔离、请求校验、会话轮换及 SSE。
所有测试使用伪凭证和模拟上游，不消耗真实账号额度。
