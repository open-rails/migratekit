package migratekit

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
)

// SemanticContentDigest hashes SQL tokens instead of file formatting. Comments,
// whitespace, and unquoted-identifier case do not affect it; quoted and
// dollar-quoted bodies remain byte-exact because whitespace inside them can be
// executable data.
func SemanticContentDigest(content string) string {
	tokens := sqlTokens(canonicalBody(content))
	hash := sha256.New()
	hash.Write([]byte("migratekit-postgresql-tokens-v1\x00"))
	for _, token := range tokens {
		hash.Write([]byte(token))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func sqlTokens(sql string) []string {
	var out []string
	for i := 0; i < len(sql); {
		switch {
		case isSQLSpace(sql[i]):
			i++
		case i+1 < len(sql) && sql[i:i+2] == "--":
			i += 2
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
		case i+1 < len(sql) && sql[i:i+2] == "/*":
			i = skipBlockComment(sql, i)
		case sql[i] == '\'' || sql[i] == '"':
			start := i
			i = skipQuoted(sql, i, sql[i])
			out = append(out, sql[start:i])
		case sql[i] == '$':
			if end := dollarTagEnd(sql, i); end > i {
				tag := sql[i:end]
				if close := strings.Index(sql[end:], tag); close >= 0 {
					close += end + len(tag)
					out = append(out, sql[i:close])
					i = close
					continue
				}
			}
			out = append(out, "$")
			i++
		case isSQLWord(sql[i]):
			start := i
			for i < len(sql) && isSQLWord(sql[i]) {
				i++
			}
			out = append(out, strings.ToLower(sql[start:i]))
		case isSQLOperator(sql[i]):
			start := i
			for i < len(sql) && isSQLOperator(sql[i]) {
				i++
			}
			out = append(out, sql[start:i])
		default:
			out = append(out, sql[i:i+1])
			i++
		}
	}
	return out
}

func skipBlockComment(sql string, start int) int {
	depth := 1
	for i := start + 2; i < len(sql); {
		switch {
		case i+1 < len(sql) && sql[i:i+2] == "/*":
			depth++
			i += 2
		case i+1 < len(sql) && sql[i:i+2] == "*/":
			depth--
			i += 2
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return len(sql)
}

func skipQuoted(sql string, start int, quote byte) int {
	for i := start + 1; i < len(sql); i++ {
		if sql[i] == '\\' && quote == '\'' && i+1 < len(sql) {
			i++
			continue
		}
		if sql[i] != quote {
			continue
		}
		if i+1 < len(sql) && sql[i+1] == quote {
			i++
			continue
		}
		return i + 1
	}
	return len(sql)
}

func dollarTagEnd(sql string, start int) int {
	for i := start + 1; i < len(sql); i++ {
		if sql[i] == '$' {
			return i + 1
		}
		if !(sql[i] == '_' || unicode.IsLetter(rune(sql[i])) || i > start+1 && unicode.IsDigit(rune(sql[i]))) {
			return -1
		}
	}
	return -1
}

func isSQLSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\r' || b == '\n' || b == '\f' }
func isSQLWord(b byte) bool {
	return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b >= 0x80
}
func isSQLOperator(b byte) bool { return strings.ContainsRune("+-*/<>=~!@#%^&|`?", rune(b)) }
