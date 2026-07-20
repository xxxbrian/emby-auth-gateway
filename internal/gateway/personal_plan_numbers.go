package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"strconv"
)

func decodeJSONUseNumber(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("JSON contains multiple values")
		}
		return err
	}
	return nil
}

func boundedNonNegativeJSONInt(value any) (int, bool) {
	number, ok := exactJSONNumber(value)
	if !ok || number.Sign() < 0 || !number.IsInt() {
		return 0, false
	}
	integer := number.Num()
	maxInt := new(big.Int).SetUint64(uint64(^uint(0) >> 1))
	if integer.Cmp(maxInt) > 0 {
		return 0, false
	}
	return int(integer.Int64()), true
}

func exactJSONNumber(value any) (*big.Rat, bool) {
	switch value := value.(type) {
	case json.Number:
		return exactJSONNumberText(value.String())
	case int:
		return new(big.Rat).SetInt64(int64(value)), true
	case int64:
		return new(big.Rat).SetInt64(value), true
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, false
		}
		return new(big.Rat).SetFloat64(value), true
	case float32:
		value64 := float64(value)
		if math.IsNaN(value64) || math.IsInf(value64, 0) {
			return nil, false
		}
		return new(big.Rat).SetFloat64(value64), true
	default:
		return nil, false
	}
}

func exactJSONNumberText(raw string) (*big.Rat, bool) {
	if !json.Valid([]byte(raw)) {
		return nil, false
	}
	var decoded any
	if err := decodeJSONUseNumber([]byte(raw), &decoded); err != nil {
		return nil, false
	}
	if _, ok := decoded.(json.Number); !ok {
		return nil, false
	}
	number, ok := new(big.Rat).SetString(raw)
	return number, ok
}

func exactPersonalComparableNumber(value any) (*big.Rat, string, bool) {
	number, ok := exactJSONNumber(value)
	if !ok {
		return nil, "", false
	}
	switch value := value.(type) {
	case json.Number:
		return number, value.String(), true
	case int:
		return number, strconv.Itoa(value), true
	case int64:
		return number, strconv.FormatInt(value, 10), true
	case float64:
		return number, strconv.FormatFloat(value, 'g', -1, 64), true
	case float32:
		return number, strconv.FormatFloat(float64(value), 'g', -1, 32), true
	default:
		return nil, "", false
	}
}
