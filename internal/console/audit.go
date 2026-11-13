package console

import (
	"net/http"

	"github.com/exitcodenihil/llm-router/internal/audit"
)

// audit records a sign-in event. A failure carries no actor: the email that
// was tried is personal data, and usually a typo of someone's real address.
func (a *Auth) audit(r *http.Request, action string, status int, userID, email string) {
	if a.Audit == nil {
		return
	}
	e := audit.Event{ActorKind: audit.Anonymous, Action: action, Status: status,
		ClientIP: ClientIP(r, a.TrustedProxies)}
	if userID != "" {
		e.ActorKind, e.ActorUserID, e.ActorEmail = audit.User, userID, email
		e.TargetType, e.TargetID = "user", userID
	}
	a.Audit(r.Context(), e)
}

// statusWriter remembers the status a handler answered with.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}
