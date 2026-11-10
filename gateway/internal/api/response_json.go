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

func (w *tailWriter) Write(p []byte) (int, error) {
	const keep = 8192
	n := len(p)
	if len(p) >= keep {
		if _, err := w.output.Write(w.tail); err != nil {
			return 0, err
		}
		if _, err := w.output.Write(p[:len(p)-keep]); err != nil {
			return 0, err
		}
		w.tail = append(w.tail[:0], p[len(p)-keep:]...)
	} else {
		overflow := max(0, len(w.tail)+len(p)-keep)
		if overflow > 0 {
			if _, err := w.output.Write(w.tail[:overflow]); err != nil {
				return 0, err
			}
			copy(w.tail, w.tail[overflow:])
			w.tail = w.tail[:len(w.tail)-overflow]
		}
		w.tail = append(w.tail, p...)
	}
	return n, nil
}

// walkJSON validates one document without assembling the entire response. The
// largest single string token is still bounded by the response/event byte cap.
func walkJSON(d *json.Decoder, path []string, depth int, observe func([]string, any) error) error {
	if depth > 32 {
		return errLimit
	}
	token, err := d.Token()
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
		for d.More() {
			key, err := d.Token()
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
			if err := walkJSON(d, append(path[:len(path):len(path)], name), depth+1, observe); err != nil {
				return err
			}
		}
	case '[':
		for i := 0; d.More(); i++ {
			if i >= 4096 {
				return errLimit
			}
			if err := walkJSON(d, append(path[:len(path):len(path)], strconv.Itoa(i)), depth+1, observe); err != nil {
				return err
			}
		}
	default:
		return errProtocol
	}
	end, err := d.Token()
	if err != nil {
		return err
	}
	if (delim == '{' && end != json.Delim('}')) || (delim == '[' && end != json.Delim(']')) {
		return errProtocol
	}
	return nil
}

func jsonEOF(d *json.Decoder) error {
	if _, err := d.Token(); err == io.EOF {
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
	d := json.NewDecoder(io.TeeReader(reader, tail))
	d.UseNumber()
	var id, object, model, role, finish string
	var content, index, created, choice bool
	err := walkJSON(d, nil, 0, func(path []string, value any) error {
		usage.observe(path, value)
		if len(path) == 0 {
			if value != json.Delim('{') {
				return errProtocol
			}
			return nil
		}
		if len(path) == 1 {
			s, _ := value.(string)
			switch path[0] {
			case "error":
				return errProtocol
			case "id":
				id = s
			case "object":
				object = s
			case "model":
				model = s
			case "created":
				n, ok := value.(json.Number)
				if ok {
					v, e := n.Int64()
					created = e == nil && v >= 0
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
				n, ok := value.(json.Number)
				index = ok && string(n) == "0"
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
				s, ok := value.(string)
				content = ok
				if ok && s != "" {
					trace.text()
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
	if err := jsonEOF(d); err != nil {
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
