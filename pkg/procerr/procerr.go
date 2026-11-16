// Package procerr holds the typed errors processconcierge returns. Each carries a stable code, the operation that
// failed, a message, optional fields and the cause, so callers can match on the code and still reach the cause with
// errors.Is and errors.As.
package procerr

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Code names a kind of failure. Callers match on it; the message is for people.
type Code string

const (
	// CodeInvalid means the caller passed something unusable, such as no program to run.
	CodeInvalid Code = "invalid"
	// CodeUnsupported means this platform cannot do what was asked yet.
	CodeUnsupported Code = "unsupported"
	// CodeStart means a child process could not be started.
	CodeStart Code = "start"
	// CodeSignal means a signal could not be sent to a child or its group.
	CodeSignal Code = "signal"
	// CodeWait means waiting for a child failed or was cancelled.
	CodeWait Code = "wait"
	// CodeRegistry means the file of process ids could not be read or written.
	CodeRegistry Code = "registry"
)

// Error is a failure with a stable code.
type Error struct {
	Code   Code
	Op     string
	Msg    string
	Fields map[string]any
	Err    error
}

// New returns an error with no cause.
func New(code Code, op, msg string) *Error {
	return &Error{Code: code, Op: op, Msg: msg}
}

// Wrap returns an error that keeps err as its cause.
func Wrap(err error, code Code, op, msg string) *Error {
	return &Error{Code: code, Op: op, Msg: msg, Err: err}
}

// With adds a field, such as the pid, and returns the same error.
func (e *Error) With(key string, value any) *Error {
	if e.Fields == nil {
		e.Fields = map[string]any{}
	}
	e.Fields[key] = value
	return e
}

// Error reads "op: msg (key=value): cause".
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Op)
	b.WriteString(": ")
	b.WriteString(e.Msg)
	if len(e.Fields) > 0 {
		keys := make([]string, 0, len(e.Fields))
		for key := range e.Fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		pairs := make([]string, 0, len(keys))
		for _, key := range keys {
			pairs = append(pairs, fmt.Sprintf("%s=%v", key, e.Fields[key]))
		}
		b.WriteString(" (" + strings.Join(pairs, ", ") + ")")
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error {
	return e.Err
}

// Is reports whether err, or any error it wraps, is a processconcierge error with that code.
func Is(err error, code Code) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}
