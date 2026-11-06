package upstream

import (
	"bytes"
	"encoding/json"
	"math/big"
	"strings"
)

// Num is a JSON value that upstreams send sometimes as a number and sometimes
// as a string (123, "123", "0x7b"). It keeps the raw text, so nothing is lost
// to float64 on the way in.
type Num string

func (n *Num) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		*n = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*n = Num(strings.TrimSpace(s))
		return nil
	}
	*n = Num(b)
	return nil
}

// Uint returns the value as a canonical base-10 unsigned integer string. ok is
// false for anything that is not a plain decimal or 0x-hex integer.
func (n Num) Uint() (string, bool) {
	v, ok := n.big()
	if !ok {
		return "", false
	}
	return v.String(), true
}

func (n Num) big() (*big.Int, bool) {
	s, base := string(n), 10
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		s, base = s[2:], 16
	}
	if s == "" || len(s) > 80 {
		return nil, false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isDigit := c >= '0' && c <= '9'
		isHex := base == 16 && ((c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'))
		if !isDigit && !isHex {
			return nil, false
		}
	}
	return new(big.Int).SetString(s, base)
}
