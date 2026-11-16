package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// These adapters enforce bounded reads, idle timeouts and client write deadlines.
// Keeping them separate makes the request orchestration easier to follow.

// idleReader cancels the HTTP request to unblock a stalled body Read. There is
// no background body-reading goroutine or unbounded producer/consumer queue.
type idleReader struct {
	reader  io.Reader
	timeout time.Duration
	cancel  context.CancelCauseFunc
}

func (bodyReader idleReader) Read(buffer []byte) (int, error) {
	timer := time.AfterFunc(bodyReader.timeout, func() { bodyReader.cancel(errIdle) })
	defer timer.Stop()
	return bodyReader.reader.Read(buffer)
}

type limitedReader struct {
	reader    io.Reader
	remaining int64
}

func (bodyReader *limitedReader) Read(buffer []byte) (int, error) {
	if bodyReader.remaining == 0 {
		var extra [1]byte
		bytesRead, err := bodyReader.reader.Read(extra[:])
		if bytesRead > 0 {
			return 0, errLimit
		}
		return 0, err
	}
	if int64(len(buffer)) > bodyReader.remaining {
		buffer = buffer[:bodyReader.remaining]
	}
	bytesRead, err := bodyReader.reader.Read(buffer)
	bodyReader.remaining -= int64(bytesRead)
	return bytesRead, err
}

type proxyOutput struct {
	responseWriter http.ResponseWriter
	ctx            context.Context
	timeout        time.Duration
	contentType    string
	started        bool
}

func (output *proxyOutput) Write(buffer []byte) (int, error) { return output.write(buffer, false) }
func (output *proxyOutput) write(buffer []byte, errorFrame bool) (int, error) {
	controller := http.NewResponseController(output.responseWriter)
	bytesWritten := 0
	for len(buffer) > 0 {
		if !errorFrame && output.ctx.Err() != nil {
			return bytesWritten, output.ctx.Err()
		}
		deadline := time.Now().Add(output.timeout)
		if errorFrame {
			deadline = time.Now().Add(min(output.timeout, time.Second))
		} else if limit, ok := output.ctx.Deadline(); ok && limit.Before(deadline) {
			deadline = limit
		}
		if err := controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return bytesWritten, errors.Join(errClientWrite, err)
		}
		if !output.started {
			output.responseWriter.Header().Set("Content-Type", output.contentType)
			output.responseWriter.Header().Set("Cache-Control", "no-store")
			output.responseWriter.Header().Set("X-Accel-Buffering", "no")
			output.started = true
		}
		size := min(len(buffer), 16<<10)
		written, err := output.responseWriter.Write(buffer[:size])
		bytesWritten += written
		if err != nil {
			return bytesWritten, errors.Join(errClientWrite, err)
		}
		if written != size {
			return bytesWritten, errors.Join(errClientWrite, io.ErrShortWrite)
		}
		buffer = buffer[size:]
		if err := controller.Flush(); err != nil {
			return bytesWritten, errors.Join(errClientWrite, err)
		}
	}
	return bytesWritten, nil
}
