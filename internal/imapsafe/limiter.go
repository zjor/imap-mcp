package imapsafe

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Limiter caps the number of parallel IMAP sessions per username.
// Extra callers wait in Acquire until a slot is free.
type Limiter struct {
	perUser int
	mu      sync.Mutex
	users   map[string]*userSlots
}

type userSlots struct {
	ch   chan struct{}
	refs int // holders plus waiters; the entry is dropped at zero
}

// NewLimiter returns a limiter with perUser slots per username (minimum 1).
func NewLimiter(perUser int) *Limiter {
	if perUser < 1 {
		perUser = 1
	}
	return &Limiter{perUser: perUser, users: make(map[string]*userSlots)}
}

// Acquire waits for a free slot for user. Usernames are compared without
// case, so "Alice" and "alice" share slots. It returns a release func (safe
// to call more than once) and how long the caller waited. When ctx ends
// first, it returns ctx.Err().
func (l *Limiter) Acquire(ctx context.Context, user string) (release func(), waited time.Duration, err error) {
	user = strings.ToLower(user)
	l.mu.Lock()
	u := l.users[user]
	if u == nil {
		u = &userSlots{ch: make(chan struct{}, l.perUser)}
		l.users[user] = u
	}
	u.refs++
	l.mu.Unlock()

	select {
	case u.ch <- struct{}{}:
	default:
		start := time.Now()
		select {
		case u.ch <- struct{}{}:
			waited = time.Since(start)
		case <-ctx.Done():
			l.unref(user, u)
			return nil, time.Since(start), ctx.Err()
		}
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			<-u.ch
			l.unref(user, u)
		})
	}, waited, nil
}

func (l *Limiter) unref(user string, u *userSlots) {
	l.mu.Lock()
	defer l.mu.Unlock()
	u.refs--
	if u.refs == 0 {
		delete(l.users, user)
	}
}
