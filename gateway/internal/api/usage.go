package api

import "encoding/json"

// Collect only the three accounting fields; never retain response text.
type usageCollector struct {
	present, invalid bool
	fields           map[string]int64
}

func (u *usageCollector) observe(path []string, value any) {
	if len(path) == 0 || path[0] != "usage" {
		return
	}
	if len(path) == 1 {
		u.present = true
		if value != json.Delim('{') {
			u.invalid = true
		}
		return
	}
	if len(path) != 2 {
		return
	}
	switch path[1] {
	case "prompt_tokens", "completion_tokens", "total_tokens":
		n, ok := value.(json.Number)
		if !ok {
			u.invalid = true
			return
		}
		i, err := n.Int64()
		if err != nil || i < 0 {
			u.invalid = true
			return
		}
		if u.fields == nil {
			u.fields = make(map[string]int64)
		}
		u.fields[path[1]] = i
	}
}
func (u usageCollector) validated(input, cap int64) *Usage {
	if !u.present || u.invalid || len(u.fields) != 3 {
		return nil
	}
	p, c, total := u.fields["prompt_tokens"], u.fields["completion_tokens"], u.fields["total_tokens"]
	// Require agreement with chat-template tokenization, not a cached-prefill count.
	if p != input || c > cap || total != p+c {
		return nil
	}
	return &Usage{PromptTokens: int(p), CompletionTokens: int(c), TotalTokens: int(total)}
}

type responseFacts struct {
	observed, complete bool
	usage              usageCollector
	includeUsage       bool
}

func factsArg(args []*responseFacts) *responseFacts {
	if len(args) > 0 && args[0] != nil {
		return args[0]
	}
	return &responseFacts{includeUsage: true}
}
