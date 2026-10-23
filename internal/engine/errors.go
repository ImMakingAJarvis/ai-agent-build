package engine

import "errors"

var ErrTimeout = errors.New("engine: timed out waiting for service state change")
