package terminalpersistence

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"

	"github.com/tansrai/tansr-go/executor"
)

// CopyOptions selects a new encrypted candidate. Key must differ from the source
// key. CommitHook receives only the existing physical commit stage names.
type CopyOptions struct {
	Path       string
	Key        []byte
	CommitHook func(string) error
}

// CopyVerifiedCutoverPending identifies a persistent read-only candidate. It does
// not attest a writer cutover; this implementation provides no activation API.
func (s *FileStore) CopyVerifiedCutoverPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.copyOnly
}

// CopyTo preserves the source and copies every root, object, index and transfer,
// including staging. The returned candidate has been cold reopened and audited.
// Reopen retains its read-only restriction. Failure never deletes a published
// target: retain its original path/key and reopen it to reconcile the outcome.
func (s *FileStore) CopyTo(ctx context.Context, o CopyOptions) (_ *FileStore, err error) {
	key := append([]byte(nil), o.Key...)
	defer clear(key)
	if !filepath.IsAbs(o.Path) || len(key) != 32 || sha256.Sum256(key) == s.keyDigest {
		return nil, AdapterError("invalid_request")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.discardCompleteView()
	if s.closed {
		return nil, ErrClosed
	}
	if s.uncertain {
		return nil, ErrUnknown
	}
	before, err := s.current()
	if err != nil {
		return nil, err
	}
	if err = s.guard(before, ctx); err != nil {
		return nil, err
	}
	if err = s.load(); err != nil {
		return nil, err
	}
	// New key has its own encryption budget. The original counters and source
	// snapshot are never written, even when the original key budget is exhausted.
	seed := m(detached(s.state))
	seed["writes"], seed["encryptedBytes"] = 0, 0
	targetOptions := s.options
	targetOptions.Path, targetOptions.Mode, targetOptions.Key = o.Path, "create", key
	targetOptions.CurrentScope = func() (executor.Scope, error) {
		if e := s.guard(before, ctx); e != nil {
			return executor.Scope{}, e
		}
		return before, nil
	}
	targetOptions.CommitHook = func(stage string) error {
		if e := s.guard(before, ctx); e != nil {
			return e
		}
		if o.CommitHook != nil {
			if e := o.CommitHook(stage); e != nil {
				return e
			}
		}
		return s.guard(before, ctx)
	}
	openTarget, reopenTarget := openFileStore, OpenFileStore
	if s.indexed != nil {
		openTarget, reopenTarget = openIndexedFileStore, OpenIndexedFileStore
	}
	target, err := openTarget(targetOptions, seed, true)
	if err != nil {
		return nil, err
	}
	// Any later failure may follow a durable target publication, so preserve it
	// and report an unknown copy result without poisoning the unchanged source.
	defer func() {
		if err != nil {
			if target != nil {
				err = errors.Join(err, target.Close())
			}
			err = errors.Join(ErrUnknown, err)
		}
	}()
	if err = target.Close(); err != nil {
		return nil, err
	}
	targetOptions.Mode, targetOptions.CurrentScope, targetOptions.CommitHook = "reopen", s.options.CurrentScope, nil
	target, err = reopenTarget(targetOptions)
	if err != nil {
		return nil, err
	}
	if err = s.load(); err != nil {
		return nil, err
	}
	if err = target.load(); err != nil {
		return nil, err
	}
	defer target.discardCompleteView()
	original, copied := m(detached(s.state)), m(detached(target.state))
	delete(original, "writes")
	delete(original, "encryptedBytes")
	delete(copied, "writes")
	delete(copied, "encryptedBytes")
	// Compare packed rows rather than the control canonical node limit.
	a, e := packState(original, s.options.MaxFileBytes)
	if e != nil {
		return nil, e
	}
	b, e := packState(copied, s.options.MaxFileBytes)
	if e != nil {
		return nil, e
	}
	if !target.copyOnly || string(a) != string(b) {
		return nil, ErrIntegrity
	}
	if err = s.guard(before, ctx); err != nil {
		return nil, err
	}
	return target, nil
}
