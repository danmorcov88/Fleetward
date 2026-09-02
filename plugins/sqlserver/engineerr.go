package sqlserver

import (
	"errors"
	"strings"

	mssql "github.com/microsoft/go-mssqldb"
)

// Reading what the engine said.
//
// SQL Server reports failures as numbered messages, and the numbers are stable across versions and
// languages while the text is neither. Classifying on the number is what lets this plugin tell a
// damaged backup set from a lost connection without matching on English prose — the same reason the
// plugin contract carries typed error codes rather than strings.

// mssqlError is the part of a driver error this plugin acts on.
type mssqlError struct {
	number  int32
	class   uint8
	message string
	// all carries every message the server sent, because the one that explains a failure is often
	// not the last one: RESTORE reports its diagnosis and then terminates with a generic 3013.
	all []int32
}

// asEngineError unwraps to the driver's own error type and reduces it to what is acted on.
//
// The driver returns mssql.Error by value and does not always wrap it, so this walks both the
// errors.As chain and a plain type assertion.
func asEngineError(err error, out *mssqlError) bool {
	var driverErr mssql.Error
	if !errors.As(err, &driverErr) {
		return false
	}

	out.number = driverErr.Number
	out.class = driverErr.Class
	out.message = strings.TrimSpace(driverErr.Message)
	out.all = out.all[:0]
	for _, e := range driverErr.All {
		out.all = append(out.all, e.Number)
	}
	if len(out.all) == 0 {
		out.all = append(out.all, driverErr.Number)
	}
	return true
}

// hasNumber reports whether the server sent a particular message anywhere in the batch.
func (e *mssqlError) hasNumber(numbers ...int32) bool {
	for _, want := range numbers {
		for _, got := range e.all {
			if got == want {
				return true
			}
		}
	}
	return false
}

// engineMessage renders what the server said, short and safe for a message that crosses the
// process boundary.
//
// Only the server's own diagnostic text is used. A driver error's full string can carry the
// connection configuration, and a plugin error's message is written for a human to read in a UI.
func engineMessage(err error) string {
	const limit = 400

	var driverErr mssql.Error
	if !errors.As(err, &driverErr) {
		return "the instance did not accept the statement"
	}

	// The first message is the diagnosis; the last is usually "… is terminating abnormally".
	text := driverErr.Message
	if len(driverErr.All) > 0 {
		text = driverErr.All[0].Message
	}
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > limit {
		text = text[:limit] + "…"
	}
	return text
}
