package api

import "encoding/json"

// Collect only the three accounting fields; never retain response text.
type usageCollector struct {
	present, invalid bool
	fields           map[string]int64
}

func (usage *usageCollector) observe(path []string, value any) {
	if len(path) == 0 || path[0] != "usage" {
		return
	}
	if len(path) == 1 {
		usage.present = true
		if value != json.Delim('{') {
			usage.invalid = true
		}
		return
	}
	if len(path) != 2 {
		return
	}
	switch path[1] {
	case "prompt_tokens", "completion_tokens", "total_tokens":
		number, ok := value.(json.Number)
		if !ok {
			usage.invalid = true
			return
		}
		tokenCount, err := number.Int64()
		if err != nil || tokenCount < 0 {
			usage.invalid = true
			return
		}
		if usage.fields == nil {
			usage.fields = make(map[string]int64)
		}
		usage.fields[path[1]] = tokenCount
	}
}
func (usage usageCollector) validated(input, outputTokenLimit int64) *Usage {
	if !usage.present || usage.invalid || len(usage.fields) != 3 {
		return nil
	}
	promptTokens, completionTokens, total := usage.fields["prompt_tokens"], usage.fields["completion_tokens"], usage.fields["total_tokens"]
	// Require agreement with chat-template tokenization, not a cached-prefill count.
	if promptTokens != input || completionTokens > outputTokenLimit || total != promptTokens+completionTokens {
		return nil
	}
	return &Usage{PromptTokens: int(promptTokens), CompletionTokens: int(completionTokens), TotalTokens: int(total)}
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
