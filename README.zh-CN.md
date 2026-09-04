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
- Responses 流式事件缺少 `sequence_number` 时会按流内顺序补齐，已有值保持不变

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
```

## 配置

### CPA 插件源

仓库内置了可供 CPA 使用的插件源文件：

`https://raw.githubusercontent.com/yueziji/grok-tool-int-args-plugin/main/registry.json`

把这个 URL 添加到 CPA 的自定义插件源后，CPA 会根据本仓库的 GitHub Release 检查和安装更新。

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `models` | 数组 | `["grok", "xai"]` | 大小写不敏感的模型规则,只在名称开头或分隔符(`-`、`_`、`.`、`/`、`:`、`@`)之后匹配:`xai` 能命中 `xai-beta`、`openrouter/x-ai/grok-4`,不会误伤 `pixai-diffusion`。显式空数组(`models: []`)匹配全部模型;键存在但值为空(`models:` 后面不写)保持默认值 |
| `chat_completions` | 布尔 | `true` | 处理 Chat Completions |
| `responses` | 布尔 | `true` | 处理 Responses / openai-response |
| `include_custom_input` | 布尔 | `false` | 是否同时处理 custom tool 的 `input` |

## 流式说明

不会把流式变成非流式。插件同时支持标准 SSE `data:` 帧和 WebSocket 裸 JSON chunk。Responses 事件缺少 `sequence_number` 时会按流内顺序补齐，已有序号会保留；Responses 的不完整 delta 仍会跳过参数修复。Chat Completions 的跨 chunk 参数按响应/工具 ID 暂存，其他内容继续下发，并在参数组成完整 JSON 后一次性下发修复后的参数。若上游流在参数闭合前就结束（异常中断），收尾 chunk 会把暂存的参数原样冲刷下发，不会丢失。

## 本地构建

需要 Go 1.26+ 且开启 CGO。

```bash
go build -buildmode=c-shared -o grok-tool-int-args.dll .
go test .
CGO_ENABLED=0 go test .
```

## License

MIT
