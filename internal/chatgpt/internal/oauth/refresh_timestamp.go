package oauth

import (
	"encoding/json"
	"strings"
	"time"
)

const maxRefreshUnix int64 = 253402300799 // Last second representable by an RFC3339 year.

// RefreshTimestamp normalizes optional provider scheduling metadata to Unix
// seconds. Unlike credentials, malformed scheduling hints must not prevent a
// rotated refresh token from reaching the durable pending checkpoint. An absent
// or invalid hint leaves the normal expiry-based refresh policy in effect.
type RefreshTimestamp int64

func (t *RefreshTimestamp) UnmarshalJSON(data []byte) error {
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
	if seconds, numeric := ceilRefreshSeconds(raw); numeric {
		*t = RefreshTimestamp(seconds)
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
		if seconds > 0 && seconds <= maxRefreshUnix {
			*t = RefreshTimestamp(seconds)
		}
	}
	return nil
}

// ceilRefreshSeconds computes a decimal/exponent ceiling lexically. It never
// expands an exponent or rounds through floating point; input-sized work and a
// twelve-digit integer bound keep arbitrary optional metadata inexpensive.
func ceilRefreshSeconds(raw string) (int64, bool) {
	if raw == "" {
		return 0, false
	}
	negative := false
	if raw[0] == '+' || raw[0] == '-' {
		negative = raw[0] == '-'
		raw = raw[1:]
	}

	// Split the exponent and decimal point before interpreting the digits.
	// Numeric strings may omit either side of the point, but need a digit.
	mantissa := raw
	exponent := 0
	if i := strings.IndexAny(raw, "eE"); i >= 0 {
		mantissa = raw[:i]
		var ok bool
		exponent, ok = boundedExponent(raw[i+1:], len(raw)+16)
		if !ok {
			return 0, false
		}
	}
	whole, fraction, _ := strings.Cut(mantissa, ".")
	digits := whole + fraction
	if digits == "" || !decimalDigits(whole) || !decimalDigits(fraction) {
		return 0, false
	}
	if negative {
		return 0, true
	}
	significant := strings.TrimLeft(digits, "0")
	if significant == "" {
		return 0, true
	}
	position := len(whole) + exponent - (len(digits) - len(significant))
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
	if seconds > maxRefreshUnix {
		return 0, true
	}
	return seconds, true
}

func decimalDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Beyond the input length plus the output bound, only the sign matters.
func boundedExponent(raw string, limit int) (int, bool) {
	sign := 1
	if digits, negative := strings.CutPrefix(raw, "-"); negative {
		raw = digits
		sign = -1
	} else {
		raw = strings.TrimPrefix(raw, "+")
	}
	if raw == "" || !decimalDigits(raw) {
		return 0, false
	}
	exponent := 0
	for i := range len(raw) {
		exponent = exponent*10 + int(raw[i]-'0')
		if exponent >= limit {
			return sign * limit, true
		}
	}
	return sign * exponent, true
}
