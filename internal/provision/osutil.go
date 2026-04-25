package provision

import "os"

// osGetenv wraps os.Getenv so it can be stubbed in tests.
var osGetenv = os.Getenv
