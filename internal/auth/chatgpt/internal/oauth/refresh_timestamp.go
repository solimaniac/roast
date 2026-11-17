package oauth

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxRefreshUnix int64 = 253402300799 // Last second of the RFC 3339 year 9999.

// RefreshTimestamp is an optional provider hint, stored as whole Unix seconds.
// An unrecognized hint leaves expiry-based refresh in effect. It must never
// prevent a rotated refresh token from reaching storage.
type RefreshTimestamp int64

var (
	// Numeric strings may use a leading plus, a decimal point, or an exponent.
	epochNumber     = regexp.MustCompile(`^\+?([0-9]*)(?:\.([0-9]*))?(?:[eE]([+-]?[0-9]+))?$`)
	nonzeroFraction = regexp.MustCompile(`[.,]0*[1-9]`)
)

func (t *RefreshTimestamp) UnmarshalJSON(data []byte) error {
	raw := strings.TrimSpace(string(data))
	if strings.HasPrefix(raw, "\"") {
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		raw = strings.TrimSpace(raw)
		if instant, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			seconds := instant.Unix()
			// time.Parse truncates fractions beyond nanoseconds. Round up even
			// when the first nonzero digit was beyond that precision.
			if nonzeroFraction.MatchString(raw) {
				seconds++
			}
			*t = refreshTimestamp(seconds)
			return nil
		}
	}
	*t = ceilEpochSeconds(raw)
	return nil
}

func refreshTimestamp(seconds int64) RefreshTimestamp {
	if seconds <= 0 || seconds > maxRefreshUnix {
		return 0
	}
	return RefreshTimestamp(seconds)
}

// ceilEpochSeconds places the decimal point using digit counts. A float would
// round away tiny fractions and allow refresh before the provider's deadline.
func ceilEpochSeconds(raw string) RefreshTimestamp {
	parts := epochNumber.FindStringSubmatch(raw)
	if parts == nil {
		return 0
	}
	whole, fraction, exponent := parts[1], parts[2], parts[3]
	digits := strings.TrimLeft(whole+fraction, "0")
	if digits == "" {
		return 0
	}

	// ParseInt saturates on overflow. Compare the shift before adding it, so
	// even an arbitrarily large exponent takes only input-sized work.
	shift, _ := strconv.ParseInt(exponent, 10, 64) // An omitted exponent is zero.
	integerDigits := int64(len(digits) - len(fraction))
	switch {
	case shift <= -integerDigits:
		return 1 // A positive value below one second.
	case shift > 12-integerDigits:
		return 0 // Outside the twelve-digit RFC 3339 range.
	}
	end := int(integerDigits + shift)
	var seconds int64
	for i := range end {
		seconds *= 10
		if i < len(digits) {
			seconds += int64(digits[i] - '0')
		}
	}
	if end < len(digits) && strings.ContainsAny(digits[end:], "123456789") {
		seconds++
	}
	return refreshTimestamp(seconds)
}
