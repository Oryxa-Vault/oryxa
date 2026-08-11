package api

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Rate limiting, in the one unit that costs money.
//
// A turn is an agent doing work: a model call at best, and with a command-line
// agent behind the connector, minutes of one. Before this, anyone who could
// reach a room could start them without bound — which was unbounded load when
// every agent was an HTTP endpoint and is unbounded spend now.
//
// What it charges, and what it does not
//
// A request is not the unit. One message can wake seven agents and cost seven
// turns, or wake nobody and cost nothing, and the wake ladder decides which
// after the request is already in. So the bucket is checked before the message
// is accepted and charged afterwards for the turns it actually started. A room
// that asks a lot of a lot of agents runs out sooner than one talking to itself,
// which is the behaviour worth having.
//
// One consequence to be plain about: not costing anything is not the same as
// getting through. A room that is already over budget refuses everything,
// acknowledgements included, because whether a message is free is not knowable
// until the ladder has run and running it means accepting the message. Free
// messages never drain a budget; they just cannot refill one. Predicting the
// wake set before admitting would fix that and would mean deciding who answers
// twice per message, from two places that could disagree — a worse thing to own
// than a throttled room that will not take "thanks" for a few seconds.
//
// What it is keyed by
//
// The room, and the server as a whole. Not the author: Oryxa accepts identity
// and never establishes it, so an author name is a string the caller picks, and
// a per-author limit would be bypassed by picking another one. That is the same
// reason read scoping is a capability rather than a list of names — a limit that
// looks like a limit and is not is worse than none, because it is budgeted for.
// A room key cannot be forged, because reaching a room needs its secret.

// admit checks both budgets and writes a 429 if either is spent. It reports
// whether the caller may proceed.
//
// The room is checked first so the error names the limit the caller can do
// something about: being told the server is busy when it is your own room that
// is over is a support ticket rather than a fix.
func (s *Server) admit(w http.ResponseWriter, room string) bool {
	if ok, wait := s.roomTurns.allow(room); !ok {
		tooMany(w, wait, "this room has started too many turns; it is limited so one room cannot spend the whole server's budget")
		return false
	}
	if ok, wait := s.allTurns.allow(""); !ok {
		tooMany(w, wait, "the server has started too many turns")
		return false
	}
	return true
}

// charge bills both budgets for the turns a message actually started.
func (s *Server) charge(room string, turns int) {
	s.roomTurns.charge(room, turns)
	s.allTurns.charge("", turns)
}

func tooMany(w http.ResponseWriter, retryAfter time.Duration, msg string) {
	w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error":       msg,
		"retry_after": int(retryAfter.Seconds()),
	})
}

// bucket is a token bucket that refills continuously.
type bucket struct {
	tokens float64
	last   time.Time
}

type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket

	perSecond float64
	burst     float64
}

// newLimiter builds a limiter allowing perMinute over a rolling minute. A
// perMinute of zero or less means unlimited, and returns nil — a nil *limiter
// allows everything, so the disabled path costs one nil check rather than a
// branch at every call site.
func newLimiter(perMinute int) *limiter {
	if perMinute <= 0 {
		return nil
	}
	return &limiter{
		buckets:   map[string]*bucket{},
		perSecond: float64(perMinute) / 60,
		// A full minute's worth of burst. Rooms are bursty by nature — several
		// people arrive at once and each says something — and a limiter that
		// smoothed that would be throttling the normal case to catch the
		// pathological one.
		burst: float64(perMinute),
	}
}

// allow reports whether key has anything left, without spending it.
//
// Separate from charge because the cost is not known until the work is done:
// this is the doorman, and charge is the bill.
func (l *limiter) allow(key string) (ok bool, retryAfter time.Duration) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.refill(key)
	if b.tokens >= 1 {
		return true, 0
	}
	// How long until one token exists. Rounded up, so a client obeying it
	// arrives after the token rather than exactly on it.
	wait := time.Duration((1-b.tokens)/l.perSecond*float64(time.Second)) + time.Second
	return false, wait.Round(time.Second)
}

// charge spends n tokens, going negative if the work cost more than was left.
//
// Debt is deliberate. The alternative — clamping at zero — lets one message that
// woke seven agents cost the same as one that woke nobody, and the whole point
// is that those are not the same.
func (l *limiter) charge(key string, n int) {
	if l == nil || n <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.refill(key)
	b.tokens -= float64(n)
}

// refill brings a bucket up to date. Caller holds the lock.
func (l *limiter) refill(key string) *bucket {
	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
		// Sweep here rather than on a timer: it runs only when a new key
		// appears, which is exactly when the map can grow, and it means no
		// goroutine outlives the server.
		l.sweep(now)
		return b
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * l.perSecond
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	return b
}

// sweep drops buckets that have been full for long enough to be indistinguishable
// from absent. A limiter that kept one per room forever would be its own denial
// of service — memory that only grows, keyed by something a caller can create.
func (l *limiter) sweep(now time.Time) {
	if len(l.buckets) < 1024 {
		return
	}
	// Full again after burst/perSecond seconds; anything idle past twice that
	// has nothing to remember.
	idle := time.Duration(2*l.burst/l.perSecond) * time.Second
	for k, b := range l.buckets {
		if now.Sub(b.last) > idle {
			delete(l.buckets, k)
		}
	}
}
