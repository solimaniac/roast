package chatgpt

import (
	"encoding/json"
	"strings"
	"time"
)

// refreshTimestamp normalizes optional provider scheduling metadata to Unix
// seconds. Unlike credentials, malformed scheduling hints must not prevent a
// rotated refresh token from reaching the durable pending checkpoint. An absent
// or invalid hint leaves the normal expiry-based refresh policy in effect.
type refreshTimestamp int64

func (t *refreshTimestamp) UnmarshalJSON(data []byte) error {
	*t = 0
	raw := strings.TrimSpace(string(data))
	if strings.HasPrefix(raw, "\"") {
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		raw = strings.TrimSpace(raw)
	}
	// Accept epoch seconds, including numeric strings. Round fractions upward
	// so scheduling never permits refresh before the supplied earliest instant.
	const maxUnix = 253402300799 // Last second representable by an RFC3339 year.
	if seconds, numeric := ceilRefreshSeconds(raw); numeric {
		*t = refreshTimestamp(seconds)
		return nil
	}
	if instant, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		seconds := instant.Unix()
		// time.Parse accepts and truncates fractions longer than nanoseconds.
		// Inspect only the validated fractional digits to retain that remainder.
		fraction := false
		if start := strings.IndexAny(raw, ".,"); start >= 0 {
			for _, digit := range raw[start+1:] {
				if digit < '0' || digit > '9' {
					break
				}
				fraction = fraction || digit != '0'
			}
		}
		if fraction {
			seconds++
		}
		if seconds > 0 && seconds <= maxUnix {
			*t = refreshTimestamp(seconds)
		}
	}
	return nil
}

// ceilRefreshSeconds computes a decimal/exponent ceiling lexically. It never
// expands an exponent or rounds through floating point; input-sized work and a
// twelve-digit integer bound keep arbitrary optional metadata inexpensive.
func ceilRefreshSeconds(raw string) (int64, bool) {
	const maxUnix int64 = 253402300799
	if raw == "" {
		return 0, false
	}
	negative := false
	if raw[0] == '+' || raw[0] == '-' {
		negative = raw[0] == '-'
		raw = raw[1:]
	}
	var digits strings.Builder
	decimal, exponentStart := -1, -1
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		switch {
		case ch >= '0' && ch <= '9':
			digits.WriteByte(ch)
		case ch == '.' && decimal < 0:
			decimal = digits.Len()
		case ch == 'e' || ch == 'E':
			exponentStart = i + 1
		default:
			return 0, false
		}
		if exponentStart >= 0 {
			break
		}
	}
	if digits.Len() == 0 {
		return 0, false
	}
	if decimal < 0 {
		decimal = digits.Len()
	}
	exponent := 0
	if exponentStart >= 0 {
		part := raw[exponentStart:]
		sign := 1
		if len(part) > 0 && (part[0] == '+' || part[0] == '-') {
			if part[0] == '-' {
				sign = -1
			}
			part = part[1:]
		}
		if part == "" {
			return 0, false
		}
		// Beyond the input length plus the output bound, only the sign matters.
		limit := len(raw) + 16
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return 0, false
			}
			if exponent < limit {
				exponent = min(exponent*10+int(ch-'0'), limit)
			}
		}
		exponent *= sign
	}
	if negative {
		return 0, true
	}
	allDigits := digits.String()
	significant := strings.TrimLeft(allDigits, "0")
	if significant == "" {
		return 0, true
	}
	position := decimal + exponent - (len(allDigits) - len(significant))
	if position <= 0 {
		return 1, true
	}
	if position > 12 {
		return 0, true
	}
	var seconds int64
	for i := 0; i < position; i++ {
		seconds *= 10
		if i < len(significant) {
			seconds += int64(significant[i] - '0')
		}
	}
	if position < len(significant) && strings.ContainsAny(significant[position:], "123456789") {
		seconds++
	}
	if seconds > maxUnix {
		return 0, true
	}
	return seconds, true
}
