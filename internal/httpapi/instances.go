package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/audit"
	"github.com/ottermq/otter_cloud/internal/billing"
	"github.com/ottermq/otter_cloud/internal/catalog"
	"github.com/ottermq/otter_cloud/internal/instances"
)

// maxIdempotencyKey bounds the Idempotency-Key header, as the column does.
const maxIdempotencyKey = 255

func (s *Server) handleInstances(w http.ResponseWriter, r *http.Request) {
	list, err := instances.ListInstances(r.Context(), s.db, accountOf(r))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if list == nil {
		list = []instances.InstanceView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": list})
}

func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	v, err := instances.InstanceByID(r.Context(), s.db, accountOf(r), r.PathValue("id"))
	if errors.Is(err, instances.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such database")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

type createInstanceRequest struct {
	Name   string `json:"name"`
	Flavor string `json:"flavor"`
}

// handleCreateInstance accepts a create: 202, because a worker does the
// work later and the portal follows the operation. The response carries the
// generated password, the only time it is shown: the platform keeps its
// SCRAM verifier, and every response is no-store. A request repeated with
// the same Idempotency-Key gets the same instance and operation, and
// "password_shown": false instead (#54).
func (s *Server) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	var req createInstanceRequest
	if decode(w, r, &req) != nil {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > maxIdempotencyKey {
		writeError(w, http.StatusBadRequest, "Idempotency-Key is too long")
		return
	}
	sess := sessionFrom(r.Context())
	ctx := r.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := instances.Create(ctx, tx, instances.CreateRequest{AccountID: sess.User.AccountID, Name: req.Name,
		FlavorCode: req.Flavor, RequestedBy: audit.User(sess.User.ID), IdempotencyKey: key})
	if err != nil {
		s.instanceError(w, r, err)
		return
	}
	if !c.Replayed {
		if err := audit.Record(ctx, tx, audit.Event{Actor: audit.User(sess.User.ID), AccountID: sess.User.AccountID,
			Action: audit.ActionInstanceCreate, TargetType: "instance", TargetID: c.Instance.ID,
			IP: s.clientIP(r), UserAgent: r.UserAgent(), Outcome: audit.OK,
			Details: map[string]any{"name": c.Instance.Name, "flavor": c.Instance.FlavorCode}}); err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	v, err := instances.InstanceByID(ctx, tx, sess.User.AccountID, c.Instance.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	op, err := instances.OperationByID(ctx, tx, sess.User.AccountID, c.Operation.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.internalError(w, r, err)
		return
	}
	body := map[string]any{"instance": v, "operation": op}
	if c.Replayed {
		body["password_shown"] = false
	} else {
		body["password"] = c.Password
	}
	writeJSON(w, http.StatusAccepted, body)
}

type deleteInstanceRequest struct {
	Confirm string `json:"confirm"`
}

// handleDeleteInstance queues a delete: 202, the worker does it later.
// The body repeats the database's name, so a stray request can't delete
// the wrong one.
func (s *Server) handleDeleteInstance(w http.ResponseWriter, r *http.Request) {
	var req deleteInstanceRequest
	if decode(w, r, &req) != nil {
		return
	}
	sess := sessionFrom(r.Context())
	ctx := r.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	v, op, err := instances.Delete(ctx, tx, sess.User.AccountID, r.PathValue("id"), req.Confirm, audit.User(sess.User.ID))
	if err != nil {
		s.instanceError(w, r, err)
		return
	}
	if err := audit.Record(ctx, tx, audit.Event{Actor: audit.User(sess.User.ID), AccountID: sess.User.AccountID,
		Action: audit.ActionInstanceDelete, TargetType: "instance", TargetID: v.ID,
		IP: s.clientIP(r), UserAgent: r.UserAgent(), Outcome: audit.OK,
		Details: map[string]any{"name": v.Name}}); err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"instance": v, "operation": op})
}

// lifecycle handles a stop, start or restart: 202 with the queued
// operation, which the worker runs later. No body: nothing to confirm, and
// each is undone by another.
func (s *Server) lifecycle(queue func(context.Context, pgx.Tx, string, string, string) (instances.InstanceView, instances.OperationView, error),
	action string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r.Context())
		ctx := r.Context()
		tx, err := s.db.Begin(ctx)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		v, op, err := queue(ctx, tx, sess.User.AccountID, r.PathValue("id"), audit.User(sess.User.ID))
		if err != nil {
			s.instanceError(w, r, err)
			return
		}
		if err := audit.Record(ctx, tx, audit.Event{Actor: audit.User(sess.User.ID), AccountID: sess.User.AccountID,
			Action: action, TargetType: "instance", TargetID: v.ID, IP: s.clientIP(r), UserAgent: r.UserAgent(),
			Outcome: audit.OK, Details: map[string]any{"name": v.Name}}); err != nil {
			s.internalError(w, r, err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			s.internalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"instance": v, "operation": op})
	})
}

// handleResetPassword queues a new password for the database: 202, with
// the password, shown this once, as create shows the first. A request
// repeated with the same Idempotency-Key gets the same operation and
// "password_shown": false (#101).
func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) > maxIdempotencyKey {
		writeError(w, http.StatusBadRequest, "Idempotency-Key is too long")
		return
	}
	sess := sessionFrom(r.Context())
	ctx := r.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	reset, err := instances.ResetPassword(ctx, tx, sess.User.AccountID, r.PathValue("id"), audit.User(sess.User.ID), key)
	if err != nil {
		s.instanceError(w, r, err)
		return
	}
	if !reset.Replayed {
		if err := audit.Record(ctx, tx, audit.Event{Actor: audit.User(sess.User.ID), AccountID: sess.User.AccountID,
			Action: audit.ActionInstancePassword, TargetType: "instance", TargetID: reset.Instance.ID,
			IP: s.clientIP(r), UserAgent: r.UserAgent(), Outcome: audit.OK,
			Details: map[string]any{"name": reset.Instance.Name}}); err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		s.internalError(w, r, err)
		return
	}
	body := map[string]any{"instance": reset.Instance, "operation": reset.Operation}
	if reset.Replayed {
		body["password_shown"] = false
	} else {
		body["password"] = reset.Password
	}
	writeJSON(w, http.StatusAccepted, body)
}

// instanceError answers the errors a create, delete, stop, start,
// restart or password reset can give a customer.
// Their messages are written to be shown; anything else is a 500.
func (s *Server) instanceError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		quota *instances.QuotaError
		state *instances.StateError
	)
	switch {
	case errors.As(err, &quota):
		writeJSON(w, http.StatusConflict, map[string]any{"error": quota.Error(), "limit": quota.Limit})
	case errors.As(err, &state):
		writeError(w, http.StatusConflict, state.Error())
	case errors.Is(err, instances.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such database")
	case errors.Is(err, instances.ErrNotEntitled), errors.Is(err, instances.ErrAccountNotActive):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, instances.ErrInstanceNameTaken), errors.Is(err, instances.ErrBusy),
		errors.Is(err, instances.ErrAlreadyDeleted):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, instances.ErrNoCapacity):
		w.Header().Set("Retry-After", "3600")
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, instances.ErrInvalidName), errors.Is(err, catalog.ErrFlavorNotFound),
		errors.Is(err, instances.ErrIdempotencyMismatch), errors.Is(err, instances.ErrConfirmMismatch):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		s.internalError(w, r, err)
	}
}

type subscriptionJSON struct {
	Plan          planJSON                       `json:"plan"`
	Status        string                         `json:"status"`
	PaymentStatus string                         `json:"payment_status"`
	StartsAt      time.Time                      `json:"starts_at"`
	EndsAt        *time.Time                     `json:"ends_at"`
	Limits        map[string]instances.LimitView `json:"limits"`
}

type planJSON struct {
	Name       string `json:"name"`
	PriceMinor int64  `json:"price_minor"`
	Currency   string `json:"currency"`
	Interval   string `json:"interval"`
}

// handleSubscription shows the account's subscription and, for each limit,
// what its databases hold against it. Both come from the server, so the
// portal never works out an entitlement itself. With no subscription,
// "subscription" is null and "entitled" false.
func (s *Server) handleSubscription(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	account := accountOf(r)
	ent, err := billing.AccountEntitlement(ctx, s.db, account)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	sub, ok, err := billing.OpenSubscription(ctx, s.db, account)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	body := map[string]any{"entitled": ent.Entitled, "subscription": nil}
	if ok {
		usage, err := instances.AccountUsage(ctx, s.db, account)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		j := subscriptionJSON{
			Plan:   planJSON{Name: sub.PlanName, PriceMinor: sub.PriceMinor, Currency: sub.Currency, Interval: sub.Interval},
			Status: sub.Status, PaymentStatus: sub.PaymentStatus, StartsAt: sub.StartsAt.UTC(),
			Limits: instances.Allowance(sub.Limits, usage),
		}
		if sub.EndsAt != nil {
			t := sub.EndsAt.UTC()
			j.EndsAt = &t
		}
		body["subscription"] = j
	}
	writeJSON(w, http.StatusOK, body)
}
