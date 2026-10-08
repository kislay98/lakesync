package core

import (
	"errors"
	"fmt"
)

// ErrHistoryLost is returned by a source stream when its bookmark can no longer be
// resumed (for MongoDB: the resume point fell out of the oplog). The engine
// answers with a resync.
var ErrHistoryLost = errors.New("source history lost: the bookmark can no longer be resumed")

// SourceRemovedError is returned when the entity stopped existing at the source
// (dropped, renamed, database dropped, stream invalidated).
type SourceRemovedError struct {
	Reason string
}

func (e *SourceRemovedError) Error() string { return "source removed: " + e.Reason }

// IsSourceRemoved reports whether err is a SourceRemovedError.
func IsSourceRemoved(err error) bool {
	var sr *SourceRemovedError
	return errors.As(err, &sr)
}

// TransientError marks an error worth retrying (network, failover, throttling).
type TransientError struct {
	Err error
}

func (e *TransientError) Error() string { return "transient: " + e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// Transient wraps err as retryable.
func Transient(err error) error {
	if err == nil {
		return nil
	}
	return &TransientError{Err: err}
}

// IsTransient reports whether err is marked retryable.
func IsTransient(err error) bool {
	var t *TransientError
	return errors.As(err, &t)
}

// CommitConflictError is returned by a destination when a commit lost a race and
// should be retried after refreshing metadata.
type CommitConflictError struct {
	Err error
}

func (e *CommitConflictError) Error() string { return fmt.Sprintf("commit conflict: %v", e.Err) }
func (e *CommitConflictError) Unwrap() error { return e.Err }

// IsCommitConflict reports whether err is a CommitConflictError.
func IsCommitConflict(err error) bool {
	var c *CommitConflictError
	return errors.As(err, &c)
}
