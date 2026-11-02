package httpapi

import (
	"errors"
	"net/http"
	"net/netip"
	"time"

	"github.com/ottermq/otter_cloud/internal/audit"
	"github.com/ottermq/otter_cloud/internal/identity"
)

// The account's Security page (portals.md §1, #78): the user's password
// and sessions. Each change is audited.

type passwordChangeRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleChangePassword changes the user's password. A wrong current
// password counts against the login limits, as a failed login does, so a
// stolen session can't be used to guess it. On success every session of
// the user's ends, and this request gets a new one: an old cookie is
// refused from then on.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req passwordChangeRequest
	if decode(w, r, &req) != nil {
		return
	}
	u := sessionFrom(r.Context()).User
	ip, account := "ip:"+s.clientIP(r).String(), "account:"+u.Email
	denied := func(reason string) {
		e := userEvent(u, audit.ActionPasswordChange)
		e.Outcome, e.Details = audit.Denied, map[string]any{"reason": reason}
		s.record(r, e)
	}
	if s.tooManyFailures(w, ip, account) {
		denied("rate_limited")
		return
	}
	err := identity.ChangePassword(r.Context(), s.db, u.ID, req.CurrentPassword, req.NewPassword)
	switch {
	case errors.Is(err, identity.ErrWrongPassword):
		s.ipLimit.fail(ip)
		s.accountLimit.fail(account)
		denied("bad_credentials")
		writeError(w, http.StatusForbidden, err.Error())
		return
	case errors.Is(err, identity.ErrPasswordTooShort), errors.Is(err, identity.ErrPasswordTooLong),
		errors.Is(err, identity.ErrPasswordCommon), errors.Is(err, identity.ErrPasswordIsEmail):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	s.accountLimit.reset(account)
	s.record(r, userEvent(u, audit.ActionPasswordChange))
	s.startSession(w, r, u, http.StatusOK)
}

type sessionJSON struct {
	ID         string      `json:"id"`
	CreatedAt  time.Time   `json:"created_at"`
	LastSeenAt time.Time   `json:"last_seen_at"`
	IP         *netip.Addr `json:"ip"`
	UserAgent  string      `json:"user_agent"`
	Current    bool        `json:"current"`
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r.Context())
	list, err := identity.ListSessions(r.Context(), s.db, sess.User.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]sessionJSON, 0, len(list))
	for _, l := range list {
		out = append(out, sessionJSON{ID: l.ID, CreatedAt: l.CreatedAt, LastSeenAt: l.LastSeenAt, IP: l.IP,
			UserAgent: l.UserAgent, Current: l.ID == sess.ID})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// handleEndSession ends one of the user's sessions. Ending the current
// one is a logout.
func (s *Server) handleEndSession(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r.Context())
	id := r.PathValue("id")
	err := identity.EndSession(r.Context(), s.db, sess.User.ID, id)
	if errors.Is(err, identity.ErrSessionNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	e := userEvent(sess.User, audit.ActionSessionRevoke)
	e.TargetType, e.TargetID = "session", id
	s.record(r, e)
	if id == sess.ID {
		clearSessionCookie(w)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleEndOtherSessions ends every session of the user's but this one.
func (s *Server) handleEndOtherSessions(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r.Context())
	n, err := identity.EndOtherSessions(r.Context(), s.db, sess.User.ID, sess.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if n > 0 {
		e := userEvent(sess.User, audit.ActionSessionRevoke)
		e.Details = map[string]any{"others": n}
		s.record(r, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ended": n})
}
