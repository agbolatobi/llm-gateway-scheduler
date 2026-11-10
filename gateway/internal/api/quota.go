package api

import (
	"net/http"
	"strconv"
	"time"

	"llm-gateway-scheduler/gateway/internal/quota"
)

type QuotaChecker interface {
	CheckAndCount(userID string) quota.Decision
}

func checkQuota(w http.ResponseWriter, r *http.Request, checker QuotaChecker) bool {
	principal, ok := PrincipalFromContext(r.Context())
	if !ok || principal.UserID == "" || checker == nil {
		writeError(w, http.StatusServiceUnavailable, "quota_unavailable", "Quota checks are unavailable.", "")
		return false
	}
	d := checker.CheckAndCount(principal.UserID)
	return writeQuotaDecision(w, d)
}

func writeQuotaDecision(w http.ResponseWriter, d quota.Decision) bool {
	if d.Allowed {
		return true
	}
	messages := map[string]string{
		"user_request_limit":   "Your request allowance is exhausted. Retry later.",
		"global_request_limit": "The gateway request allowance is exhausted. Retry later.",
		"user_token_limit":     "Your token allowance is exhausted. Retry later.",
		"global_token_limit":   "The gateway token allowance is exhausted. Retry later.",
		"model_request_limit":  "The model request allowance is exhausted. Retry later.",
		"model_token_limit":    "The model token allowance is exhausted. Retry later.",
	}
	message, known := messages[d.Code]
	if !known {
		writeError(w, http.StatusServiceUnavailable, "quota_unavailable", "Quota checks are unavailable.", "")
		return false
	}
	if d.RetryAfter > 0 {
		seconds := d.RetryAfter / time.Second
		if d.RetryAfter%time.Second != 0 {
			seconds++
		}
		w.Header().Set("Retry-After", strconv.FormatInt(int64(seconds), 10))
	}
	writeJSON(w, http.StatusTooManyRequests, ErrorResponse{Error: APIError{
		Message: message, Type: "rate_limit_error", Code: d.Code,
	}})
	return false
}
