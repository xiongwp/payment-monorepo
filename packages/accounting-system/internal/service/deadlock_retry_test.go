package service

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// ─── isDeadlockError ─────────────────────────────────────────────────────────

func TestIsDeadlockError_Nil(t *testing.T) {
	assert.False(t, isDeadlockError(nil))
}

func TestIsDeadlockError_PlainError(t *testing.T) {
	assert.False(t, isDeadlockError(errors.New("some other error")))
}

func TestIsDeadlockError_MySQLDeadlock(t *testing.T) {
	err := &mysql.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock"}
	assert.True(t, isDeadlockError(err))
}

func TestIsDeadlockError_MySQLOtherError(t *testing.T) {
	err := &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"}
	assert.False(t, isDeadlockError(err))
}

func TestIsDeadlockError_WrappedDeadlock(t *testing.T) {
	inner := &mysql.MySQLError{Number: 1213}
	wrapped := fmt.Errorf("tcc try account=X: create tcc branch: %w", inner)
	assert.True(t, isDeadlockError(wrapped), "errors.As should unwrap MySQLError across fmt.Errorf %%w")
}

// ─── retryOnDeadlock ─────────────────────────────────────────────────────────

func newTestSvc() *accountingService {
	logger, _ := zap.NewDevelopment()
	return &accountingService{logger: logger}
}

func TestRetryOnDeadlock_SuccessFirstAttempt(t *testing.T) {
	svc := newTestSvc()
	calls := 0
	err := svc.retryOnDeadlock(func() error {
		calls++
		return nil
	})
	assert.NoError(t, err)
	assert.Equal(t, 1, calls)
}

func TestRetryOnDeadlock_SuccessAfterOneDeadlock(t *testing.T) {
	svc := newTestSvc()
	calls := 0
	err := svc.retryOnDeadlock(func() error {
		calls++
		if calls == 1 {
			return &mysql.MySQLError{Number: 1213}
		}
		return nil
	})
	assert.NoError(t, err)
	assert.Equal(t, 2, calls)
}

func TestRetryOnDeadlock_ExhaustedAllRetries(t *testing.T) {
	svc := newTestSvc()
	calls := 0
	err := svc.retryOnDeadlock(func() error {
		calls++
		return &mysql.MySQLError{Number: 1213}
	})
	assert.Error(t, err)
	assert.Equal(t, deadlockMaxRetries, calls)
	assert.Contains(t, err.Error(), "retry exhausted")
}

func TestRetryOnDeadlock_NonDeadlockNoRetry(t *testing.T) {
	// Non-deadlock errors must propagate immediately — no retry, no backoff.
	svc := newTestSvc()
	calls := 0
	bizErr := errors.New("account not found")
	err := svc.retryOnDeadlock(func() error {
		calls++
		return bizErr
	})
	assert.ErrorIs(t, err, bizErr)
	assert.Equal(t, 1, calls, "non-deadlock errors must not trigger retry")
}

// ─── isRetriableMySQLErr: extended error set ─────────────────────────────────

func TestIsRetriableMySQLErr_LockWaitTimeout(t *testing.T) {
	err := &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"}
	assert.True(t, isRetriableMySQLErr(err))
}

func TestIsRetriableMySQLErr_TooManyConnections(t *testing.T) {
	err := &mysql.MySQLError{Number: 1040, Message: "Too many connections"}
	assert.True(t, isRetriableMySQLErr(err))
}

func TestIsRetriableMySQLErr_QueryInterrupted(t *testing.T) {
	err := &mysql.MySQLError{Number: 1317, Message: "Query execution was interrupted"}
	assert.True(t, isRetriableMySQLErr(err))
}

func TestIsRetriableMySQLErr_XADeadlock(t *testing.T) {
	err := &mysql.MySQLError{Number: 1614}
	assert.True(t, isRetriableMySQLErr(err))
}

func TestIsRetriableMySQLErr_DuplicateKeyNotRetriable(t *testing.T) {
	// 1062 = Duplicate entry — business logic error, must NOT retry.
	err := &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}
	assert.False(t, isRetriableMySQLErr(err))
}

func TestIsRetriableMySQLErr_SyntaxErrorNotRetriable(t *testing.T) {
	err := &mysql.MySQLError{Number: 1064, Message: "You have an error in your SQL syntax"}
	assert.False(t, isRetriableMySQLErr(err))
}

func TestRetryOnDeadlock_LockWaitTimeoutRetried(t *testing.T) {
	// 1205 must trigger retry (was not retried in the original deadlock-only impl).
	svc := newTestSvc()
	calls := 0
	err := svc.retryOnDeadlock(func() error {
		calls++
		if calls < 2 {
			return &mysql.MySQLError{Number: 1205}
		}
		return nil
	})
	assert.NoError(t, err)
	assert.Equal(t, 2, calls)
}

func TestRetryOnDeadlock_WrappedDeadlockIsRetried(t *testing.T) {
	// Simulates tccTry's wrapping: `fmt.Errorf("tcc try account=X: %w", mysqlErr)`.
	svc := newTestSvc()
	calls := 0
	err := svc.retryOnDeadlock(func() error {
		calls++
		if calls == 1 {
			return fmt.Errorf("tcc try account=X: create tcc branch: %w",
				&mysql.MySQLError{Number: 1213})
		}
		return nil
	})
	assert.NoError(t, err)
	assert.Equal(t, 2, calls)
}
