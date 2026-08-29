package metadb

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// IsUUID reports whether s has the canonical 8-4-4-4-12 hexadecimal form.
//
// Checking before a value reaches a query keeps a typo in a URL from becoming a 500: a malformed
// identifier is the caller's mistake, and letting PostgreSQL reject the ::uuid cast would report it
// as the server's.
//
// It lives here rather than in one domain service because every service that reads a row by
// identifier needs the same check, and two copies of it are two places for it to drift. Each
// service still wraps the failure in its own sentinel, because what a client sees is that service's
// decision.
func IsUUID(s string) bool {
	const canonicalLength = 36
	if len(s) != canonicalLength {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// IsUniqueViolation reports a duplicate key, including a partial unique index used as a constraint
// on concurrency — such as the one that allows at most one active job per instance and kind.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// IsForeignKeyViolation reports a reference to a row that does not exist.
func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
