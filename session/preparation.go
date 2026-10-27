package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tansrai/tansr-go/api"
	"github.com/tansrai/tansr-go/executor"
	"github.com/tansrai/tansr-go/internal/wire"
)

// PreparationStage 是客户端实际工作阶段，不替代服务端权限或运行状态。
type PreparationStage string

const (
	PreparationPersistence              PreparationStage = "preparing-persistence"
	PreparationConnecting               PreparationStage = "connecting"
	PreparationSessionConfirmed         PreparationStage = "session-confirmed"
	PreparationInitializingCapabilities PreparationStage = "initializing-capabilities"
	PreparationReady                    PreparationStage = "ready"
)

type PreparationProgress struct {
	Stage    PreparationStage
	Status   string // started、completed、failed 或 cancelled
	Sequence uint64
	Elapsed  time.Duration
}

// PreparationOptions 仅组合已有接口；不会隐式启动设备、授予工具或订阅事件。
// 回调应快速返回。回调 panic 被隔离，但同步回调的阻塞由宿主负责。
type PreparationOptions struct {
	// 显式独立来源准备：返回表示真实 ready；nil 只借用常驻来源，无停止权。
	// ctx 仅控制准备，不能作为已移交资源的长期生命周期。
	PreparePersistence func(context.Context) (PersistenceResource, error)
	Execution          *executor.Client
	Platform           *executor.Platform
	RequestedTools     []string // nil 保留服务端默认；非 nil 空切片显式清空。
	OnProgress         func(PreparationProgress)
	OnSessionConfirmed func(*Session)
}

type PreparedSession struct {
	Session      *Session
	Capabilities executor.Capabilities
	persistence  *preparationStop
}

// PersistenceResource 只包含本次明确取得的本地资源；不关闭远端会话。
// 准备失败且尚未交出资源时，provider 自行完成内部清理。
type PersistenceResource interface{ Stop() error }

type preparationStop struct {
	resource PersistenceResource
	once     sync.Once
	err      error
}

func (s *preparationStop) stop() error {
	if s == nil {
		return nil
	}
	s.once.Do(func() {
		defer func() {
			if recover() != nil {
				s.err = errors.New("persistence cleanup panicked; completion unknown")
			}
		}()
		s.err = s.resource.Stop()
	})
	return s.err // 未知停止结果也缓存，不自动重试。
}

// Stop 只结清本次 owned 来源；借用来源不受影响，同一结果并发调用只停止一次。
func (p *PreparedSession) Stop() error { return p.persistence.stop() }

// PreparationError 保留已确认会话与原始错误，不能把初始化失败当成建会失败重建。
type PreparationError struct {
	Stage   PreparationStage
	Session *Session
	Cause   error
	// 清理失败独立保留，不能覆盖原请求错误/unknown 或已确认 Session。
	CleanupError error
	persistence  *preparationStop
}

// Stop 返回已尝试停止的原结果，不重放未知清理操作。
func (e *PreparationError) Stop() error { return e.persistence.stop() }

func (e *PreparationError) Error() string {
	return fmt.Sprintf("session preparation %s failed: %v", e.Stage, e.Cause)
}
func (e *PreparationError) Unwrap() error { return e.Cause }

// Prepare 使用原 create/resume 意图及原键。ctx 只控制这次准备，不保存在返回会话中。
// 取消或错误后不关闭远端会话、不重试；调用者可按原键对账。
func (c *Client) Prepare(ctx context.Context, create CreateOptions, options PreparationOptions) (prepared *PreparedSession, err error) {
	var persistence *preparationStop
	defer func() {
		if prepared == nil && persistence != nil {
			cleanup := persistence.stop()
			var failure *PreparationError
			if errors.As(err, &failure) {
				failure.CleanupError = cleanup
				failure.persistence = persistence
			}
		}
	}()
	if ctx == nil || options.Execution == nil || options.Execution.API() != c.API() {
		return nil, &PreparationError{Stage: PreparationConnecting, Cause: invalidBody("preparation requires context and execution client using the same API transport")}
	}
	if !create.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, create.Deadline)
		defer cancel() // 仅准备期；返回的 Session 不持有此 ctx。
	}
	platform := executor.CurrentPlatform()
	if options.Platform != nil {
		platform = *options.Platform
	}
	if err := wire.Validate("sdk2-ext-v1", "ClientPlatform", platform); err != nil {
		return nil, &PreparationError{Stage: PreparationConnecting, Cause: err}
	}
	// 所有工具值仍交既有合同验证；此处提前拒绝重复，避免无效配置先建会。
	if options.RequestedTools != nil {
		seen := make(map[string]bool, len(options.RequestedTools))
		for _, tool := range options.RequestedTools {
			if tool == "" || seen[tool] {
				return nil, &PreparationError{Stage: PreparationConnecting, Cause: invalidBody("invalid or duplicate requested tool")}
			}
			seen[tool] = true
		}
	}
	started := time.Now()
	var sequence uint64
	var confirmed *Session
	emit := func(stage PreparationStage, status string) {
		sequence++
		if options.OnProgress != nil {
			isolatePreparationObserver(func() { options.OnProgress(PreparationProgress{stage, status, sequence, time.Since(started)}) })
		}
	}
	run := func(stage PreparationStage, work func() error) error {
		emit(stage, "started")
		err := ctx.Err()
		if err == nil {
			err = work()
		}
		// 原失败优先；不能把未知提交结果改写为普通取消。
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			status := "failed"
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				status = "cancelled"
			}
			emit(stage, status)
			return &PreparationError{Stage: stage, Session: confirmed, Cause: err}
		}
		emit(stage, "completed")
		return nil
	}
	if options.PreparePersistence != nil {
		if err := run(PreparationPersistence, func() error {
			resource, cause := options.PreparePersistence(ctx)
			// 先接回资源，再核取消/失败；合法 late return 不遗弃句柄。
			if resource != nil {
				persistence = &preparationStop{resource: resource}
			}
			return cause
		}); err != nil {
			return nil, err
		}
	}
	if err := run(PreparationConnecting, func() error {
		var err error
		confirmed, err = c.Create(ctx, create)
		if confirmed != nil {
			// 先保存确认身份，再处理取消或后续失败。
			if options.OnSessionConfirmed != nil {
				isolatePreparationObserver(func() { options.OnSessionConfirmed(confirmed) })
			}
		}
		return err
	}); err != nil {
		return nil, err
	}
	if err := run(PreparationSessionConfirmed, func() error { return nil }); err != nil {
		return nil, err
	}
	var capabilities executor.Capabilities
	if err := run(PreparationInitializingCapabilities, func() error {
		closure, err := confirmed.Capabilities(ctx)
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if state := closure.Operations[api.OpExecutionInitialize]; state != api.StateEnabled {
			return &CapabilityError{Operation: api.OpExecutionInitialize, State: state}
		}
		capabilities, err = options.Execution.Initialize(ctx, confirmed.ID(), platform, options.RequestedTools, closure.ClosureID)
		return err
	}); err != nil {
		return nil, err
	}
	if err := run(PreparationReady, func() error { return nil }); err != nil {
		return nil, err
	}
	return &PreparedSession{Session: confirmed, Capabilities: capabilities, persistence: persistence}, nil
}

func isolatePreparationObserver(observer func()) { defer func() { _ = recover() }(); observer() }
