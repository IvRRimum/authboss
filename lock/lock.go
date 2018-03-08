// Package lock implements user locking after N bad sign-in attempts.
package lock

import (
	"context"
	"net/http"
	"time"

	"github.com/pkg/errors"

	"github.com/volatiletech/authboss"
)

// Storage key constants
const (
	StoreAttemptNumber = "attempt_number"
	StoreAttemptTime   = "attempt_time"
	StoreLocked        = "locked"
)

var (
	errUserMissing = errors.New("user not loaded in BeforeAuth callback")
)

func init() {
	authboss.RegisterModule("lock", &Lock{})
}

// Lock module
type Lock struct {
	*authboss.Authboss
}

// Init the module
func (l *Lock) Init(ab *authboss.Authboss) error {
	l.Authboss = ab

	// Events
	l.Events.Before(authboss.EventAuth, l.BeforeAuth)
	l.Events.After(authboss.EventAuth, l.AfterAuthSuccess)
	l.Events.After(authboss.EventAuthFail, l.AfterAuthFail)

	return nil
}

// BeforeAuth ensures the account is not locked.
func (l *Lock) BeforeAuth(w http.ResponseWriter, r *http.Request, handled bool) (bool, error) {
	user, err := l.Authboss.CurrentUser(r)
	if err != nil {
		return false, err
	}

	lu := authboss.MustBeLockable(user)
	if !IsLocked(lu) {
		return false, nil
	}

	ro := authboss.RedirectOptions{
		Code:         http.StatusTemporaryRedirect,
		Failure:      "Your account is locked. Please contact the administrator.",
		RedirectPath: l.Authboss.Config.Paths.LockNotOK,
	}
	return true, l.Authboss.Config.Core.Redirector.Redirect(w, r, ro)
}

// AfterAuthSuccess resets the attempt number field.
func (l *Lock) AfterAuthSuccess(w http.ResponseWriter, r *http.Request, handled bool) (bool, error) {
	user, err := l.Authboss.CurrentUser(r)
	if err != nil {
		return false, err
	}

	lu := authboss.MustBeLockable(user)
	lu.PutAttemptCount(0)
	lu.PutLastAttempt(time.Now().UTC())

	return false, l.Authboss.Config.Storage.Server.Save(r.Context(), lu)
}

// AfterAuthFail adjusts the attempt number and time negatively
// and locks the user if they're beyond limits.
func (l *Lock) AfterAuthFail(w http.ResponseWriter, r *http.Request, handled bool) (bool, error) {
	user, err := l.Authboss.CurrentUser(r)
	if err != nil {
		return false, err
	}

	lu := authboss.MustBeLockable(user)
	last := lu.GetLastAttempt()
	attempts := lu.GetAttemptCount()
	attempts++

	nowLocked := false

	if time.Now().UTC().Sub(last) <= l.Modules.LockWindow {
		if attempts >= l.Modules.LockAfter {
			lu.PutLocked(time.Now().UTC().Add(l.Modules.LockDuration))
			nowLocked = true
		}

		lu.PutAttemptCount(attempts)
	} else {
		lu.PutAttemptCount(1)
	}
	lu.PutLastAttempt(time.Now().UTC())

	if err := l.Authboss.Config.Storage.Server.Save(r.Context(), lu); err != nil {
		return false, err
	}

	if !nowLocked {
		return false, nil
	}

	ro := authboss.RedirectOptions{
		Code:         http.StatusTemporaryRedirect,
		Failure:      "Your account has been locked, please contact the administrator.",
		RedirectPath: l.Authboss.Config.Paths.LockNotOK,
	}
	return true, l.Authboss.Config.Core.Redirector.Redirect(w, r, ro)
}

// Lock a user manually.
func (l *Lock) Lock(ctx context.Context, key string) error {
	user, err := l.Authboss.Config.Storage.Server.Load(ctx, key)
	if err != nil {
		return err
	}

	lu := authboss.MustBeLockable(user)
	lu.PutLocked(time.Now().UTC().Add(l.Authboss.Config.Modules.LockDuration))

	return l.Authboss.Config.Storage.Server.Save(ctx, lu)
}

// Unlock a user that was locked by this module.
func (l *Lock) Unlock(ctx context.Context, key string) error {
	user, err := l.Authboss.Config.Storage.Server.Load(ctx, key)
	if err != nil {
		return err
	}

	lu := authboss.MustBeLockable(user)

	// Set the last attempt to be -window*2 to avoid immediately
	// giving another login failure. Don't reset Locked to Zero time
	// because some databases may have trouble storing values before
	// unix_time(0): Jan 1st, 1970
	now := time.Now().UTC()
	lu.PutAttemptCount(0)
	lu.PutLastAttempt(now.Add(-l.Authboss.Config.Modules.LockWindow * 2))
	lu.PutLocked(now.Add(-l.Authboss.Config.Modules.LockDuration))

	return l.Authboss.Config.Storage.Server.Save(ctx, lu)
}

// Middleware ensures that a user is confirmed, or else it will intercept the request
// and send them to the confirm page, this will load the user if he's not been loaded
// yet from the session.
//
// Panics if the user was not able to be loaded in order to allow a panic handler to show
// a nice error page, also panics if it failed to redirect for whatever reason.
// TODO(aarondl): Document this middleware better
func Middleware(ab *authboss.Authboss) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := ab.LoadCurrentUserP(&r)

			lu := authboss.MustBeLockable(user)
			if IsLocked(lu) {
				next.ServeHTTP(w, r)
				return
			}

			logger := ab.RequestLogger(r)
			logger.Infof("user %s prevented from accessing %s: locked", user.GetPID(), r.URL.Path)
			ro := authboss.RedirectOptions{
				Code:         http.StatusTemporaryRedirect,
				Failure:      "Your account has been locked, please contact the administrator.",
				RedirectPath: ab.Config.Paths.LockNotOK,
			}
			ab.Config.Core.Redirector.Redirect(w, r, ro)
		})
	}
}

// IsLocked checks if a user is locked
func IsLocked(lu authboss.LockableUser) bool {
	return lu.GetLocked().After(time.Now().UTC())
}
