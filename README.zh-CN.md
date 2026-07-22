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

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `models` | 数组 | `["grok", "xai"]` | 大小写不敏感的模型子串匹配；空数组表示全部模型 |
| `chat_completions` | 布尔 | `true` | 处理 Chat Completions |
| `responses` | 布尔 | `true` | 处理 Responses / openai-response |
| `include_custom_input` | 布尔 | `false` | 是否同时处理 custom tool 的 `input` |

## 流式说明

不会把流式变成非流式。只在 arguments 已完整的事件上改写，delta 碎片直接跳过。

## 本地构建

需要 Go 1.26+ 且开启 CGO。

```bash
go build -buildmode=c-shared -o grok-tool-int-args.dll .
go test .
```

## License

MIT
