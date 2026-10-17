package domain

import "errors"

var ErrNotSignedUp = errors.New("agent is not signed up to this platform experiment")

var ErrPlatformExperimentClosed = errors.New("platform experiment is closed")
