package golang

import (
	"fmt"
	"os"
)

const (
	timeoutRequiredErrorStr = `Not all queries have a timeout option configured.
Please add a timeout option to all queries.`
)

// nolint: unused
func warnf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "WARNING: "+format, args...)
}

// nolint: unused
func errorf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", args...)
}
