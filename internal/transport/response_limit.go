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

func readResponseBodyForRequest(body io.Reader, req Request) ([]byte, error) {
	if !req.CaptureResponse || req.MaxResponseBytes <= 0 || req.MaxResponseBytes == math.MaxInt64 {
		return readResponseBody(body, req.MaxResponseBytes)
	}
	data, err := io.ReadAll(io.LimitReader(body, req.MaxResponseBytes+1))
	if int64(len(data)) > req.MaxResponseBytes {
		return data[:req.MaxResponseBytes], ErrResponseTooLarge
	}
	return data, err
}

func classifyBodyFailure(err, canceled error) error {
	if err == nil && canceled == nil {
		return nil
	}
	if errors.Is(err, ErrResponseTooLarge) {
		return err
	}
	return classifyRequestError(err, canceled)
}
