# Grok 工具参数整数修复插件

[English](README.md)

CLIProxyAPI（CPA）原生插件：把工具调用 `arguments` 里的整数值浮点（如 `23000.0`）改成整数 `23000`，避免 Codex 因不接受浮点参数而失败。主要针对 Grok/xAI。

## 功能

- 非流式：`response.intercept_after`
- 流式：`response.intercept_stream_chunk`
- 递归处理 `arguments`（JSON 字符串或对象）
- 只改“值上等于整数”的 float，真实小数不动
- 失败放行：半截/非法 JSON 原样返回
- **保持流式**：逐 chunk 处理，不会把流缓冲成非流式
- 跳过不完整的 `response.function_call_arguments.delta`
- Responses 流式事件缺少 `sequence_number` 时，优先根据已下发流历史推导下一个序号；历史被清空时使用按请求保存的兜底序号，已有值保持不变
- 可通过 `repair_sequence_numbers: false` 关闭序号修复

## 运行要求

- CPA 宿主版本 **v7.2.129 或更高**（RPC schema 3）

## 安装

从 Release 下载对应平台产物，解压动态库到 CPA 的插件目录。库文件基名必须是 `grok-tool-int-args`，以便映射到 `plugins.configs.grok-tool-int-args`。

- Windows: `grok-tool-int-args.dll`
- Linux: `grok-tool-int-args.so`
- macOS: `grok-tool-int-args.dylib`

```yaml
plugins:
  enabled: true
  dir: "/absolute/path/to/plugins"
  configs:
    grok-tool-int-args:
      enabled: true
      priority: 20
      models: ["grok", "xai"]
      chat_completions: true
      responses: true
      include_custom_input: false
      repair_sequence_numbers: true
```

## 配置

### CPA 插件源

仓库内置了可供 CPA 使用的插件源文件：

`https://raw.githubusercontent.com/yueziji/grok-tool-int-args-plugin/main/registry.json`

把这个 URL 添加到 CPA 的自定义插件源后，CPA 会从本仓库的 GitHub Release 检查和安装更新。Release 资产严格使用 `grok-tool-int-args_<版本>_<goos>_<goarch>.zip` 命名，并通过随附的 `checksums.txt` 校验。

支持的平台：`windows/amd64`、`linux/amd64`、`linux/arm64`、`darwin/arm64`。

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `models` | 数组 | `["grok", "xai"]` | 大小写不敏感的模型规则,只在名称开头或分隔符(`-`、`_`、`.`、`/`、`:`、`@`)之后匹配:`xai` 能命中 `xai-beta`、`openrouter/x-ai/grok-4`,不会误伤 `pixai-diffusion`。显式空数组(`models: []`)匹配全部模型;键存在但值为空(`models:` 后面不写)保持默认值 |
| `chat_completions` | 布尔 | `true` | 处理 Chat Completions |
| `responses` | 布尔 | `true` | 处理 Responses / openai-response |
| `include_custom_input` | 布尔 | `false` | 同时处理 custom tool 的 JSON `input`，包括 `response.custom_tool_call_input.done` 完成事件；普通文本和不完整 JSON 保持原样 |
| `repair_sequence_numbers` | 布尔 | `true` | 优先根据已下发流历史补齐 `sequence_number`，历史被清空时使用按请求保存的兜底序号 |

## 流式说明

不会把流式变成非流式。插件同时支持标准 SSE `data:` 帧和 WebSocket 裸 JSON chunk。

- Responses 缺失的 `sequence_number` 优先根据已下发流历史推导；单个事件超过宿主的 1 MiB 历史上限，或最近的历史中没有序号时，使用按宿主 `RequestID` 保存的兜底值继续编号。已有序号保持不变。
- Chat Completions 的跨 chunk 参数按请求、响应、choice 和工具隔离暂存，其他内容继续下发；参数组成完整 JSON 后，一次性下发修复后的参数。
- 单个工具的参数超过 1 MiB 时，已暂存内容原样下发，该工具后续片段持续透传，直到 choice 结束。
- 收尾 chunk 会把尚未闭合的暂存参数原样下发。活跃请求的缓存不会因空闲超时被丢弃；宿主的 `request.complete` 通知会在成功、失败、拒绝或取消时清理该请求的缓存和兜底序号，插件关闭也会清理全部状态。
- Responses 的不完整参数 delta 仍跳过参数修复。若一个 JSON 事件被上游拆到两个 chunk，拦截时两半都不是合法 JSON，因此无法修复其 `sequence_number`；这是 chunk 级拦截的固有限制。

## 本地构建

需要 Go 1.26+ 且开启 CGO。

```bash
go build -buildmode=c-shared -o grok-tool-int-args.dll .
go test .
CGO_ENABLED=0 go test .
```

## License

MIT
