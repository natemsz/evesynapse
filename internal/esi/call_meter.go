package esi

import (
	"time"
)

// Call metering: every HTTP attempt against ESI lands in an
// hourly bucket, so the Sync page can show calls per active pilot
// per day over the trailing 24 hours. In-process only; a restart
// resets the window.

// noteCall records one HTTP call. It sits at the top of sendOnce,
// so retries count separately, like ESI counts them.
func (c *Client) noteCall() {
	c.noteCallAt(time.Now())
}

func (c *Client) noteCallAt(now time.Time) {
	hour := now.Unix() / 3600
	c.callsMu.Lock()
	defer c.callsMu.Unlock()
	if c.callsByHour == nil {
		c.callsByHour = map[int64]int64{}
	}
	c.callsByHour[hour]++
	c.callsTotal++
	for h := range c.callsByHour {
		if h < hour-23 {
			delete(c.callsByHour, h)
		}
	}
}

// CallsTotal is every HTTP call this client has made.
func (c *Client) CallsTotal() int64 {
	c.callsMu.Lock()
	defer c.callsMu.Unlock()
	return c.callsTotal
}

// CallsLastDay is the calls in the trailing 24 hours.
func (c *Client) CallsLastDay() int64 {
	return c.callsLastDayAt(time.Now())
}

func (c *Client) callsLastDayAt(now time.Time) int64 {
	cutoff := now.Unix()/3600 - 23
	c.callsMu.Lock()
	defer c.callsMu.Unlock()
	var n int64
	for h, count := range c.callsByHour {
		if h >= cutoff {
			n += count
		}
	}
	return n
}
