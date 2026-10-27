# Go SDK 开发手册

适用版本：**`github.com/tansrai/tansr-go v0.3.0`**，Go 1.25 及以上。SDK 与三个 Demo 采用 MIT；Serve 核心的许可不因此改变。

这份手册面向把智能体集成进现有 Go 应用的开发者。直接通过 `go.mod` 引入 SDK 即可，不需要复制仓库、安装 Demo 或在客户端安装 Node.js。SDK 连接独立运行的 Tansr Serve；Serve 可以在本机，也可以由开发者部署在远端。客户端负责界面、登录接入、明确安装的业务工具及本地档案，Serve/kernel 负责模型循环、会话、上下文组织、权限、裁决和用量。

工具和档案的具体接线见 [Go 工具与档案接入](Go工具与档案接入.md)。本版本的迁移和发行实证见 [GO-03](GO-03-tansrai开源迁移与发布.md)，原 `v0.2.0` 三平台运行证据见 [GO-02](GO-02-三平台运行验收与公开发布.md)，合同依据见 [SDK2/UAPI 冻结记录](GO-01-SDK2与UAPI合同冻结-2026-10-07.md)。

从旧模块 `github.com/cpple/tansr-go v0.2.0` 升级时，须同步把 `go.mod` 和业务代码的全部 Go imports 改为 `github.com/tansrai/tansr-go`，再执行 `go mod tidy`。两种路径具有不同的 Go 类型身份，不混用其客户端、DTO 或错误类型。v0.3.0 只迁移模块身份，API 协议和 SDK 行为不变，历史标签不重打。SDK 与 Demo 的 MIT 范围及上游资料的许可边界见 [NOTICE](../NOTICE.md)。

## 1. 通过 Go Modules 集成

已有项目在模块根目录执行：

```sh
go get github.com/tansrai/tansr-go@v0.3.0
```

加入业务代码及其 SDK `import` 后执行 `go mod tidy`，更新并保留实际使用的依赖。或者先在 `go.mod` 中增加固定版本依赖，再按同样顺序添加代码、运行 `tidy`：

```go.mod
module example.com/my-agent-app

go 1.25.0

require github.com/tansrai/tansr-go v0.3.0
```

`go mod tidy` 会移除未使用依赖，应先加入下面的业务代码。提交应用的 `go.mod` 和 `go.sum`；正式构建不使用指向本地源码的 `replace`，不以 `@main` 代替固定发行版本。当前 SDK 仅使用 Go 标准库，无第三方 Go 运行依赖。普通应用构建不要求 CGO；`-race` 验收另需相应平台支持的 C 工具链。

| 导入路径 | 用途 |
| --- | --- |
| `github.com/tansrai/tansr-go/api` | 统一传输、发现、围栏、错误模型及 manifest 操作 |
| `github.com/tansrai/tansr-go/session` | 多轮会话、事件、审批、提问、取消、同轮输入、历史与快照 |
| `github.com/tansrai/tansr-go/executor` | 显式业务工具、执行器绑定、运行及回执、输出分块 |
| `github.com/tansrai/tansr-go/archive` | 加密档案、耐久后 ACK、材料交接、显式 ACK 恢复 |
| `github.com/tansrai/tansr-go/canonical`、`.../sse` | 需要实现合同适配时使用的底层编码与 SSE 解析 |

仅需要对话时，导入 `api` 和 `session`。不要在业务项目中导入 `examples/internal`；它是 Demo 私有辅助代码。

## 2. Serve 与认证准备

接入前由服务部署者准备以下条件：

1. Serve 已启用统一 `/api` 门面和所选会话族，认证能够确定应用、终端用户及授权代际。SDK 的 `BaseURL` 只填写源地址，例如 `https://serve.example.com`，不附加 `/api`、查询参数、账号或密码。
2. 可信应用配置已选定可用模型和能力范围；模型密钥与平台 `appkey` 留在服务端。登录服务完成用户认证后签发短期终端令牌，客户端使用该令牌调用 Serve。
3. 代理保留统一合同响应头，允许 SSE 持续输出，并配置合适的流空闲超时和缓冲策略。SDK 拒绝跟随重定向；应直接使用最终 Serve 地址。
4. 按需要安装工具执行、档案、音频等扩展，并对目标应用授权。使用 `go-tools` 的 Serve 必须包含 GO-01 首次设备绑定修复，实证源码基线为 `3a5ba4f8`；不能只凭某个旧 npm 包版本猜测已包含修复。

`api.Options.TokenFunc` 在每次请求前调用，适合从应用的令牌管理器读取当前有效令牌；它不会替换已经建立的 SSE 连接所用认证。回调应尊重 `context`，对并发刷新做互斥或合并，并且只续期**同一应用、同一用户**的令牌。切换账号时停止旧事件流、执行器和存储访问，再创建新客户端；不能在旧会话对象下悄悄替换成另一个人的令牌。

`TokenFunc` 不会自动实现登录，也不会在 401 后自行重新登录和重放写请求。`Authorize` 是高级认证头接入点，同样不赋予客户端自报用户身份的权力。生产远程连接使用 HTTPS；下面的 HTTP 地址只用于本机开发。

## 3. 最小流式会话：完整可编译示例

在新的业务项目中执行 `go mod init example.com/my-agent-app`，按第 1 节安装 SDK，将下面完整代码保存为 `main.go`。程序创建一条自己独占的新会话，先订阅事件再发送一次消息，只在收到本轮完成事件后返回成功。它保留会话供后续恢复，不自动关闭会话。

这份入门程序没有交互审批界面：遇到审批、提问或旧式内联工具请求时明确报错并尝试中断，绝不自动同意。需要人工审批、多轮输入和 `/cancel` 的完整交互示例，请运行第 10 节的 `go-chat`。

```go
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/tansrai/tansr-go/api"
	"github.com/tansrai/tansr-go/session"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, safeText(err.Error()))
		os.Exit(1)
	}
}

func readToken(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	token := os.Getenv("TANSR_TOKEN")
	if path := os.Getenv("TANSR_TOKEN_FILE"); path != "" {
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("open token file: %w", err)
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
		if len(b) > 64*1024 {
			return "", errors.New("token file exceeds 64 KiB")
		}
		token = string(b)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("set TANSR_TOKEN_FILE or a short-lived TANSR_TOKEN")
	}
	return token, ctx.Err()
}

func newWrite(deadline time.Time) (session.WriteOptions, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return session.WriteOptions{}, err
	}
	return session.WriteOptions{
		IdempotencyKey: hex.EncodeToString(b[:]), Deadline: deadline,
	}, nil
}

func safeText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, s)
}

func run(ctx context.Context) error {
	base := os.Getenv("TANSR_SERVE_URL")
	if base == "" {
		base = "http://127.0.0.1:8787"
	}
	ht := http.DefaultTransport.(*http.Transport).Clone()
	ht.ResponseHeaderTimeout = 30 * time.Second
	defer ht.CloseIdleConnections()
	transport, err := api.New(api.Options{
		BaseURL: base, SessionFamily: "sdk1", EventEnvelope: true,
		TokenFunc: readToken, HTTPClient: &http.Client{Transport: ht},
	})
	if err != nil {
		return err
	}
	client, err := session.New(transport)
	if err != nil {
		return err
	}
	createCtx, cancelCreate := context.WithTimeout(ctx, 30*time.Second)
	defer cancelCreate()
	createDeadline, _ := createCtx.Deadline()
	createWrite, err := newWrite(createDeadline)
	if err != nil {
		return err
	}
	fmt.Println("create request:", createWrite.IdempotencyKey)
	current, err := client.Create(createCtx, session.CreateOptions{
		WriteOptions: createWrite, // 不在创建时带 Prompt，先订阅再发送。
	})
	if err != nil {
		return err // 不因超时而换键再次创建。
	}
	fmt.Println("session:", safeText(current.ID()))
	meta, err := current.Meta(ctx)
	if err != nil {
		return err
	}
	if !meta.Live || meta.Status != "idle" {
		return errors.New("session is not live and idle")
	}
	stream, err := current.Events(ctx, strconv.FormatInt(meta.LastSeq, 10))
	if err != nil {
		return err
	}
	defer stream.Close()

	completed := false
	attempted := false
	defer func() {
		if !attempted || completed {
			return
		}
		// 关闭 SSE 或取消本地 context 不等于取消 Serve 中的轮次。
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := current.Interrupt(cleanup, session.WriteOptions{}); err != nil {
			fmt.Fprintln(os.Stderr, "interrupt unconfirmed; inspect the same session:", safeText(err.Error()))
		} else {
			fmt.Fprintln(os.Stderr, "interrupt accepted; terminal state still needs reconciliation")
		}
	}()
	prompt := os.Getenv("TANSR_PROMPT")
	if strings.TrimSpace(prompt) == "" {
		prompt = "请用一句话介绍你能提供的帮助。"
	}
	sendCtx, cancelSend := context.WithTimeout(ctx, 30*time.Second)
	defer cancelSend()
	sendDeadline, _ := sendCtx.Deadline()
	write, err := newWrite(sendDeadline)
	if err != nil {
		return err
	}
	fmt.Println("message request:", write.IdempotencyKey)
	attempted = true // 失去响应也可能已经被受理；不自动重发。
	if _, err := current.Send(sendCtx, prompt, write); err != nil {
		return err
	}
	for {
		event, err := stream.Next()
		if err != nil {
			return fmt.Errorf("stream ended without confirmed completion (not success): %w", err)
		}
		switch event.Type {
		case "server.replay.gap":
			return errors.New("event replay gap; inspect history and retain the original request")
		case "server.permission.request", "server.question.request", "server.tool.request":
			return fmt.Errorf("%s requires an interactive host; use go-chat and an installed executor", event.Type)
		case "msg.text.delta":
			var delta struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(event.Raw, &delta); err != nil {
				return err
			}
			fmt.Print(safeText(delta.Text))
		}
		if outcome, terminal := event.TurnOutcome(); terminal {
			if outcome.Status != session.OutcomeCompleted {
				return fmt.Errorf("turn was not completed: %s", outcome.Status)
			}
			completed = true
			fmt.Println("\n[turn completed]")
			return nil
		}
	}
}
```

PowerShell 运行配置：

```powershell
$env:TANSR_SERVE_URL = "http://127.0.0.1:8787"
$env:TANSR_TOKEN_FILE = "$env:LOCALAPPDATA\Tansr\user-token.txt"
go run .
```

macOS / Linux：

```sh
export TANSR_SERVE_URL="http://127.0.0.1:8787"
export TANSR_TOKEN_FILE="$HOME/.config/tansr/user-token.txt"
go run .
```

令牌文件须由你的登录流程安全写入并及时更新。`TANSR_TOKEN` 仅作未设置令牌文件时的替代入口，程序不输出令牌。示例仅在进程内保存请求身份并打印，**不提供崩溃恢复日志**；正式应用应在发出有副作用的请求前持久化请求标识、正文、原截止时间和所属主体。不要仅存幂等键却丢失请求正文，也不要把打印日志当耐久事务。

## 4. 会话族、能力配置与核心边界

所有调用仍走统一 `/api`。`sdk1` 和 `sdk2-offload-v1` 是 `tansr-session-family` 的合同选择，不是要求开发者拼接 `/v2`、`/v3` 或 `/sdk2`。

| 选择 | 使用场景 | 开发者责任 |
| --- | --- | --- |
| `sdk1` | 使用 Serve 现有完整会话管理与已配置持久化；三个 Go Demo 均使用此族 | 管理 sessionId、事件订阅与请求身份；按需叠加执行器和档案扩展 |
| `sdk2-offload-v1` | 明确启用 Source 驱动的历史卸载 | 新建需保留 `CreateOptions.RequestID`；完成 Source、档案及材料生命周期，不能只换族名；当前不支持 fork |

`session.New` 要求显式会话族和 `EventEnvelope: true`。创建时会做只读族发现；不支持就失败，不静默降级。`sdk1` 没有 `CreateOptions.RequestID` 字段语义，写身份通过嵌入的 `WriteOptions.IdempotencyKey` 表达。Go 组合字面量需写 `CreateOptions{WriteOptions: session.WriteOptions{...}}`，不能把嵌入字段直接当作外层字面量键。

`Model`、`Profile`、`Tools`、`ClientTools`、`CapabilitiesProfile`、`Budget`、`Cwd` 都是请求配置，不是授权凭据。应用政策和会话能力围栏最终决定是否可用；客户端报告 Windows/Linux/macOS 等平台也不会扩大权限。`Budget` 表达 `MaxUSD` / `MaxTokens` 请求限制，不代替平台计量或开发者自己的收费系统。`Cwd` 或 `SetCwd` 只请求 Serve 按部署政策解析工作区，不能据此访问 Serve 宿主或客户端任意目录。

`Capabilities` 返回会话围栏，操作状态为 `enabled`、`disabled`、`unavailable`。高层会话内写操作（关闭会话除外）会重新取得围栏再请求，服务端仍复核实时授权；`Create` / `Resume` 执行的是会话族发现，创建前没有现成会话围栏。读取成功不保证后续写入一定成功；处理时应保留新的拒绝状态。`ApplicationPromptMeta` 可查看应用提示词元数据；本版 `CreateOptions` 没有直接传入任意系统提示词的字段，不应在请求 JSON 中另造一个键。

## 5. 会话生命周期、人工交互和同轮插入

### 创建、恢复和连续多轮

`Create` 接受 201 新建或 200 附着的合法回执。`Attach` 是只读取得既有引用，不会唤醒休眠会话；需要恢复时使用 `Resume`，依赖 Serve 的持久化和归属检查。恢复失败应展示原错误，不以新建替代。`Close` 请求关闭会话，也可用于休眠或已结束会话；其 `Accepted` 回执不等于所有资源已清理完毕。

多轮对话复用同一 `Session`。发送前检查 `Meta.Status == "idle"`；已有活动轮时观察事件、人工取消或调用同轮输入入口。每个新的业务动作创建新的请求身份，同一次动作的查账与获准重放保留旧身份。不要并发调用多个 `Send` 试图实现同轮追加。

### 事件和恢复观察

`Events(ctx, lastEventID)` 返回 SSE 流，`Next` 逐帧消费，`Close` 关闭连接。统一七键信封和所属会话由 SDK 校验；`Event.Raw` 保留原领域对象，先检查 `Type` 再解析。未知新增事件可以记录或交给扩展处理，不能推断为成功。

| 观察 | 业务含义 |
| --- | --- |
| `msg.text.delta` | 增量文本，不是本轮完成 |
| `tool.started/completed/failed` | 工具状态，不是整轮结果 |
| `turn.completed` + `TurnOutcome()` 返回 `OutcomeCompleted` | 当前被观察轮完成；恢复时还需匹配轮次和事件水位 |
| `turn.aborted`、不可恢复 `turn.error` | 本轮中止/失败 |
| `session.ended` | 会话观察结束，不能冒充本轮成功 |
| HTTP 202、SSE EOF、网络超时 | 受理或传输状态，均不能证明业务完成 |
| `server.replay.gap` | 重放不完整，需结合历史和状态对账，不能自动补猜缺失事件 |

SDK 不自动重连或重发消息。宿主处理完事件后，把应用处理水位与业务状态一起保存；断线后用最后**已处理**的事件位置重新 `Events`。`LastEventID()` 表示 SDK 已读取的帧，不代表你的数据库/UI 已耐久处理，应在应用完成处理后再提交自己的恢复点。一个流只由一个消费循环调用 `Next`；关闭和读取当前游标可并发。

恢复一个活动轮时，旧审批票据和旧轮终态可能重放。应结合保存的 `turnId`、原发送前水位和当前 `Meta.LastSeq` 排除旧终态；不能拿第一条 `turn.completed` 结束当前 UI。`go-chat` 实现了恢复时重放与终态水位检查，适合作为宿主参考。事件游标只续订事件；不能当作档案覆盖或工具输出 ACK。

### 人工审批与提问

收到 `server.permission.request` 后，从原 `Raw` 保存 `requestId`（票据）、`digest` 和供用户理解的摘要；用户确认后调用 `Permission(ctx, ticketID, digest, "allow" 或 "deny", write)`。这里的 `ticketID` 来自服务端，与写请求的 `IdempotencyKey` 不是一回事。收到 `server.permission.closed` 应撤销 UI 上的待决操作；Serve 最终复核归属、摘要与过期，客户端不能延长票据有效期。

收到 `server.question.request` 后呈现原问题，用 `Answer` 回传 `[]session.Answer`；每项包含 `QuestionID`、非 nil 的 `SelectedOptionIDs`（没有选择时使用空切片）和可选 `FreeText`。不能凭模型生成的文字假造已批准事件。完整交互命令见 `go-chat` 的 `/allow`、`/deny`、`/answers`。

### 取消与同轮插入

本地 `context.Cancel`、关闭窗口或 `stream.Close()` 只停止本地等待；需要停止 Serve 当前轮时调用 `Interrupt`，然后继续观察明确终态。网络失败时保留原 sessionId 对账，不把“取消请求已发送”写成“已取消”。应用退出时可短时尝试中断并保存未确认状态，避免无限等待。

同轮追加使用 `InputCapabilities` → `SubmitInput` → `InputStatus`。`Input` 需要稳定 `InputID`、原目标 `HistoryEpoch` / `TurnID`，以及 `Content.Text` 或非空文本块之一。目标信息来自当前会话的输入能力与活动轮状态，不从最近一条旧历史猜测。`Ack` 可为 `memory`、`durable` 或留空接受服务端默认；要求耐久回执的应用先检查 `durableAck` 能力，支持时显式选择 `durable` 并检查实际响应。不支持时应报告能力缺口，不能降为内存回执或仅凭 Serve 配置了存储就推定耐久。

这条入口不会由 SDK 截断或重启当前轮。HTTP 202 仍只表示受理，应按原输入身份查询 `InputStatus` 及事件确认后续处理；文字已入队不等于模型已经消费。活动轮结束、历史代际改变、并发冲突或能力不具备时，应呈现拒绝状态，不自动改投新一轮。当前同轮入口只接文本，不能把图片、音频、视频块塞进 `Input.Content.Blocks`。

## 6. 历史、快照、压缩与多媒体

| API | 使用要点 |
| --- | --- |
| `History(ctx, offset, limit)` | 分页读取 Serve 现有历史；`limit=0` 为计数模式，不下载全历史，也不推进档案 ACK |
| `Checkpoint` / `Checkpoints` / `Restore` | Serve 管理快照和恢复；恢复仍受归属和部署能力约束 |
| `ExportCheckpoint` / `ImportCheckpoint` | 保存和传回原始字节，不自己改写内容；导入上限 32 MiB |
| `Compact` | 使用核心上下文管理器，返回值须分辨 `compacted`、`rejected`、`failed`；HTTP 200 不是压缩成功，SDK 不自动重复付费压缩 |
| `SendBlocks` | 用户消息支持文本、内联 base64 图片；图片类型为 PNG/JPEG/GIF/WebP |
| `Transcribe` | 请求转写草稿；不会自动把结果发送成用户消息 |
| `Speak` | 请求语音结果；检查 `errorCode`、`taskId` 等实际返回内容后再播放，200 不保证已有音频产物 |

音频通过专用能力请求，不表示主模型消息已支持原生音频/视频块。Go SDK 没有录音器、播放器、桌面窗口或移动端控件；这些由你的宿主实现。完整历史保存、记忆材料交接和 Serve 核心上下文组织是不同职责，详见 [工具与档案接入](Go工具与档案接入.md)。

## 7. 错误、请求身份和恢复

`api.ClientError` 表示本地验证、网络或客户端执行失败，`api.ContractUnavailableError` 表示缺少/不符统一合同、代理 HTML 或异常响应，`api.APIError` 为统一服务端错误。分支先看 `APIError.Code` / `RetryAction`，需要领域细节时再看 `Detail.DomainCode`。未包装的少量领域信封用 `api.DomainError` 保留，不猜造统一字段。

业务日志记录操作名、统一码、领域码、`TraceID`、请求身份和经脱敏的会话关联，避免输出整个请求、令牌、工具参数、档案或原始错误 detail。`TraceID` 是观测关联，不能当幂等键。

| `api.Advice(err).Action` | 宿主处理 |
| --- | --- |
| `none` | 展示或修正原因，不自动重放 |
| `same-request` | 仅服务端明确允许且保留原请求时，按等待时间、原键、原正文和原截止重放 |
| `query-status` | 查询原操作/回执；未知副作用不能换键重新执行 |
| `rebind` | 按该能力合同重建绑定/租约并对账旧操作，不抹掉未知执行记录 |
| `refresh` | 重读版本/投影，确认本次操作的状态与前置条件，再决定下一动作 |
| `rediscover` | 重新发现能力与围栏；关闭的能力不改走另一条路由 |

`api.RetrySameRequest` 是显式的一次重放辅助函数，不是自动重试开关。它接收原 `SameRequest`，只对明确可重放的错误生效；默认最多等待 30 秒，并拒绝越过原 `Deadline`。对高层调用不应丢失原低层参数后重新拼一个“差不多”的请求来套用它。网络中断不是服务端承诺 `same-request`；保持原身份先查账。

`api.CallOptions.IfMatch` 来自先前强 ETag，只能用于 manifest 声明 `expectedRevision` 的操作；SDK 不会替你决定冲突的业务合并方式。`session.WriteOptions` 提供幂等键和截止时间，不隐式生成。`context` 的期限约束本地等待，`Deadline` 是传给服务端的原动作截止；正式应用应同时设置并在恢复中保持原值。超过原截止后，需要新业务动作时应由应用明确决定，不能让重试循环偷偷延长它。

## 8. 并发、资源与安全配置

- 每个认证主体保有稳定客户端，复用 HTTP 连接；`TokenFunc`、`Authorize`、`OnContract` 可能由并发请求调用，宿主回调必须并发安全，且不修改共享请求正文或选项。
- 每个会话对新轮发送、同轮输入和控制动作进行业务层协调。一个事件流使用一个 `Next` 消费循环；跨多个会话可以并行，但应用应限制流数、待处理事件队列和工具并发。
- 对 SSE 使用可取消的长生命周期 `context`；普通请求用独立短期限。注入 `http.Client.Timeout` 会覆盖整个 SSE 读取周期，不宜误设为普通请求的短超时；可以配置 `http.Transport` 的建连和响应头超时。自建 Transport 最终调用 `CloseIdleConnections`。
- 默认 JSON/普通字节响应和单个 SSE 帧上限均为 2 MiB。只有确有需要才调整 `MaxResponseBytes` / `MaxEventFrameBytes`；调大并不改变服务端合同上限，也不取消档案、材料和业务结果的独立限额。部分媒体/快照高层调用使用自身 32 MiB 响应限额。
- 本机文件、进程或第三方业务系统的访问只通过明确安装的工具 handler，配合本地授权复核。SDK 不提供默认任意 shell，不以服务端执行兜底缺失的终端能力。
- 保护令牌、执行日志目录、档案密钥及文件。Windows 使用 ACL，不能把 Unix `0600` 当作已配置 Windows 用户隔离。不要把令牌/密钥提交进 `go.mod`、配置样例或仓库。

## 9. 通用 API 与尚未提供的高层编排

高层未包装的操作可通过 `transport.Call(ctx, api.Op..., api.CallOptions{...})` 或相应事件入口调用。操作常量、路径、方法和查询键以 manifest 生成物为准；开发者不自行拼 `/v2` / `/v3` 路由，也不从响应体的 URL 导航。

当前通用层覆盖冻结 r7 的 81 个操作、11 个合同族，并不意味着所有能力都有 Go 一行式封装。严格控制 JSON、重复键拒绝、UTF-8、canonical、摘要、ACK、租约及执行身份仍然有效，不能用任意 struct 的宽松解析绕过。已有高层适配优先复用。

`v0.3.0` 的明确交付范围是高层会话、显式业务工具、单端有界加密档案及三个命令行 Demo。未承诺完整记忆发布编排、双副本/跨设备同步、自动副本切换、保留策略与备份、高级缓存编排、与 Node SQLite 文件直接互读或所有 Node/Electron 能力一一对等。ACK 修订恢复已有显式 `archive.RecoverPending`，但默认同步不会新建恢复意图或自动升级旧档案格式；不能把这一项推广成全面自动恢复。

## 10. Demo、测试与排障

业务项目通过 `go.mod` 集成；Demo 是独立的可选学习入口，可直接安装：

```sh
go install github.com/tansrai/tansr-go/examples/go-chat@v0.3.0
go install github.com/tansrai/tansr-go/examples/go-tools@v0.3.0
go install github.com/tansrai/tansr-go/examples/go-archive@v0.3.0
```

命令在 `GOBIN`，未设置时在 `GOPATH/bin`。配置第 2 节短期令牌后，执行 `go-chat -base <Serve源地址>`。`go-chat -resume <sessionId>` 恢复同一个会话；已经有活动轮时省略 `-message`，先观察或取消。`go-tools`、`go-archive` 的权限、身份和密钥要求见 [专项接入文档](Go工具与档案接入.md)。安装 Demo 不会在你的应用中添加 SDK 依赖，两者用途不同。

开发应用时围绕自己的用户流程测试会话完成、人工审批、断线恢复、取消和未知结果；以 `httptest` 测试 UI/业务分支，再用部署中的 Serve 验证真实配置、认证和能力闭合。仓库本身的开发门见 [AGENTS.md](../AGENTS.md)，包括 vet、测试、race 条件门、构建和冻结合同检查。原 `v0.2.0` 三平台验证环境和合成模型边界见 [GO-02](GO-02-三平台运行验收与公开发布.md)，新模块验收和公开 `go.mod` 消费证据见 [GO-03](GO-03-tansrai开源迁移与发布.md)，不将其等同于你的生产配置已验收。

| 现象 | 排查入口 |
| --- | --- |
| `BaseURL` 无效 | 只传 `http(s)://host[:port]`，去掉路径、用户信息、query 和 fragment |
| 401 或令牌回调失败 | 检查短期令牌有效性、同一主体续期、令牌文件读取；不要输出凭据 |
| 合同不可用 / 缺统一头 / 返回 HTML | 确认直达正确 Serve、门面已装配、代理保留响应头；不要回退旧路由 |
| 能力 `disabled` / `unavailable` | 查看会话 closure、应用配置、当前状态及扩展安装；平台声明不能代替授权 |
| 412 `closure_stale` | 重新发现能力，保留原操作身份并核对实际状态；不要用旧围栏循环重试 |
| 409 活动轮冲突 | 等待原轮、明确取消或按目标调用同轮输入；不要重新创建会话掩盖冲突 |
| 没有文字或只收到 202 | 持续读取 SSE，检查代理缓冲、待决审批/问题、工具执行及最终事件 |
| EOF / replay gap | 原会话、原请求对账；重新订阅或核对历史，不能打印成功或重复发消息 |
| 档案完整性、错密钥或 ACK 冲突 | 保存原介质与原请求，按专项文档处理；不清空重建或提前确认覆盖 |
| `go get` 网络失败 | 核查企业网络、Go 代理和 sumdb 可达性；保持校验开启，不能用本地 replace 冒充公开发行消费 |

## INIT-01 标准准备入口（2026-10-26，未发布候选）

`session.Client.Prepare(ctx, createOptions, preparationOptions)` 复用原 create/resume 意图，再读取当前能力闭包并调用 SDK2 initialize。使用同一个 API transport 构造会话与 executor 客户端；Scope 必须由宿主当前认证事实提供，不从会话 ID 猜测。`OnSessionConfirmed` 在初始化之前交回原 Session；后续 `PreparationError.Session` 和 `Unwrap()` 保留已确认身份与原错误。失败、取消或失回不会重建、关闭、换键或自动重试。

```go
execution, err := executor.NewClient(transport, currentScope)
if err != nil { return err }
prepared, err := sessions.Prepare(startupContext, originalCreateOptions, session.PreparationOptions{
    Execution: execution,
    OnSessionConfirmed: func(s *session.Session) { saveConfirmedSessionID(s.ID()) },
    OnProgress: func(p session.PreparationProgress) { showLoading(p.Stage, p.Status) },
})
```

`RequestedTools == nil` 省略字段，非 nil 空切片显式关闭工具。准备 ctx 不保存在返回 Session；后续会话调用传入独立上下文。进度仅报告实际 `connecting → session-confirmed → initializing-capabilities → ready`，不是服务器状态、百分比或授权。同步观察回调 panic 被隔离，回调须快速返回；未配置的设备、供材或事件流不被自动启动，也不虚报对应可选阶段完成。需要 terminal 供材的应用仍先按既有宿主边界安装相应服务。

`examples/go-chat` 新建/恢复现在使用同一入口，加载阶段写往原输出，确认 ID 在后续失败前可见。新增 `-application`、`-user`、`-authorization-revision`（或已有 `TANSR_APPLICATION_SCOPE_ID` / `TANSR_END_USER_ID` / `TANSR_AUTHORIZATION_REVISION` 环境值），不得伪造；模型、业务工具、事件观察和后续写操作维持原接口。此增量不扩展线协议或合同资产。
### INIT-A12 显式建会前供材（2026-10-26，本地候选）

`PreparationOptions.PreparePersistence` 是可选的开发者端口，接收本次准备的 `context.Context`。只有其返回才表示独立持久化来源真实 ready，SDK 随后才执行原建会/恢复；阶段 `preparing-persistence` 仅在配置时出现。云模式省略该选项，不增加 IO。它不生成端侧来源协议、不把设备 Runner 前移，也不能用空回调冒充供材能力。

回调返回 `nil, nil` 表示只等待外部常驻来源，SDK 无停止权；返回 `PersistenceResource` 明确移交本次 owned 本地资源。成功后保留 `PreparedSession` 并显式调用 `Stop()`，只停止该资源，不关闭远端 Session。Stop 并发调用只实际执行一次，成功或失败结果均保留；未知停止不自动重试。回调抛错前尚未交回的资源由 provider 自行清理，合法迟到返回的资源则在取消后由 SDK 收回且不再 POST。后继失败保留 `PreparationError.Cause` 与确认 Session，`CleanupError` 单列，不替换原 unknown。

provider 必须用独立长期生命周期承接后台工作，传入 ctx 仅控制准备/等待就绪，不得在成功后以 ctx 取消连带停掉长期来源。回调与 Stop 应合作结清，SDK 不启动额外后台任务假装完成。真实来源权限、原身份与写者围栏仍归现有平台/宿主端口判断，本入口没有新增授权。

本批新增 7 项定向反例通过：ready 前零 HTTP、来源失败零 POST、独立 cloud 不受另一个供材等待阻塞、成功移交/借用不停止、late cancel 收回、后继失败与未知清理结果保真、意外 transport panic 时保原 panic 并清理自有资源，以及原 deadline 已过时不调用 provider。首次 borrowed 夹具漏原请求键被原合同拒绝，修夹具后通过，红记录保留。未重跑原全池，未测试真实平台供材通道，不将显式编排接缝代作内置端侧协议已交付。
