package gateway

import (
	"net/http"
	"strings"
)

// Registration paths are host-root (not under /emby). After Emby Web
// host injection, clients call these instead of mb3admin.com.
const registrationPrefix = "/admin/service/registration/"

// RegistrationHandler serves fixed Emby Premiere / mb3admin-compatible JSON stubs.
// Mount at host root under /admin/service/registration/{name}.
type RegistrationHandler struct{}

func (RegistrationHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r == nil {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodPost, http.MethodHead:
		// allowed
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name, ok := registrationRouteName(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	switch name {
	case "validateDevice":
		writeJSON(w, http.StatusOK, registrationValidateDevice{CacheExpirationDays: 233, Message: "Device Valid", ResultCode: "GOOD"})
	case "validate":
		writeJSON(w, http.StatusOK, registrationValidate{Registered: true, Expires: "2333-10-01"})
	case "getStatus":
		writeJSON(w, http.StatusOK, registrationStatus{PlanType: "Lifetime"})
	default:
		http.NotFound(w, r)
	}
}

func registrationRouteName(requestPath string) (string, bool) {
	if !strings.HasPrefix(requestPath, registrationPrefix) {
		return "", false
	}
	name := strings.Trim(strings.TrimPrefix(requestPath, registrationPrefix), "/")
	if name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}
