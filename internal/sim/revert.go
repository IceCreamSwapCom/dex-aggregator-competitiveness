package sim

import (
	"encoding/hex"
	"fmt"
	"math/big"
)

// decodeRevert turns revert data into something a person can read:
// Error(string) as its message, Panic(uint256) as "panic 0x..", anything else
// (custom errors) as its hex, which explorers and 4byte directories decode.
func decodeRevert(data []byte) string {
	if len(data) == 0 {
		return "reverted without data"
	}
	if len(data) >= 4 {
		switch hex.EncodeToString(data[:4]) {
		case "08c379a0": // Error(string)
			if msg, err := dynamicBytes(data[4:], 0); err == nil {
				return string(msg)
			}
		case "4e487b71": // Panic(uint256)
			if len(data) >= 36 {
				return fmt.Sprintf("panic 0x%x", new(big.Int).SetBytes(data[4:36]))
			}
		}
	}
	if len(data) > 260 {
		data = data[:260]
	}
	return "0x" + hex.EncodeToString(data)
}
