package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// SSE recognizes LF, CRLF and CR. The emitted framing is normalized to LF.
func splitSSELine(data []byte, atEOF bool) (int, []byte, error) {
	for i, b := range data {
		if b == '\n' {
			return i + 1, data[:i], nil
		}
		if b == '\r' {
			if i+1 == len(data) && !atEOF {
				return 0, nil, nil
			}
			n := i + 1
			if n < len(data) && data[n] == '\n' {
				n++
			}
			return n, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func relaySSE(reader io.Reader, output *proxyOutput, trace *requestTrace, args ...*responseFacts) error {
	facts := factsArg(args)
	scanner := bufio.NewScanner(reader)
	scanner.Split(splitSSELine)
	scanner.Buffer(make([]byte, 4096), maxEventBytes+1)
	var raw, pending []byte
	var data []string
	var event, id, model string
	seen, finished, usageSeen, firstLine := false, false, false, true
	for scanner.Scan() {
		line := scanner.Text()
		if firstLine {
			line = strings.TrimPrefix(line, "\ufeff")
			firstLine = false
		}
		if !utf8.ValidString(line) {
			return errProtocol
		}
		if len(raw)+len(line)+1 > maxEventBytes {
			return errLimit
		}
		raw = append(raw, []byte(line)...)
		raw = append(raw, '\n')
		if line != "" {
			if strings.HasPrefix(line, ":") {
				continue
			}
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "data":
				data = append(data, value)
			case "event":
				event = value
			}
			continue
		}
		if event != "" && event != "message" {
			return errProtocol
		} // Upstream error events are sanitized, never echoed.
		if len(data) == 0 {
			if finished {
				pending = append(pending, raw...)
				if len(pending) > 2*maxEventBytes {
					return errLimit
				}
			} else if _, err := output.Write(raw); err != nil {
				return err
			}
			raw = nil
			event = ""
			continue
		}
		payload := strings.TrimSpace(strings.Join(data, "\n"))
		if payload == "[DONE]" {
			if !seen || !finished {
				return errProtocol
			}
			facts.complete = true
			if _, err := output.Write(pending); err != nil {
				return err
			}
			_, err := output.Write(raw)
			return err
		}
		d := json.NewDecoder(strings.NewReader(payload))
		d.UseNumber()
		var hasCreated, hasChoices, hasIndex, hasDelta bool
		var usage usageCollector
		if err := walkJSON(d, nil, 0, func(path []string, v any) error {
			usage.observe(path, v)
			if len(path) == 0 && v != json.Delim('{') {
				return errProtocol
			}
			if len(path) == 1 && path[0] == "error" {
				return errProtocol
			}
			if len(path) == 1 && path[0] == "created" {
				n, ok := v.(json.Number)
				if !ok {
					return errProtocol
				}
				i, err := n.Int64()
				hasCreated = err == nil && i >= 0
			}
			if len(path) == 1 && path[0] == "choices" {
				hasChoices = v == json.Delim('[')
				if !hasChoices {
					return errProtocol
				}
			}
			if len(path) == 3 && path[0] == "choices" && path[2] == "index" {
				n, ok := v.(json.Number)
				hasIndex = ok && string(n) == "0"
			}
			if len(path) == 3 && path[0] == "choices" && path[2] == "delta" {
				hasDelta = v == json.Delim('{')
				if !hasDelta {
					return errProtocol
				}
			}
			if len(path) == 4 && path[0] == "choices" && path[2] == "delta" && (path[3] == "tool_calls" || path[3] == "function_call") {
				return errProtocol
			}
			return nil
		}); err != nil {
			return errProtocol
		}
		if err := jsonEOF(d); err != nil {
			return errProtocol
		}
		var chunk Chunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return errProtocol
		}
		if chunk.ID == "" || chunk.Object != "chat.completion.chunk" || chunk.Model == "" || !hasCreated || !hasChoices {
			return errProtocol
		}
		// llama.cpp stamps individual events; created may change during one reply.
		// Preserve those timestamps, but require a stable completion ID and model.
		if seen && (chunk.ID != id || chunk.Model != model) {
			return errProtocol
		}
		if !seen {
			id, model = chunk.ID, chunk.Model
			seen = true
		}
		if len(chunk.Choices) == 0 {
			if !finished || usageSeen || chunk.Usage == nil || chunk.Usage.PromptTokens < 0 || chunk.Usage.CompletionTokens < 0 || chunk.Usage.TotalTokens < 0 {
				return errProtocol
			}
			usageSeen = true
			facts.usage = usage
		} else {
			if finished || len(chunk.Choices) != 1 || !hasIndex || !hasDelta {
				return errProtocol
			}
			choice := chunk.Choices[0]
			if choice.Delta.Role != "" && choice.Delta.Role != "assistant" {
				return errProtocol
			}
			if choice.Delta.Content != nil && *choice.Delta.Content != "" {
				trace.text()
			}
			if choice.FinishReason != nil {
				if *choice.FinishReason == "" {
					return errProtocol
				}
				finished = true
			}
		}
		facts.observed = true
		if usageSeen && !facts.includeUsage && len(chunk.Choices) == 0 {
			raw = nil
			data = nil
			event = ""
			continue
		}
		if finished {
			pending = append(pending, raw...)
			if len(pending) > 2*maxEventBytes {
				return errLimit
			}
		} else if _, err := output.Write(raw); err != nil {
			return err
		}
		raw = nil
		data = nil
		event = ""
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return errLimit
		}
		return err
	}
	// EOF, even after a finish_reason, is not an SSE completion marker. Pending
	// finish/usage frames are discarded so an interrupted stream cannot look done.
	return io.ErrUnexpectedEOF
}
