package coremigrate

import "strings"

// SplitStatements splits SQL into statements and strips comments. It is
// quote-aware: semicolons and comment markers inside '...' strings,
// "..." identifiers, and `...` identifiers are preserved verbatim
// (including ” doubled-quote and backslash escapes inside strings).
//
// Two callers need it, for the same reason: the statements must reach the
// server one at a time. ClickHouse has no transactional DDL, and Postgres
// wraps a multi-statement simple-protocol Exec in an IMPLICIT transaction —
// which is exactly what `-- migratekit:no-transaction` exists to avoid.
//
// It does NOT understand dollar-quoting ($$ ... $$); callers that can meet one
// must reject it rather than mis-split it.
func SplitStatements(sql string) []string {
	sql = strings.ReplaceAll(sql, "\r\n", "\n")
	sql = strings.ReplaceAll(sql, "\r", "\n")

	var out []string
	var b strings.Builder
	i, n := 0, len(sql)

	flush := func() {
		if s := strings.TrimSpace(b.String()); s != "" {
			out = append(out, s)
		}
		b.Reset()
	}

	for i < n {
		c := sql[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			// Copy the quoted region verbatim.
			quote := c
			b.WriteByte(c)
			i++
			for i < n {
				ch := sql[i]
				b.WriteByte(ch)
				i++
				if ch == '\\' && quote == '\'' && i < n {
					b.WriteByte(sql[i])
					i++
					continue
				}
				if ch == quote {
					if i < n && sql[i] == quote {
						b.WriteByte(sql[i])
						i++
						continue
					}
					break
				}
			}
		case c == '-' && i+1 < n && sql[i+1] == '-':
			for i < n && sql[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && sql[i+1] == '*':
			i += 2
			for i+1 < n && !(sql[i] == '*' && sql[i+1] == '/') {
				i++
			}
			if i+1 < n {
				i += 2
			} else {
				i = n
			}
		case c == ';':
			flush()
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	flush()
	return out
}
