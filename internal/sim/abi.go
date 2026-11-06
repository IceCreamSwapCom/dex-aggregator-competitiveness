package sim

import (
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
)

// The little ABI encoding the simulation needs, written out so that no
// Ethereum library has to be trusted or audited.

// word is a 32-byte ABI word.
func word(v *big.Int) []byte { return v.FillBytes(make([]byte, 32)) }

func addressWord(addr string) []byte {
	b, _ := hex.DecodeString(strings.TrimPrefix(strings.ToLower(addr), "0x"))
	return append(make([]byte, 32-len(b)), b...)
}

// bytesTail ABI-encodes a dynamic bytes value: length, then the data padded to 32 bytes.
func bytesTail(data []byte) []byte {
	out := word(big.NewInt(int64(len(data))))
	out = append(out, data...)
	return append(out, make([]byte, (32-len(data)%32)%32)...)
}

// words splits ABI return data into 32-byte words.
func words(ret []byte, n int) ([]*big.Int, error) {
	if len(ret) < n*32 {
		return nil, errors.New("return data too short")
	}
	out := make([]*big.Int, n)
	for i := range out {
		out[i] = new(big.Int).SetBytes(ret[i*32 : (i+1)*32])
	}
	return out, nil
}

// dynamicBytes reads the bytes value whose head offset is at word index i.
func dynamicBytes(ret []byte, i int) ([]byte, error) {
	w, err := words(ret, i+1)
	if err != nil {
		return nil, err
	}
	off := w[i]
	if !off.IsInt64() || off.Int64()+32 > int64(len(ret)) {
		return nil, errors.New("bad bytes offset")
	}
	start := int(off.Int64()) + 32
	n := new(big.Int).SetBytes(ret[start-32 : start])
	if !n.IsInt64() || int64(start)+n.Int64() > int64(len(ret)) {
		return nil, errors.New("bad bytes length")
	}
	return ret[start : start+int(n.Int64())], nil
}

func hexBytes(s string) ([]byte, error) { return hex.DecodeString(strings.TrimPrefix(s, "0x")) }

func hexBig(v *big.Int) string { return "0x" + v.Text(16) }

func parseHexBig(s string) (*big.Int, bool) {
	if !strings.HasPrefix(s, "0x") {
		return nil, false
	}
	if s == "0x" {
		return new(big.Int), true
	}
	return new(big.Int).SetString(s[2:], 16)
}
