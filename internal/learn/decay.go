package learn

import (
	"math"
	"time"
)

// counter is an exponentially decaying counter: recent observations
// weigh more than old ones, and an entry nobody touches fades to zero
// on its own. This is what makes the model "adapt over time" without
// keeping any history: one float and one timestamp per counter.
//
// Decay is applied lazily (on read and on update), so an idle counter
// costs nothing.
type counter struct {
	val float64
	at  time.Time
}

// decayFactor returns exp(-ln2 * dt / halfLife): the weight left of an
// observation made dt ago.
func decayFactor(dt, halfLife time.Duration) float64 {
	if dt <= 0 {
		return 1
	}
	if halfLife <= 0 {
		return 0
	}
	return math.Exp(-math.Ln2 * float64(dt) / float64(halfLife))
}

// add decays the counter to now and adds w.
func (c *counter) add(now time.Time, halfLife time.Duration, w float64) {
	c.decay(now, halfLife)
	c.val += w
	c.at = now
}

// decay brings the counter forward to now.
func (c *counter) decay(now time.Time, halfLife time.Duration) {
	if c.at.IsZero() {
		c.at = now
		return
	}
	if d := now.Sub(c.at); d > 0 {
		c.val *= decayFactor(d, halfLife)
		c.at = now
	}
}

// value returns the counter as of now without mutating it.
func (c *counter) value(now time.Time, halfLife time.Duration) float64 {
	if c.at.IsZero() {
		return 0
	}
	return c.val * decayFactor(now.Sub(c.at), halfLife)
}

// ewma is an exponentially weighted moving average, used for the
// origin latency of a page (the preloader prioritizes slow pages).
type ewma struct {
	val   float64
	valid bool
}

const ewmaAlpha = 0.2

func (e *ewma) observe(v float64) {
	if !e.valid {
		e.val, e.valid = v, true
		return
	}
	e.val += ewmaAlpha * (v - e.val)
}
