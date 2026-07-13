package main

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
)

const (
	// activityLogTokenRedactionMiddlewareId is the stable router middleware id.
	// Re-binding with the same id replaces any previous registration.
	activityLogTokenRedactionMiddlewareId = "gatewayActivityLogTokenRedaction"

	// Run inside the default activity logger (after its Next starts) so handlers
	// still observe original credentials, while logRequest sees redacted values.
	activityLogTokenRedactionMiddlewarePriority = apis.DefaultActivityLoggerMiddlewarePriority + 1

	activityLogRedactedValue = "REDACTED"
)

// sensitiveActivityLogQueryKeys are matched case-insensitively against query keys.
var sensitiveActivityLogQueryKeys = map[string]struct{}{
	"api_key":              {},
	"access_token":         {},
	"token":                {},
	"x-emby-token":         {},
	"x-mediabrowser-token": {},
}

func registerActivityLogTokenRedaction(app core.App) {
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: "gatewayRegisterActivityLogTokenRedaction",
		Func: func(e *core.ServeEvent) error {
			e.Router.Bind(activityLogTokenRedactionMiddleware())
			return e.Next()
		},
	})
}

func activityLogTokenRedactionMiddleware() *hook.Handler[*core.RequestEvent] {
	return &hook.Handler[*core.RequestEvent]{
		Id:       activityLogTokenRedactionMiddlewareId,
		Priority: activityLogTokenRedactionMiddlewarePriority,
		Func: func(e *core.RequestEvent) error {
			// Handlers must see the original request; redact only for the
			// outer activity logger that runs after this middleware returns.
			// Derive the selected credential before mutating URL/Referer.
			selected := ""
			if e.Request != nil {
				selected = gateway.ExtractToken(e.Request)
			}
			err := e.Next()
			redactRequestForActivityLog(e.Request, selected)
			return err
		},
	}
}

func redactRequestForActivityLog(r *http.Request, selectedToken string) {
	if r == nil {
		return
	}
	if r.URL != nil {
		redactURLForActivityLog(r.URL, selectedToken)
	}
	if referer := r.Referer(); referer != "" {
		r.Header.Set("Referer", redactRefererForActivityLog(referer, selectedToken))
	}
}

// redactURLForActivityLog mutates u in place: strips userinfo and redacts
// sensitive query values. Malformed queries are replaced wholesale.
func redactURLForActivityLog(u *url.URL, selectedToken string) {
	if u == nil {
		return
	}
	if u.User != nil {
		u.User = nil
	}
	u.RawQuery = redactRawQueryForActivityLog(u.RawQuery, selectedToken)
}

func redactRefererForActivityLog(referer, selectedToken string) string {
	referer = strings.TrimSpace(referer)
	if referer == "" {
		return ""
	}
	u, err := url.Parse(referer)
	if err != nil {
		return activityLogRedactedValue
	}
	redactURLForActivityLog(u, selectedToken)
	return u.String()
}

func redactRawQueryForActivityLog(rawQuery, selectedToken string) string {
	if rawQuery == "" {
		return ""
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		// Conservatively hide the entire query rather than risk leaking secrets.
		return activityLogRedactedValue
	}
	for key, vals := range values {
		sensitiveKey := isSensitiveActivityLogQueryKey(key)
		for i, val := range vals {
			if sensitiveKey {
				vals[i] = activityLogRedactedValue
				continue
			}
			// Redact the selected credential under arbitrary keys (e.g. signature=).
			// Do not broadly redact unrelated 43-character signatures.
			if selectedToken != "" && val == selectedToken {
				vals[i] = activityLogRedactedValue
			}
		}
		values[key] = vals
	}
	return values.Encode()
}

func isSensitiveActivityLogQueryKey(key string) bool {
	_, ok := sensitiveActivityLogQueryKeys[strings.ToLower(strings.TrimSpace(key))]
	return ok
}
