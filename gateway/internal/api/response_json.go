package api

import (
	"encoding/json"
	"io"
	"strconv"
)

// Keep a bounded tail until validation reaches EOF. Large replies are forwarded
// incrementally; malformed/truncated replies never receive a clean HTTP finish.
type tailWriter struct {
	output io.Writer
	tail   []byte
}

func (writer *tailWriter) Write(buffer []byte) (int, error) {
	const keep = 8192
	bytesAccepted := len(buffer)
	if len(buffer) >= keep {
		if _, err := writer.output.Write(writer.tail); err != nil {
			return 0, err
		}
		if _, err := writer.output.Write(buffer[:len(buffer)-keep]); err != nil {
			return 0, err
		}
		writer.tail = append(writer.tail[:0], buffer[len(buffer)-keep:]...)
	} else {
		overflow := max(0, len(writer.tail)+len(buffer)-keep)
		if overflow > 0 {
			if _, err := writer.output.Write(writer.tail[:overflow]); err != nil {
				return 0, err
			}
			copy(writer.tail, writer.tail[overflow:])
			writer.tail = writer.tail[:len(writer.tail)-overflow]
		}
		writer.tail = append(writer.tail, buffer...)
	}
	return bytesAccepted, nil
}

// walkJSON validates one document without assembling the entire response. The
// largest single string token is still bounded by the response/event byte cap.
func walkJSON(decoder *json.Decoder, path []string, depth int, observe func([]string, any) error) error {
	if depth > 32 {
		return errLimit
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if err := observe(path, token); err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errProtocol
			}
			seen[name] = true
			if len(seen) > 256 {
				return errLimit
			}
			if err := walkJSON(decoder, append(path[:len(path):len(path)], name), depth+1, observe); err != nil {
				return err
			}
		}
	case '[':
		for elementIndex := 0; decoder.More(); elementIndex++ {
			if elementIndex >= 4096 {
				return errLimit
			}
			if err := walkJSON(decoder, append(path[:len(path):len(path)], strconv.Itoa(elementIndex)), depth+1, observe); err != nil {
				return err
			}
		}
	default:
		return errProtocol
	}
	end, err := decoder.Token()
	if err != nil {
		return err
	}
	if (delim == '{' && end != json.Delim('}')) || (delim == '[' && end != json.Delim(']')) {
		return errProtocol
	}
	return nil
}

func jsonEOF(decoder *json.Decoder) error {
	if _, err := decoder.Token(); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return errProtocol
}

func relayJSON(reader io.Reader, output *proxyOutput, trace *requestTrace, args ...*responseFacts) error {
	facts := factsArg(args)
	var usage usageCollector
	tail := &tailWriter{output: output}
	decoder := json.NewDecoder(io.TeeReader(reader, tail))
	decoder.UseNumber()
	var id, object, model, role, finish string
	var content, index, created, choice bool
	err := walkJSON(decoder, nil, 0, func(path []string, value any) error {
		usage.observe(path, value)
		if len(path) == 0 {
			if value != json.Delim('{') {
				return errProtocol
			}
			return nil
		}
		if len(path) == 1 {
			textValue, _ := value.(string)
			switch path[0] {
			case "error":
				return errProtocol
			case "id":
				id = textValue
			case "object":
				object = textValue
			case "model":
				model = textValue
			case "created":
				number, ok := value.(json.Number)
				if ok {
					integerValue, parseError := number.Int64()
					created = parseError == nil && integerValue >= 0
				}
			case "choices":
				if value != json.Delim('[') {
					return errProtocol
				}
			}
			return nil
		}
		if path[0] != "choices" {
			return nil
		}
		if path[1] != "0" {
			return errProtocol
		} // API currently supports one choice only.
		if len(path) == 2 {
			choice = value == json.Delim('{')
			if !choice {
				return errProtocol
			}
		}
		if len(path) == 3 {
			switch path[2] {
			case "index":
				number, ok := value.(json.Number)
				index = ok && string(number) == "0"
			case "finish_reason":
				finish, _ = value.(string)
			case "message":
				if value != json.Delim('{') {
					return errProtocol
				}
			}
		}
		if len(path) == 4 && path[2] == "message" {
			switch path[3] {
			case "role":
				role, _ = value.(string)
			case "content":
				textValue, ok := value.(string)
				content = ok
				if ok && textValue != "" {
					trace.markFirstText()
				}
			case "tool_calls", "function_call":
				return errProtocol
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := jsonEOF(decoder); err != nil {
		return err
	}
	if id == "" || object != "chat.completion" || model == "" || !created || !choice || !index || role != "assistant" || !content || finish == "" {
		return errProtocol
	}
	facts.observed = true
	facts.complete = true
	facts.usage = usage
	_, err = output.Write(tail.tail)
	return err
}
