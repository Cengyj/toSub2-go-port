package transport

import (
	"errors"
	"io"
	"math"
)

var ErrResponseTooLarge = errors.New("HTTP response exceeds the configured size limit")

func readResponseBody(body io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 || limit == math.MaxInt64 {
		return io.ReadAll(body)
	}
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if int64(len(data)) > limit {
		return nil, ErrResponseTooLarge
	}
	return data, err
}
