package command

import (
	"encoding/json"
	"strconv"
	"strings"
)

var minorExponent = map[string]int{
	"JPY": 0, "KRW": 0, "VND": 0, "XOF": 0, "XAF": 0, "UGX": 0, "RWF": 0,
	"BHD": 3, "KWD": 3, "OMR": 3, "JOD": 3, "TND": 3,
}

// minorInt reads a whole number of minor units from a decoded JSON value.
func minorInt(v any) (int64, bool) {
	switch t := v.(type) {
	case json.Number:
		n, err := strconv.ParseInt(t.String(), 10, 64)
		return n, err == nil
	case float64:
		if t == float64(int64(t)) {
			return int64(t), true
		}
	case int64:
		return t, true
	case int:
		return int64(t), true
	}
	return 0, false
}

// formatMinor writes minor units as major units with the currency the API named.
// It only moves the decimal point; every figure itself comes from the API.
func formatMinor(v any, currency string) string {
	n, ok := minorInt(v)
	if !ok {
		if v == nil {
			return ""
		}
		return scalarText(v)
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency == "" {
		return strconv.FormatInt(n, 10) + " (minor units, currency not stated)"
	}
	exp := 2
	if e, known := minorExponent[currency]; known {
		exp = e
	}
	digits := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(digits, "-")
	digits = strings.TrimPrefix(digits, "-")
	if exp > 0 {
		for len(digits) <= exp {
			digits = "0" + digits
		}
	}
	whole, frac := digits, ""
	if exp > 0 {
		whole, frac = digits[:len(digits)-exp], digits[len(digits)-exp:]
	}
	out := groupThousands(whole)
	if frac != "" {
		out += "." + frac
	}
	if neg {
		out = "-" + out
	}
	return currency + " " + out
}

func groupThousands(s string) string {
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func scalarText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(encoded)
}
