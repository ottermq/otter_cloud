package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/ottermq/otter_cloud/internal/identity"
)

// sessionCookie is the session cookie's name. The __Host- prefix makes the
// browser refuse it unless it is Secure, has Path=/ and no Domain, so a
// sibling subdomain can't set or overwrite it.
const sessionCookie = "__Host-otter_session"

// setSessionCookie sends the session token. Secure is set even for
// http://localhost in development: browsers treat localhost as a secure
// context and accept it. Max-Age is the absolute lifetime; the server
// enforces the idle timeout.
func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(identity.SessionAbsoluteTimeout.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func sessionToken(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

type sessionKey struct{}

// requireSession answers 401 unless the request carries a live session,
// and puts the session in the request's context for next.
func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, err := identity.Authenticate(r.Context(), s.db, sessionToken(r))
		if errors.Is(err, identity.ErrNoSession) {
			writeError(w, http.StatusUnauthorized, "not logged in")
			return
		}
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, sess)))
	})
}

func sessionFrom(ctx context.Context) identity.Session {
	sess, _ := ctx.Value(sessionKey{}).(identity.Session)
	return sess
}

// me is the logged-in user, as /api/me, login and sign-up return it.
type me struct {
	ID         string `json:"id"`
	AccountID  string `json:"account_id"`
	Email      string `json:"email"`
	IsOperator bool   `json:"is_operator"`
}

func meFrom(u identity.User) me {
	return me{ID: u.ID, AccountID: u.AccountID, Email: u.Email, IsOperator: u.IsOperator}
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, meFrom(sessionFrom(r.Context()).User))
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if decode(w, r, &req) != nil {
		return
	}
	u, err := identity.Login(r.Context(), s.db, req.Email, req.Password)
	if errors.Is(err, identity.ErrLoginFailed) {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.startSession(w, r, u, http.StatusOK)
}

// startSession rotates the session: whatever session the request carried
// is revoked and a new one issued, so a token planted before login (session
// fixation) is worthless after it.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u identity.User, status int) {
	if err := identity.RevokeSession(r.Context(), s.db, sessionToken(r)); err != nil {
		s.internalError(w, r, err)
		return
	}
	token, err := identity.CreateSession(r.Context(), s.db, u.ID, identity.Client{
		IP:        clientIP(r),
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	setSessionCookie(w, token)
	writeJSON(w, status, meFrom(u))
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := identity.RevokeSession(r.Context(), s.db, sessionToken(r)); err != nil {
		s.internalError(w, r, err)
		return
	}
	clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

type signUpRequest struct {
	Token       string `json:"token"`
	Password    string `json:"password"`
	AccountName string `json:"account_name"`
}

// handleSignUp consumes an invite (the token from the link's fragment,
// which the sign-up page posts here) and logs the new user in.
func (s *Server) handleSignUp(w http.ResponseWriter, r *http.Request) {
	var req signUpRequest
	if decode(w, r, &req) != nil {
		return
	}
	u, err := identity.SignUpWithInvite(r.Context(), s.db, identity.SignUp{
		Token:       req.Token,
		Password:    req.Password,
		AccountName: req.AccountName,
	})
	switch {
	case errors.Is(err, identity.ErrInviteInvalid), errors.Is(err, identity.ErrInviteExpired),
		errors.Is(err, identity.ErrPasswordTooShort), errors.Is(err, identity.ErrPasswordTooLong),
		errors.Is(err, identity.ErrPasswordCommon), errors.Is(err, identity.ErrPasswordIsEmail):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, identity.ErrEmailTaken):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	s.startSession(w, r, u, http.StatusCreated)
}
