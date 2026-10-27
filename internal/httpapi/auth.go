package httpapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"

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
	// Limits are checked before the password, so a blocked guess costs no
	// argon2id work and learns nothing, even when it is right.
	ip := "ip:" + s.clientIP(r).String()
	account := ""
	if email, err := identity.NormalizeEmail(req.Email); err == nil {
		account = "account:" + email
	}
	if s.tooManyFailures(w, ip, account) {
		return
	}
	u, err := identity.Login(r.Context(), s.db, req.Email, req.Password)
	if errors.Is(err, identity.ErrLoginFailed) {
		s.ipLimit.fail(ip)
		if account != "" {
			s.accountLimit.fail(account)
		}
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if account != "" {
		s.accountLimit.reset(account)
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
		IP:        s.clientIP(r),
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
	// Invite tokens are 256 bits, so guessing is hopeless anyway; bad tokens
	// still count against the address, as failed logins do.
	ip := "ip:" + s.clientIP(r).String()
	if s.tooManyFailures(w, ip, "") {
		return
	}
	u, err := identity.SignUpWithInvite(r.Context(), s.db, identity.SignUp{
		Token:       req.Token,
		Password:    req.Password,
		AccountName: req.AccountName,
	})
	switch {
	case errors.Is(err, identity.ErrInviteInvalid), errors.Is(err, identity.ErrInviteExpired):
		s.ipLimit.fail(ip)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, identity.ErrPasswordTooShort), errors.Is(err, identity.ErrPasswordTooLong),
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

// tooManyFailures answers 429 if the client's address or the account (if
// not empty) has used up its failures.
func (s *Server) tooManyFailures(w http.ResponseWriter, ip, account string) bool {
	wait, blocked := s.ipLimit.blocked(ip)
	if !blocked && account != "" {
		wait, blocked = s.accountLimit.blocked(account)
	}
	if !blocked {
		return false
	}
	minutes := int(math.Ceil(wait.Minutes()))
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
	writeError(w, http.StatusTooManyRequests,
		fmt.Sprintf("too many failed attempts; try again in %d minute%s", minutes, plural(minutes)))
	return true
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
