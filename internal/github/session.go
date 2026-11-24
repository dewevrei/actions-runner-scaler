package github

import (
	"context"
	"sync"

	"github.com/actions/scaleset"

	"github.com/dewevrei/actions-runner-scaler/internal/logging"
	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// Session is a message session. It implements listener.Client: every call
// is named with its retry.Op and returns converted errors, so the session
// loop can classify what listener.Run returns. It is safe for concurrent
// use.
//
// The library refreshes an expired session inside GetMessage, DeleteMessage
// and AcquireJobs; a failed refresh is reported under the calling method's
// Op, which classifies the same way as a refresh (404 is SessionLost, 401
// after the refresh is Permanent). A 4xx from the token exchange the
// refresh may need first is marked retry.ErrAuth and is Permanent.
type Session struct {
	inner   messageSession
	secrets *logging.Secrets

	mu     sync.Mutex
	token  string // message queue token currently registered as a secret
	closed bool   // set by Close; no token is registered afterwards
}

// messageSession is the part of *scaleset.MessageSessionClient that Session
// uses.
type messageSession interface {
	GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error)
	DeleteMessage(ctx context.Context, messageID int) error
	AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error)
	Session() scaleset.RunnerScaleSetSession
	Close(ctx context.Context) error
}

// GetMessage long-polls for the next message after lastMessageID. It
// returns nil when the poll ends without one.
func (s *Session) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	var msg *scaleset.RunnerScaleSetMessage
	err := s.call(ctx, retry.OpGetMessage, func(ctx context.Context) error {
		var err error
		msg, err = s.inner.GetMessage(ctx, lastMessageID, maxCapacity)
		return err
	})
	if err != nil {
		return nil, err
	}
	return msg, nil
}

// DeleteMessage acknowledges messageID.
func (s *Session) DeleteMessage(ctx context.Context, messageID int) error {
	return s.call(ctx, retry.OpDeleteMessage, func(ctx context.Context) error {
		return s.inner.DeleteMessage(ctx, messageID)
	})
}

// AcquireJobs acquires the job requests and returns the IDs acquired.
func (s *Session) AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error) {
	var ids []int64
	err := s.call(ctx, retry.OpAcquireJobs, func(ctx context.Context) error {
		var err error
		ids, err = s.inner.AcquireJobs(ctx, requestIDs)
		return err
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// Session returns the current session. Its MessageQueueAccessToken is a
// secret and must not be logged.
func (s *Session) Session() scaleset.RunnerScaleSetSession {
	return s.inner.Session()
}

// Close deletes the session and unregisters its token. Pass a context that
// is not canceled, such as context.WithoutCancel with a timeout; a canceled
// one fails the delete. A call still running when Close returns does not
// register a token again.
func (s *Session) Close(ctx context.Context) error {
	err := s.call(ctx, retry.OpSessionClose, s.inner.Close)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secrets.Unregister(s.token)
	s.token = ""
	s.closed = true
	return err
}

func (s *Session) call(ctx context.Context, op retry.Op, fn func(context.Context) error) error {
	err := call(ctx, op, s.secrets, fn)
	s.trackToken()
	return err
}

// trackToken keeps the current message queue token, which changes when the
// library refreshes the session, registered as a secret. The token is read
// under the lock, so concurrent calls apply the reads in order and the last
// one leaves the newest token registered. After Close it does nothing, so
// a call that ends after Close cannot leave a token registered for good.
func (s *Session) trackToken() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	current := s.inner.Session().MessageQueueAccessToken
	if current == s.token {
		return
	}
	s.secrets.Register(current)
	s.secrets.Unregister(s.token)
	s.token = current
}
